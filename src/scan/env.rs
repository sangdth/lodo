//! `.env` parsing, localhost/sibling host substitution, and user-info password masking.

use std::cmp::Reverse;
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::fmt;
use std::fs;
use std::path::Path;
use std::sync::LazyLock;

use regex::Regex;
use serde::Serialize;

use crate::compose::{self, clean_path};
use crate::fsutil;
use crate::store::{self, Domain};

use super::apps::App;
use super::{Project, links_inside, same_folder};

#[expect(clippy::expect_used)] // compile-time valid static regex
static KEY_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_.]*)[ \t]*=").expect("valid KEY_RE")
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static TEST_HOST_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(&format!(
        r"(?i)[a-z0-9]([a-z0-9-]{{0,61}}[a-z0-9])?(?:\.[a-z0-9]([a-z0-9-]{{0,61}}[a-z0-9])?)*\.(?:{}|test)",
        store::TLD
    ))
    .expect("valid TEST_HOST_RE")
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static USER_INFO_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)(?-u:\b)([a-z][a-z0-9+.-]*://)[^\t\n\x0c\r /]*@").expect("valid USER_INFO_RE")
});

/// One `KEY=value` line read from an app's `.env` file.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct EnvLine {
    /// Slash-separated path relative to the project root (e.g. `"apps/web/.env"` or `".env"`).
    pub file: String,
    /// 1-based line number in `file`.
    pub line: usize,
    pub key: String,
    /// Raw line text without trailing `\r` or `\n`.
    pub text: String,
}

#[derive(Clone, Default)]
struct SecretValue(String);

impl fmt::Debug for SecretValue {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("[REDACTED]")
    }
}

/// Environment variable `lodo dev` and `lodo exec` export in place of an app's `.env` value.
///
/// `value` is always masked (`***@`) for terminal and JSON display; the raw secret stays inside
/// a private `#[serde(skip)]` field whose `Debug` impl prints `"[REDACTED]"` and is exposed only
/// via [`Var::env_pair`].
#[derive(Debug, Clone, Serialize)]
pub struct Var {
    pub file: String,
    pub line: usize,
    pub key: String,
    pub value: String,
    #[serde(skip)]
    raw_value: SecretValue,
}

impl Var {
    /// Creates a `Var` from an unmasked value, storing `mask(&raw)` in `value` and keeping the
    /// raw value private for [`Var::env_pair`].
    pub fn new(
        file: impl Into<String>,
        line: usize,
        key: impl Into<String>,
        raw_value: impl Into<String>,
    ) -> Self {
        let raw = raw_value.into();
        Self {
            file: file.into(),
            line,
            key: key.into(),
            value: mask(&raw),
            raw_value: SecretValue(raw),
        }
    }

    /// Returns `KEY=value` with the real unmasked value for child process environments.
    pub fn env_pair(&self) -> String {
        format!("{}={}", self.key, self.raw_value.0)
    }
}

impl PartialEq for Var {
    fn eq(&self, other: &Self) -> bool {
        self.file == other.file
            && self.line == other.line
            && self.key == other.key
            && self.value == other.value
    }
}

impl Eq for Var {}

/// Reads the `KEY=value` lines of each app's `.env` (keeping only the last occurrence of duplicate
/// keys within one file, and reading the root `.env` only when the root itself is an app).
pub(crate) fn find_env(root: &Path, apps: &[App]) -> Vec<EnvLine> {
    let mut lines = Vec::new();
    for app in apps {
        let rel_file = if app.dir == "." {
            ".env".to_string()
        } else {
            format!("{}/.env", app.dir)
        };
        let Ok(data) = fsutil::read_regular(&root.join(&rel_file)) else {
            continue;
        };
        let content = String::from_utf8_lossy(&data);
        let mut file_lines = Vec::new();
        let mut last: HashMap<String, usize> = HashMap::new();

        for (i, raw) in content.split('\n').enumerate() {
            let text = raw.strip_suffix('\r').unwrap_or(raw);
            if let Some(caps) = KEY_RE.captures(text)
                && let Some(k) = caps.get(1)
            {
                let key = k.as_str().to_string();
                last.insert(key.clone(), file_lines.len());
                file_lines.push(EnvLine {
                    file: rel_file.clone(),
                    line: i + 1,
                    key,
                    text: text.to_string(),
                });
            }
        }

        for (idx, env_line) in file_lines.into_iter().enumerate() {
            if last.get(&env_line.key) == Some(&idx) {
                lines.push(env_line);
            }
        }
    }
    lines
}

/// Extracts the scalar value of a `.env` line as `dotenv` and Node read it.
/// Returns `None` for multiline quoted strings or values containing `\` or `$`.
pub(crate) fn env_value(text: &str) -> Option<String> {
    let m = KEY_RE.find(text)?;
    let v = text[m.end()..].trim_start_matches([' ', '\t']);
    let extracted = if let Some(&quote) = v.as_bytes().first()
        && matches!(quote, b'"' | b'\'' | b'`')
    {
        let rest = &v[1..];
        let end = rest.as_bytes().iter().position(|&b| b == quote)?;
        &rest[..end]
    } else {
        let without_comment = v.split('#').next().unwrap_or("");
        without_comment.trim()
    };

    if extracted.contains(['\\', '$']) {
        return None;
    }
    Some(extracted.to_string())
}

pub(crate) fn env_vars(
    p: &Project,
    v: &compose::Values,
    bound: Option<&BTreeSet<u16>>,
    siblings: &BTreeMap<String, String>,
) -> (Vec<Var>, Vec<String>) {
    struct EntryValue {
        line: EnvLine,
        out: String,
        known: bool,
        changed: bool,
    }

    let mut notes = Vec::new();
    let mut keys: Vec<String> = Vec::new();
    let mut by_key: HashMap<String, Vec<EntryValue>> = HashMap::new();

    for l in &p.env {
        let got = if let Some(val) = env_value(&l.text) {
            let (mut out, missing) = rename_hosts(&val, siblings);
            for missing_host in missing {
                notes.push(format!(
                    "{}:{} {}: {} has no name for {missing_host}, so lodo dev leaves it",
                    l.file, l.line, l.key, v.domain
                ));
            }
            let (mapped, _) = compose::map_urls(&out, |u| {
                let (url, named) = v.point(u);
                let parsed_port = u.port.parse::<u16>().ok();
                let is_bound =
                    parsed_port.is_some_and(|port| bound.is_some_and(|b| b.contains(&port)));
                if named || is_bound {
                    return Some(url);
                }
                if u.is_web()
                    && let Some(port) = parsed_port
                    && !v.names.contains_key(&port)
                {
                    notes.push(format!(
                        "{}:{} {}: no name of {} has port {port}",
                        l.file, l.line, l.key, v.domain
                    ));
                }
                None
            });
            out = mapped;
            let changed = out != val;
            EntryValue {
                line: l.clone(),
                out,
                known: true,
                changed,
            }
        } else {
            if is_local_or_sibling(&l.text, siblings) {
                notes.push(format!(
                    "{}:{} {}: lodo can't read a value over more lines or with a \\ or a $, so lodo dev leaves it",
                    l.file, l.line, l.key
                ));
            }
            EntryValue {
                line: l.clone(),
                out: String::new(),
                known: false,
                changed: false,
            }
        };

        if !by_key.contains_key(&l.key) {
            keys.push(l.key.clone());
        }
        by_key.entry(l.key.clone()).or_default().push(got);
    }

    let mut vars = Vec::new();
    for key in keys {
        let Some(values) = by_key.get(&key) else {
            continue;
        };
        let Some(first_changed) = values.iter().find(|x| x.changed) else {
            continue;
        };
        if values
            .iter()
            .any(|x| !x.known || x.out != first_changed.out)
        {
            let mut files: Vec<&str> = values.iter().map(|x| x.line.file.as_str()).collect();
            files.dedup();
            notes.push(format!(
                "{key}: {} set it to different values, and lodo dev gives every app one environment, so it leaves {key} as each file has it",
                files.join(" and ")
            ));
            continue;
        }
        vars.push(Var::new(
            &first_changed.line.file,
            first_changed.line.line,
            &key,
            &first_changed.out,
        ));
    }

    (vars, notes)
}

fn is_local_or_sibling(text: &str, siblings: &BTreeMap<String, String>) -> bool {
    let low = text.to_ascii_lowercase();
    if low.contains("localhost") || low.contains("127.0.0.1") {
        return true;
    }
    siblings.keys().any(|name| low.contains(name))
}

fn is_host_byte(c: u8) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, b'.' | b'-' | b'_')
}

/// Replaces whole hostnames matching keys in `names` (case-insensitively) with their mapped value.
/// Hosts mapped to `""` stay unchanged and are returned in `missing`.
pub(crate) fn rename_hosts(s: &str, names: &BTreeMap<String, String>) -> (String, Vec<String>) {
    if names.is_empty() {
        return (s.to_string(), Vec::new());
    }
    let mut keys: Vec<String> = names.keys().map(|name| regex::escape(name)).collect();
    keys.sort_by(|a, b| {
        Reverse(a.len())
            .cmp(&Reverse(b.len()))
            .then_with(|| a.cmp(b))
    });

    let Ok(re) = Regex::new(&format!("(?i){}", keys.join("|"))) else {
        return (s.to_string(), Vec::new());
    };

    let bytes = s.as_bytes();
    let mut out = String::new();
    let mut missing = Vec::new();
    let mut last = 0;

    for m in re.find_iter(s) {
        if (m.start() > 0 && is_host_byte(bytes[m.start() - 1]))
            || bytes.get(m.end()).is_some_and(|&b| is_host_byte(b))
        {
            continue;
        }
        let host = m.as_str().to_ascii_lowercase();
        let target = names.get(&host).map_or("", String::as_str);
        if target.is_empty() {
            missing.push(host);
            continue;
        }
        out.push_str(&s[last..m.start()]);
        out.push_str(target);
        last = m.end();
    }
    out.push_str(&s[last..]);
    (out, missing)
}

/// Maps every domain belonging to another checkout of `p`'s git repository — and every `.lodo`/`.test`
/// host in `p.env` belonging to an unlisted project — to the corresponding domain under `project`
/// (or `""` if `list` lacks that subdomain).
pub(crate) fn sibling_names(
    p: &Project,
    list: &[Domain],
    project: &str,
) -> BTreeMap<String, String> {
    let mut names = BTreeMap::new();

    if !p.repo.is_empty() {
        for owner in list {
            let sibling = &owner.name;
            if store::project(sibling) != sibling
                || sibling == project
                || same_folder(&owner.root, &p.root)
                || !same_repo(list, owner, &p.repo)
            {
                continue;
            }
            for d in list {
                if store::project(&d.name) != sibling {
                    continue;
                }
                let prefix = d.name.strip_suffix(sibling.as_str()).unwrap_or("");
                let candidate = format!("{prefix}{project}");
                let target = if store::find(list, &candidate).is_some() {
                    candidate
                } else {
                    String::new()
                };
                names.insert(d.name.to_ascii_lowercase(), target);
            }
        }
    }

    let unlisted = env_project(p, list);
    if !unlisted.is_empty() && unlisted != project {
        for host in env_hosts(&p.env) {
            if store::project(&host) != unlisted {
                continue;
            }
            let prefix = host.strip_suffix(unlisted.as_str()).unwrap_or("");
            let candidate = format!("{prefix}{project}");
            let target = if store::find(list, &candidate).is_some() {
                candidate
            } else {
                String::new()
            };
            names.insert(host, target);
        }
    }

    names
}

fn same_repo(list: &[Domain], owner: &Domain, repo: &str) -> bool {
    if !owner.root.is_empty() {
        return same_folder(repo_root(Path::new(&owner.root)), repo);
    }
    list.iter()
        .any(|d| store::project(&d.name) == owner.name && links_inside(d, repo))
}

fn env_hosts(lines: &[EnvLine]) -> Vec<String> {
    let mut hosts = Vec::new();
    for l in lines {
        let Some(val) = env_value(&l.text) else {
            continue;
        };
        let bytes = val.as_bytes();
        for m in TEST_HOST_RE.find_iter(&val) {
            if (m.start() > 0 && is_host_byte(bytes[m.start() - 1]))
                || bytes.get(m.end()).is_some_and(|&b| is_host_byte(b))
            {
                continue;
            }
            let host = m.as_str().to_ascii_lowercase();
            if store::validate_name(&host).is_ok() && !hosts.contains(&host) {
                hosts.push(host);
            }
        }
    }
    hosts
}

pub(crate) fn env_project(p: &Project, domains: &[Domain]) -> String {
    for host in env_hosts(&p.env) {
        let proj = store::project(&host);
        if !domains.iter().any(|d| store::project(&d.name) == proj) {
            return proj.to_string();
        }
    }
    String::new()
}

/// Returns the main checkout directory of the git repository rooted at `dir`: `dir` itself for a
/// normal `.git` folder, `<main>` when `.git` is a worktree file pointing into `<main>/.git/worktrees/<name>`,
/// or `""` outside a git repository or in a submodule.
pub(crate) fn repo_root(dir: &Path) -> String {
    if dir.as_os_str().is_empty() {
        return String::new();
    }
    let git_path = dir.join(".git");
    let Ok(info) = fs::symlink_metadata(&git_path) else {
        return String::new();
    };
    if info.file_type().is_dir() {
        return dir.to_string_lossy().into_owned();
    }
    let Ok(data) = fsutil::read_regular(&git_path) else {
        return String::new();
    };
    let text = String::from_utf8_lossy(&data);
    let Some(gitdir) = text.trim().strip_prefix("gitdir:") else {
        return String::new();
    };
    let gitdir = gitdir.trim();
    let resolved = if Path::new(gitdir).is_absolute() {
        clean_path(Path::new(gitdir))
    } else {
        clean_path(&dir.join(gitdir))
    };
    let slash_path = resolved.to_string_lossy();
    let Some((main, _)) = slash_path.split_once("/.git/worktrees/") else {
        return String::new();
    };
    main.to_string()
}

/// Builds the [`compose::Values`] routing table for `owner`'s project from `domains`.
pub fn values_for(domains: &[Domain], owner: &Domain) -> compose::Values {
    let mut names = BTreeMap::new();
    let mut secure = BTreeSet::new();
    let owner_proj = store::project(&owner.name);

    for d in domains {
        if d.has_site() && d.address == owner.address && store::project(&d.name) == owner_proj {
            names.insert(d.port, d.name.clone());
            if d.https {
                secure.insert(d.port);
            } else {
                secure.remove(&d.port);
            }
        }
    }
    compose::Values {
        domain: owner.name.clone(),
        names,
        secure,
    }
}

/// Masks every URL's `userinfo@` segment in `s` as `***@` so credentials in `.env` values never
/// appear on screen or in serialized JSON.
pub fn mask(s: &str) -> String {
    USER_INFO_RE.replace_all(s, "${1}***@").into_owned()
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::path::PathBuf;

    use super::*;
    use crate::scan::{Page, find, propose, worktree_root};

    fn is_gone(path: &Path) -> bool {
        matches!(fs::metadata(path), Err(e) if e.kind() == std::io::ErrorKind::NotFound)
    }

    fn write_file(path: &Path, content: &str) {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(path, content).unwrap();
    }

    fn make_project(base: &Path, folder: &str, files: &[(&str, &str)]) -> PathBuf {
        let root = base.join(folder);
        write_file(&root.join(".git/HEAD"), "ref: refs/heads/master\n");
        for (rel, content) in files {
            write_file(&root.join(rel), content);
        }
        root
    }

    #[test]
    fn var_security_invariant_redacts_json_and_debug() {
        let v = Var::new(
            "apps/web/.env",
            3,
            "DATABASE_URL",
            "postgres://admin:s3cr3tP@ss@shop.lodo:5432/db",
        );
        assert_eq!(v.value, "postgres://***@shop.lodo:5432/db");
        assert_eq!(
            v.env_pair(),
            "DATABASE_URL=postgres://admin:s3cr3tP@ss@shop.lodo:5432/db"
        );

        let json = serde_json::to_string(&v).unwrap();
        assert!(!json.contains("s3cr3tP@ss"), "JSON leaked secret: {json}");
        assert!(
            !json.contains("raw_value"),
            "JSON included raw_value: {json}"
        );
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(
            parsed,
            serde_json::json!({
                "file": "apps/web/.env",
                "line": 3,
                "key": "DATABASE_URL",
                "value": "postgres://***@shop.lodo:5432/db"
            })
        );

        let dbg = format!("{v:?}");
        assert!(!dbg.contains("s3cr3tP@ss"), "Debug leaked secret: {dbg}");
        assert!(
            dbg.contains("[REDACTED]"),
            "Debug missing [REDACTED]: {dbg}"
        );
    }

    #[test]
    fn find_env_monorepo_duplicates_and_root_app() {
        let tmp = tempfile::tempdir().unwrap();
        let root = make_project(
            tmp.path(),
            "shop",
            &[
                ("package.json", r#"{"scripts": {"dev": "turbo dev"}}"#),
                ("pnpm-workspace.yaml", "packages: [apps/*]\n"),
                (".env", "DATABASE_URL=postgres://localhost:5434/shop\n"),
                (
                    "apps/web/package.json",
                    r#"{"scripts": {"dev": "next dev -p 3303"}}"#,
                ),
                ("apps/web/.env.example", "API_URL=http://localhost:3000\n"),
                (
                    "apps/server/package.json",
                    r#"{"scripts": {"dev": "nest start"}}"#,
                ),
                ("apps/server/.env", "PORT=3300\n"),
                (
                    "apps/web/.env",
                    "DATABASE_URL=\"postgres://u:p@LOCALHOST:5434/shop\"\n# AUTH_URL=http://localhost:3303\nexport AUTH_URL=http://127.0.0.1:3303 # auth\r\nCERT=\"line one\nhttp://localhost:9999\"",
                ),
            ],
        );
        let p = find(&root).unwrap();
        assert_eq!(
            p.env,
            vec![
                EnvLine {
                    file: "apps/server/.env".into(),
                    line: 1,
                    key: "PORT".into(),
                    text: "PORT=3300".into(),
                },
                EnvLine {
                    file: "apps/web/.env".into(),
                    line: 1,
                    key: "DATABASE_URL".into(),
                    text: "DATABASE_URL=\"postgres://u:p@LOCALHOST:5434/shop\"".into(),
                },
                EnvLine {
                    file: "apps/web/.env".into(),
                    line: 3,
                    key: "AUTH_URL".into(),
                    text: "export AUTH_URL=http://127.0.0.1:3303 # auth".into(),
                },
                EnvLine {
                    file: "apps/web/.env".into(),
                    line: 4,
                    key: "CERT".into(),
                    text: "CERT=\"line one".into(),
                },
            ]
        );
        assert_eq!(p.repo, root.to_string_lossy());

        let dup_tmp = tempfile::tempdir().unwrap();
        let dup_root = make_project(
            dup_tmp.path(),
            "api",
            &[
                ("package.json", r#"{"scripts": {"dev": "next dev"}}"#),
                (
                    ".env",
                    "API_URL=http://localhost:9999\nAPI_URL=http://localhost:3000\n",
                ),
            ],
        );
        let dup_p = find(&dup_root).unwrap();
        assert_eq!(
            dup_p.env,
            vec![EnvLine {
                file: ".env".into(),
                line: 2,
                key: "API_URL".into(),
                text: "API_URL=http://localhost:3000".into(),
            }]
        );
    }

    #[test]
    fn env_value_parsing() {
        let cases: &[(&str, Option<&str>)] = &[
            ("A=http://localhost:3000", Some("http://localhost:3000")),
            ("export A = x # a comment", Some("x")),
            (
                r#"A="http://localhost:3000" # api"#,
                Some("http://localhost:3000"),
            ),
            ("A='x # not a comment'", Some("x # not a comment")),
            ("A=`x`", Some("x")),
            ("A=", Some("")),
            (r#"A="line one"#, None),
            (r#"A="a\nb""#, None),
            ("A=postgres://u:pa$s@x", None),
            ("# A=x", None),
        ];
        for (text, want) in cases {
            assert_eq!(env_value(text).as_deref(), *want, "text {text:?}");
        }
    }

    #[test]
    fn rename_hosts_and_mask() {
        let names = BTreeMap::from([
            ("hugger.test".to_string(), "hugger9.test".to_string()),
            (
                "mail.hugger.test".to_string(),
                "mail.hugger9.test".to_string(),
            ),
            ("api.hugger.test".to_string(), String::new()),
        ]);
        let host_cases: &[(&str, &str, &[&str])] = &[
            (
                "postgresql://u:p@hugger.test:5432/db",
                "postgresql://u:p@hugger9.test:5432/db",
                &[],
            ),
            ("hugger.test", "hugger9.test", &[]),
            (
                "http://MAIL.hugger.test,http://hugger.test/x",
                "http://mail.hugger9.test,http://hugger9.test/x",
                &[],
            ),
            ("noreply@hugger.test", "noreply@hugger9.test", &[]),
            (
                "http://x-hugger.test http://hugger.test.example.com",
                "http://x-hugger.test http://hugger.test.example.com",
                &[],
            ),
            (
                "http://api.hugger.test",
                "http://api.hugger.test",
                &["api.hugger.test"],
            ),
        ];
        for (input, want, want_missing) in host_cases {
            let (got, missing) = rename_hosts(input, &names);
            let expected_missing: Vec<String> =
                want_missing.iter().map(|s| (*s).to_string()).collect();
            assert_eq!(got, *want);
            assert_eq!(missing, expected_missing);
        }

        let mask_cases: &[(&str, &str)] = &[
            (
                "DATABASE_URL=postgres://u:p@ss@localhost:5434/db",
                "DATABASE_URL=postgres://***@localhost:5434/db",
            ),
            (
                "REDIS_URL=redis://:pw@127.0.0.1:6379",
                "REDIS_URL=redis://***@127.0.0.1:6379",
            ),
            (
                "REPO=https://TOKEN@github.com/o/r",
                "REPO=https://***@github.com/o/r",
            ),
            ("PROFILE=https://x.com/@user", "PROFILE=https://x.com/@user"),
            (
                "API_URL=http://localhost:3000",
                "API_URL=http://localhost:3000",
            ),
            (
                "A=http://localhost:3000,postgres://admin:p#ss@db.example.com:5432",
                "A=http://localhost:3000,postgres://***@db.example.com:5432",
            ),
            ("A=postgres://u:p?w@db", "A=postgres://***@db"),
            (r"A=postgres://u:p\w@db", "A=postgres://***@db"),
        ];
        for (input, want) in mask_cases {
            assert_eq!(mask(input), *want);
        }
    }

    #[test]
    fn repo_root_and_worktree_root() {
        let tmp = tempfile::tempdir().unwrap();
        let main = make_project(tmp.path(), "shop", &[]);
        let wt = tmp.path().join("wt");
        write_file(
            &wt.join(".git"),
            &format!("gitdir: {}\n", main.join(".git/worktrees/wt").display()),
        );
        let relative = main.join("nested");
        write_file(&relative.join(".git"), "gitdir: ../.git/worktrees/nested\n");
        let submodule = tmp.path().join("sub");
        write_file(
            &submodule.join(".git"),
            &format!("gitdir: {}\n", main.join(".git/modules/sub").display()),
        );
        let plain = tmp.path().join("plain");
        fs::create_dir_all(&plain).unwrap();

        let main_str = main.to_string_lossy().into_owned();
        assert_eq!(repo_root(&main), main_str);
        assert_eq!(repo_root(&wt), main_str);
        assert_eq!(repo_root(&relative), main_str);
        assert_eq!(repo_root(&submodule), "");
        assert_eq!(repo_root(&plain), "");
        assert_eq!(repo_root(Path::new("")), "");

        let subdir = wt.join("apps/web");
        write_file(&subdir.join("package.json"), "{}");
        assert_eq!(worktree_root(&wt), Some(wt.clone()));
        assert_eq!(worktree_root(&subdir), Some(wt));
        assert_eq!(worktree_root(&main), None);
        assert_eq!(worktree_root(&submodule), None);
        assert_eq!(worktree_root(&plain), None);
    }

    #[test]
    fn propose_env_and_key_conflicts() {
        let compose = "/p/shop/compose.yaml";
        let prod = "/p/shop/compose.prod.yaml";
        let base_domains = vec![
            Domain {
                name: "shop.test".into(),
                address: "127.0.1.4".into(),
                enabled: true,
                root: "/p/shop".into(),
                compose: compose.into(),
                ..Domain::default()
            },
            Domain {
                name: "web.shop.test".into(),
                address: "127.0.1.4".into(),
                port: 3000,
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "api.shop.test".into(),
                address: "127.0.1.4".into(),
                port: 4000,
                enabled: true,
                https: true,
                ..Domain::default()
            },
            Domain {
                name: "off.shop.test".into(),
                address: "127.0.1.4".into(),
                port: 5000,
                ..Domain::default()
            },
        ];

        let bound_5434 = BTreeSet::from([5434]);
        let empty_bound = BTreeSet::new();
        struct EnvCase<'a> {
            name: &'static str,
            text: &'static str,
            bound: &'a BTreeSet<u16>,
            linked: &'static str,
            want_val: Option<&'static str>,
            want_notes: &'static [&'static str],
        }
        let cases = [
            EnvCase {
                name: "a named port",
                text: "API_URL=http://localhost:3000/v1",
                bound: &empty_bound,
                linked: "",
                want_val: Some("http://web.shop.test/v1"),
                want_notes: &[],
            },
            EnvCase {
                name: "HTTPS",
                text: "API_URL=http://localhost:4000",
                bound: &empty_bound,
                linked: "",
                want_val: Some("https://api.shop.test"),
                want_notes: &[],
            },
            EnvCase {
                name: "ws",
                text: "WS_URL=ws://127.0.0.1:4000/socket",
                bound: &empty_bound,
                linked: "",
                want_val: Some("wss://api.shop.test/socket"),
                want_notes: &[],
            },
            EnvCase {
                name: "user info",
                text: "AUTH=http://u:hunter2@localhost:3000",
                bound: &empty_bound,
                linked: "",
                want_val: Some("http://***@web.shop.test"),
                want_notes: &[],
            },
            EnvCase {
                name: "a web port no name has",
                text: "NEXT_PUBLIC_URL=http://u:hunter2@localhost:3300",
                bound: &empty_bound,
                linked: "",
                want_val: None,
                want_notes: &[
                    "apps/web/.env:1 NEXT_PUBLIC_URL: no name of shop.test has port 3300",
                ],
            },
            EnvCase {
                name: "https without a port",
                text: "X=https://localhost",
                bound: &empty_bound,
                linked: "",
                want_val: None,
                want_notes: &[],
            },
            EnvCase {
                name: "https on a named port",
                text: "X=https://localhost:3000",
                bound: &empty_bound,
                linked: "",
                want_val: None,
                want_notes: &[],
            },
            EnvCase {
                name: "a bound port",
                text: "DATABASE_URL=postgres://u:hunter2@localhost:5434/x",
                bound: &bound_5434,
                linked: "",
                want_val: Some("postgres://***@shop.test:5434/x"),
                want_notes: &[],
            },
            EnvCase {
                name: "a port compose doesn't bind",
                text: "DATABASE_URL=postgres://localhost:5432/x",
                bound: &bound_5434,
                linked: "",
                want_val: None,
                want_notes: &[],
            },
            EnvCase {
                name: "another compose file runs",
                text: "DATABASE_URL=postgres://localhost:5434/x",
                bound: &bound_5434,
                linked: prod,
                want_val: None,
                want_notes: &[],
            },
            EnvCase {
                name: "a disabled name",
                text: "X=http://localhost:5000",
                bound: &empty_bound,
                linked: "",
                want_val: None,
                want_notes: &["apps/web/.env:1 X: no name of shop.test has port 5000"],
            },
            EnvCase {
                name: "quotes, export and a comment",
                text: r#"export API_URL="http://localhost:3000" # api"#,
                bound: &empty_bound,
                linked: "",
                want_val: Some("http://web.shop.test"),
                want_notes: &[],
            },
            EnvCase {
                name: "two URLs",
                text: "ORIGINS=http://localhost:3000,http://localhost:4000",
                bound: &empty_bound,
                linked: "",
                want_val: Some("http://web.shop.test,https://api.shop.test"),
                want_notes: &[],
            },
            EnvCase {
                name: "nothing local",
                text: "SECRET=abc",
                bound: &empty_bound,
                linked: "",
                want_val: None,
                want_notes: &[],
            },
            EnvCase {
                name: "a value lodo can't read",
                text: "DATABASE_URL=postgres://u:pa$s@localhost:5434/x",
                bound: &bound_5434,
                linked: "",
                want_val: None,
                want_notes: &[
                    "apps/web/.env:1 DATABASE_URL: lodo can't read a value over more lines or with a \\ or a $, so lodo dev leaves it",
                ],
            },
        ];

        for c in &cases {
            let mut list = base_domains.clone();
            if !c.linked.is_empty() {
                list[0].compose = c.linked.to_string();
            }
            let key = KEY_RE.captures(c.text).unwrap()[1].to_string();
            let p = Project {
                root: "/p/shop".into(),
                name: "shop.test".into(),
                compose: vec![compose.into(), prod.into()],
                apps: vec![App {
                    dir: "apps/web".into(),
                    port: 3000,
                }],
                env: vec![EnvLine {
                    file: "apps/web/.env".into(),
                    line: 1,
                    key: key.clone(),
                    text: c.text.into(),
                }],
                bound: c.bound.clone(),
                ..Project::default()
            };

            let got = propose(&p, &list, "", is_gone);
            if let Some(wv) = c.want_val {
                assert_eq!(got.env.len(), 1, "case {}", c.name);
                assert_eq!(got.env[0].value, wv, "case {}", c.name);
                assert_eq!(
                    got.env[0].env_pair(),
                    format!("{key}={}", wv.replace("***@", "u:hunter2@")),
                    "case {} env_pair",
                    c.name
                );
            } else {
                assert!(got.env.is_empty(), "case {}: {:?}", c.name, got.env);
            }
            let expected_notes: Vec<String> =
                c.want_notes.iter().map(|s| (*s).to_string()).collect();
            assert_eq!(got.notes, expected_notes, "case {} notes", c.name);
            assert!(!got.can_accept(), "case {} can_accept", c.name);
            for s in got.notes.iter().chain(got.env.iter().map(|v| &v.value)) {
                assert!(!s.contains("hunter2"), "leaked password in {s}");
            }
        }

        // Duplicate keys across two app .env files.
        let key_domains = vec![
            Domain {
                name: "shop.test".into(),
                address: "127.0.1.4".into(),
                enabled: true,
                root: "/p/shop".into(),
                ..Domain::default()
            },
            Domain {
                name: "web.shop.test".into(),
                address: "127.0.1.4".into(),
                port: 3000,
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "admin.shop.test".into(),
                address: "127.0.1.4".into(),
                port: 3001,
                enabled: true,
                ..Domain::default()
            },
        ];

        let make_lines = |a: &str, b: &str| {
            vec![
                EnvLine {
                    file: "apps/web/.env".into(),
                    line: 1,
                    key: "API".into(),
                    text: a.into(),
                },
                EnvLine {
                    file: "apps/admin/.env".into(),
                    line: 1,
                    key: "API".into(),
                    text: b.into(),
                },
            ]
        };

        let same_p = Project {
            root: "/p/shop".into(),
            name: "shop.test".into(),
            apps: vec![
                App {
                    dir: "apps/web".into(),
                    port: 3000,
                },
                App {
                    dir: "apps/admin".into(),
                    port: 3001,
                },
            ],
            env: make_lines("API=http://localhost:3000", "API=http://localhost:3000"),
            ..Project::default()
        };
        let same_got = propose(&same_p, &key_domains, "", is_gone);
        assert_eq!(same_got.env[0].env_pair(), "API=http://web.shop.test");

        for (a, b) in [
            ("API=http://localhost:3000", "API=http://localhost:3001"),
            ("API=http://localhost:3000", "API=https://shop.example.com"),
        ] {
            let diff_p = Project {
                env: make_lines(a, b),
                ..same_p.clone()
            };
            let diff_got = propose(&diff_p, &key_domains, "", is_gone);
            assert!(diff_got.env.is_empty());
            assert_eq!(
                diff_got.notes,
                vec![
                    "API: apps/web/.env and apps/admin/.env set it to different values, and lodo dev gives every app one environment, so it leaves API as each file has it"
                ]
            );
        }
    }

    #[test]
    fn propose_worktree_env_and_slots() {
        let tmp = tempfile::tempdir().unwrap();
        let main = make_project(tmp.path(), "hugger", &[]);
        let worktree = tmp.path().join("t3-71ba597b");
        write_file(
            &worktree.join(".git"),
            &format!(
                "gitdir: {}\n",
                main.join(".git/worktrees/t3-71ba597b").display()
            ),
        );
        let other = make_project(tmp.path(), "crm", &[]);

        let domains = vec![
            Domain {
                name: "hugger.test".into(),
                address: "127.0.1.1".into(),
                port: 3000,
                enabled: true,
                root: main.to_string_lossy().into_owned(),
                ..Domain::default()
            },
            Domain {
                name: "mail.hugger.test".into(),
                address: "127.0.1.1".into(),
                port: 8025,
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "api.hugger.test".into(),
                address: "127.0.1.1".into(),
                port: 4000,
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "hugger9.test".into(),
                address: "127.0.1.9".into(),
                port: 3000,
                enabled: true,
                root: worktree.to_string_lossy().into_owned(),
                ..Domain::default()
            },
            Domain {
                name: "mail.hugger9.test".into(),
                address: "127.0.1.9".into(),
                port: 8025,
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "crm.test".into(),
                address: "127.0.1.3".into(),
                port: 3000,
                enabled: true,
                root: other.to_string_lossy().into_owned(),
                ..Domain::default()
            },
        ];

        let raw_lines = [
            "DATABASE_URL=postgresql://postgres:hunter2@hugger.test:5432/postgres",
            "SMTP_HOST=hugger.test",
            "MAILPIT_URL=http://mail.hugger.test",
            "BETTER_AUTH_URL=http://hugger.test",
            "API_URL=http://api.hugger.test",
            "CRM_URL=http://crm.test",
        ];
        let env: Vec<EnvLine> = raw_lines
            .iter()
            .enumerate()
            .map(|(i, s)| EnvLine {
                file: ".env".into(),
                line: i + 1,
                key: KEY_RE.captures(s).unwrap()[1].to_string(),
                text: (*s).into(),
            })
            .collect();

        let p = Project {
            root: worktree.to_string_lossy().into_owned(),
            name: "hugger9.test".into(),
            apps: vec![App {
                dir: ".".into(),
                port: 3000,
            }],
            env: env.clone(),
            repo: repo_root(&worktree),
            ..Project::default()
        };

        let want_pairs = vec![
            "DATABASE_URL=postgresql://postgres:hunter2@hugger9.test:5432/postgres",
            "SMTP_HOST=hugger9.test",
            "MAILPIT_URL=http://mail.hugger9.test",
            "BETTER_AUTH_URL=http://hugger9.test",
        ];

        for slice in [&domains[..], &domains[3..]] {
            let got = propose(&p, slice, "", is_gone);
            let pairs: Vec<String> = got.env.iter().map(Var::env_pair).collect();
            assert_eq!(pairs, want_pairs);
            assert_eq!(
                got.notes,
                vec![
                    ".env:5 API_URL: hugger9.test has no name for api.hugger.test, so lodo dev leaves it"
                ]
            );
        }

        let main_p = Project {
            root: main.to_string_lossy().into_owned(),
            name: "hugger.test".into(),
            apps: vec![App {
                dir: ".".into(),
                port: 3000,
            }],
            env,
            repo: repo_root(&main),
            ..Project::default()
        };
        let main_got = propose(&main_p, &domains, "", is_gone);
        assert!(main_got.env.is_empty());
        assert!(main_got.notes.is_empty());

        // Worktree slot allocation tests.
        let wt_new = tmp.path().join("t3-new");
        write_file(
            &wt_new.join(".git"),
            &format!("gitdir: {}\n", main.join(".git/worktrees/t3-new").display()),
        );
        let live_wt = make_project(tmp.path(), "t3-live", &[]);
        let gone_wt = tmp.path().join("t3-deleted");

        let slot_base = vec![
            Domain {
                name: "hugger.test".into(),
                address: "127.0.1.1".into(),
                port: 3000,
                enabled: true,
                https: true,
                root: main.to_string_lossy().into_owned(),
                ..Domain::default()
            },
            Domain {
                name: "mail.hugger.test".into(),
                address: "127.0.1.1".into(),
                port: 8025,
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "crm.test".into(),
                address: "127.0.1.2".into(),
                port: 3000,
                enabled: true,
                ..Domain::default()
            },
        ];

        let wt_p = Project {
            root: wt_new.to_string_lossy().into_owned(),
            name: "t3-new.test".into(),
            apps: vec![App {
                dir: ".".into(),
                port: 3000,
            }],
            repo: repo_root(&wt_new),
            compose: vec![wt_new.join("compose.yaml").to_string_lossy().into_owned()],
            pages: vec![Page {
                label: "mail".into(),
                port: 8025,
            }],
            ..Project::default()
        };

        // 1. Deleted worktree's slot is reclaimed with takeover note.
        let mut with_deleted = slot_base.clone();
        with_deleted.push(Domain {
            name: "hugger3.test".into(),
            address: "127.0.1.3".into(),
            port: 3000,
            enabled: true,
            https: true,
            root: gone_wt.to_string_lossy().into_owned(),
            ..Domain::default()
        });
        with_deleted.push(Domain {
            name: "mail.hugger3.test".into(),
            address: "127.0.1.3".into(),
            port: 8025,
            enabled: true,
            ..Domain::default()
        });
        let got_del = propose(&wt_p, &with_deleted, "", is_gone);
        assert_eq!(
            (
                &got_del.name,
                &got_del.address,
                got_del.claim,
                got_del.worktree,
                &got_del.main
            ),
            (
                &"hugger3.test".to_string(),
                &"127.0.1.3".to_string(),
                true,
                true,
                &"hugger.test".to_string()
            )
        );
        assert!(got_del.add.is_empty());
        assert!(
            got_del
                .notes
                .iter()
                .any(|n| n.contains("takes over a gone folder's name"))
        );

        // 2. Live worktree's slot is skipped for the next free slot.
        let mut with_live = slot_base.clone();
        with_live.push(Domain {
            name: "hugger3.test".into(),
            address: "127.0.1.3".into(),
            port: 3000,
            enabled: true,
            https: true,
            root: live_wt.to_string_lossy().into_owned(),
            ..Domain::default()
        });
        let got_live = propose(&wt_p, &with_live, "", is_gone);
        assert_eq!(got_live.name, "hugger4.test");
        assert_eq!(got_live.address, "127.0.1.4");
        assert_eq!(
            got_live.add,
            vec![
                Domain {
                    name: "hugger4.test".into(),
                    address: "127.0.1.4".into(),
                    port: 3000,
                    enabled: true,
                    https: true,
                    root: wt_new.to_string_lossy().into_owned(),
                    ..Domain::default()
                },
                Domain {
                    name: "mail.hugger4.test".into(),
                    address: "127.0.1.4".into(),
                    port: 8025,
                    enabled: true,
                    ..Domain::default()
                },
            ]
        );

        // 3. Unlisted .lodo main in .env with public URLs ignored.
        let unlisted_wt = Project {
            root: wt_new.to_string_lossy().into_owned(),
            name: "t3-new.test".into(),
            apps: vec![App {
                dir: ".".into(),
                port: 3000,
            }],
            repo: repo_root(&wt_new),
            env_address: "127.0.1.1".into(),
            env: vec![
                EnvLine {
                    file: ".env".into(),
                    line: 1,
                    key: "STRIPE_URL".into(),
                    text: "STRIPE_URL=https://api.stripe.com".into(),
                },
                EnvLine {
                    file: ".env".into(),
                    line: 2,
                    key: "DATABASE_URL".into(),
                    text: "DATABASE_URL=postgresql://postgres:hunter2@hugger.lodo:5432/postgres"
                        .into(),
                },
            ],
            ..Project::default()
        };
        let got_unlisted = propose(&unlisted_wt, &slot_base[2..], "", is_gone);
        assert_eq!(got_unlisted.name, "hugger3.lodo");
        assert_eq!(got_unlisted.address, "127.0.1.3");
        assert_eq!(got_unlisted.main, "hugger.lodo");
        assert_eq!(
            got_unlisted.env[0].env_pair(),
            "DATABASE_URL=postgresql://postgres:hunter2@hugger3.lodo:5432/postgres"
        );
    }
}
