//! Project app, dev-server, port, worktree, and `.env` scanner.

mod apps;
mod devserver;
mod env;
mod pages;
mod worktree;

use std::collections::{BTreeMap, BTreeSet, HashSet};
use std::fs;
use std::ops::Not;
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::sync::LazyLock;

use regex::Regex;
use serde::Serialize;

use crate::compose;
use crate::fsutil;
use crate::store::{self, Domain, StoreError};

pub use apps::App;
pub use env::{EnvLine, Var, mask, values_for};
pub use pages::Page;
pub use worktree::worktree_root;

/// Error message reported when `find` detects no enclosing git repository or manifest+lockfile root.
pub const ERR_NO_PROJECT: &str =
    "not in a project: lodo scans the git root, or a folder with a lock file, it starts in";

/// Error message reported when naming an app whose port `lodo` cannot determine.
pub const ERR_UNKNOWN_PORT: &str = "lodo can't tell its port, so the name wouldn't stick: set PORT in its .env or a port in its dev script";

const NO_PORT_NOTE: &str = "lodo can't tell its port; set it with lodo set --port";

#[expect(clippy::expect_used)] // compile-time valid static regex
static NON_LABEL_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"[^a-z0-9-]+").expect("valid NON_LABEL_RE"));

#[expect(clippy::expect_used)] // compile-time valid static regex
static HOST_IP_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(&format!(
        r#"(?m)^[ \t]*(?:export[ \t]+)?{}[ \t]*=[ \t]*["']?([0-9.]+)"#,
        compose::ENV_VAR
    ))
    .expect("valid HOST_IP_RE")
});

#[expect(clippy::expect_used)] // compile-time valid static regex
static PORT_LINE_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?m)^[ \t]*(?:export[ \t]+)?PORT[ \t]*=[ \t]*["']?([0-9]{1,5})(?-u:\b)"#)
        .expect("valid PORT_LINE_RE")
});

/// Everything `find` discovered inside a project directory.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Project {
    pub root: String,
    pub name: String,
    pub compose: Vec<String>,
    pub apps: Vec<App>,
    pub pages: Vec<Page>,
    pub env_address: String,
    pub env: Vec<EnvLine>,
    pub bound: BTreeSet<u16>,
    pub repo: String,
}

/// Proposed additions, compose link, `.env` overrides, and notes produced by [`propose`].
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize)]
pub struct Proposal {
    pub root: String,
    pub name: String,
    pub address: String,
    pub listed: bool,
    #[serde(skip_serializing_if = "<&bool>::not")]
    pub claim: bool,
    pub add: Vec<Domain>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub compose: String,
    pub env: Vec<Var>,
    pub notes: Vec<String>,
    #[serde(skip_serializing_if = "<&bool>::not")]
    pub worktree: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub main: String,
    #[serde(skip)]
    pub dirs: BTreeMap<String, String>,
}

impl Proposal {
    /// Reports whether there is anything to persist to `domains.json` (`add`, `compose`, or `claim`).
    pub fn can_accept(&self) -> bool {
        !self.add.is_empty() || !self.compose.is_empty() || self.claim
    }
}

/// Scans the project enclosing `dir`, returning `None` outside a project.
pub fn find(dir: &Path) -> Option<Project> {
    let root = compose::project_root(dir)?;
    let root_str = root.to_string_lossy().into_owned();
    let files: Vec<PathBuf> = compose::find(&root).unwrap_or_default();
    let compose_strs: Vec<String> = files
        .iter()
        .map(|p| p.to_string_lossy().into_owned())
        .collect();
    let found_apps = apps::find_apps(&root);
    let env_lines = env::find_env(&root, &found_apps);
    let (pages, bound) = match files.first() {
        Some(first) => pages::read_compose(first),
        None => (Vec::new(), BTreeSet::new()),
    };

    Some(Project {
        name: name(&root),
        root: root_str,
        compose: compose_strs,
        apps: found_apps,
        pages,
        env_address: env_address(&root),
        env: env_lines,
        bound,
        repo: env::repo_root(&root),
    })
}

/// Converts a folder base name into a DNS label (e.g. `"Flowy"` -> `"flowy"`, `"my_app"` -> `"my-app"`).
pub fn label(folder: &str) -> String {
    NON_LABEL_RE
        .replace_all(&folder.to_ascii_lowercase(), "-")
        .trim_matches('-')
        .to_string()
}

/// Suggests a default `<label>.lodo` domain name from `root`'s folder name, or `""` if invalid.
pub fn name(root: impl AsRef<Path>) -> String {
    let base = root
        .as_ref()
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("");
    let candidate = format!("{}{}", label(base), store::SUFFIX);
    if store::validate_name(&candidate).is_err() {
        return String::new();
    }
    candidate
}

fn env_address(root: &Path) -> String {
    env_match(
        &HOST_IP_RE,
        store::is_own,
        &[root.join(".env"), root.join(".env.example")],
    )
}

pub(crate) fn env_port(dir: &Path) -> u16 {
    let matched = env_match(
        &PORT_LINE_RE,
        |s| s.parse::<u16>().is_ok_and(|p| p >= 1),
        &[dir.join(".env"), dir.join(".env.example")],
    );
    matched.parse::<u16>().unwrap_or(0)
}

fn env_match(re: &Regex, valid: impl Fn(&str) -> bool, files: &[PathBuf]) -> String {
    for file in files {
        let Ok(data) = fsutil::read_regular(file) else {
            continue;
        };
        let text = String::from_utf8_lossy(&data);
        if let Some(caps) = re.captures(&text)
            && let Some(m) = caps.get(1)
            && valid(m.as_str())
        {
            return m.as_str().to_string();
        }
    }
    String::new()
}

/// Reports whether `a` and `b` refer to the same file or directory (accounting for case-insensitive
/// APFS paths and symlinks such as `/tmp` -> `/private/tmp`).
pub fn same_folder(a: impl AsRef<Path>, b: impl AsRef<Path>) -> bool {
    let pa = a.as_ref();
    let pb = b.as_ref();
    if pa.as_os_str() == pb.as_os_str() {
        return true;
    }
    let (Ok(ma), Ok(mb)) = (fs::metadata(pa), fs::metadata(pb)) else {
        return false;
    };
    ma.dev() == mb.dev() && ma.ino() == mb.ino()
}

/// Reports whether listed domain `d` belongs to another live project folder than `root`.
pub fn taken(d: &Domain, root: impl AsRef<Path>, gone: impl Fn(&Path) -> bool) -> bool {
    taken_ref(d, root.as_ref(), &gone)
}

pub(crate) fn taken_ref(d: &Domain, root: &Path, gone: &dyn Fn(&Path) -> bool) -> bool {
    !d.root.is_empty() && !same_folder(&d.root, root) && !gone(Path::new(&d.root))
}

/// Formats the error message explaining why `name` cannot be claimed because it belongs to `folder`.
pub fn belongs_to(name: &str, folder: &str) -> String {
    format!("{name} belongs to {folder}: give this project another name")
}

pub(crate) fn listed_project(
    domains: &[Domain],
    root: &str,
    gone: &dyn Fn(&Path) -> bool,
) -> Option<Domain> {
    if let Some(d) = domains
        .iter()
        .find(|d| store::project(&d.name) == d.name && same_folder(&d.root, root))
    {
        return Some(d.clone());
    }
    let root_path = Path::new(root);
    if let Some(d) = domains.iter().find(|d| {
        let owner = store::find(domains, store::project(&d.name));
        links_inside(d, root) && owner.is_none_or(|o| !taken_ref(o, root_path, gone))
    }) {
        let proj_name = store::project(&d.name).to_string();
        if let Some(owner) = store::find(domains, &proj_name) {
            return Some(owner.clone());
        }
        return Some(Domain {
            name: proj_name,
            address: d.address.clone(),
            ..Domain::default()
        });
    }
    None
}

fn project_name(p: &Project, domains: &[Domain], gone: &dyn Fn(&Path) -> bool) -> String {
    if let Some(d) = listed_project(domains, &p.root, gone) {
        return d.name;
    }
    if let Some(main) = worktree::main_project(p, domains, gone)
        && let Some((slot_name, _)) = worktree::worktree_slot(p, domains, &main, gone)
    {
        return slot_name;
    }
    let from_env = env::env_project(p, domains);
    if !from_env.is_empty() {
        return from_env;
    }
    let root_path = Path::new(&p.root);
    if store::find(domains, &p.name).is_none_or(|d| !taken_ref(d, root_path, gone)) {
        return p.name.clone();
    }
    let (base, suffix) = store::cut_project(&p.name);
    let mut n: u64 = 2;
    loop {
        let candidate = format!("{base}-{n}{suffix}");
        if !domains.iter().any(|d| store::project(&d.name) == candidate) {
            return candidate;
        }
        n += 1;
    }
}

/// Compares `project` against `domains` and proposes missing domain entries, compose links,
/// `.env` overrides, and diagnostic notes.
///
/// Passing `""` for `chosen_name` lets `propose` pick the project name automatically.
pub fn propose(
    project: &Project,
    domains: &[Domain],
    chosen_name: &str,
    gone: impl Fn(&Path) -> bool,
) -> Proposal {
    propose_impl(project, domains, chosen_name, &gone)
}

fn propose_impl(
    orig: &Project,
    domains: &[Domain],
    chosen_name: &str,
    gone: &dyn Fn(&Path) -> bool,
) -> Proposal {
    let mut p = orig.clone();
    let mut prop = Proposal {
        root: p.root.clone(),
        ..Proposal::default()
    };

    if !chosen_name.is_empty() {
        if let Err(err) = store::validate_project(chosen_name) {
            prop.notes.push(format!("{chosen_name}: {err}"));
            return prop;
        }
        p.name = chosen_name.to_string();
    } else {
        p.name = project_name(&p, domains, gone);
    }

    prop.name.clone_from(&p.name);
    prop.worktree = p.is_worktree();
    let main = worktree::main_project(&p, domains, gone);
    if let Some(m) = &main {
        prop.main.clone_from(&m.name);
    }

    if p.name.is_empty() || (p.apps.is_empty() && p.compose.is_empty()) {
        return prop;
    }

    let root_path = Path::new(&p.root);
    if let Some(d) = store::find(domains, &p.name)
        && !same_folder(&d.root, &p.root)
    {
        if !taken_ref(d, root_path, gone) {
            prop.claim = true;
            if p.is_worktree() && !p.compose.is_empty() {
                prop.notes.push(format!(
                    "{} takes over a gone folder's name: lodo dev removes its old compose containers and data",
                    p.name
                ));
            }
        } else if !chosen_name.is_empty() {
            prop.notes.push(belongs_to(&p.name, &d.root));
            return prop;
        }
    }

    let (address, listed) = project_address(&p, domains);
    prop.address = address;
    prop.listed = listed;

    if let Some(m) = &main
        && !m.name.is_empty()
        && !prop.listed
        && let Some((slot_name, slot_addr)) = worktree::worktree_slot(&p, domains, m, gone)
        && slot_name == p.name
    {
        prop.address = slot_addr;
    }

    if prop.address.is_empty() {
        prop.notes.push(format!(
            "{}: free one, then scan again",
            StoreError::BlockFull
        ));
        return prop;
    }

    let (app_entries, app_notes) = entries(&p, domains, &prop.address);
    prop.notes.extend(app_notes);

    let mut list = domains.to_vec();
    for e in app_entries {
        if let Some(d) = store::find(&list, &e.name) {
            if let Some(app) = &e.app {
                prop.dirs.insert(e.name.clone(), app.dir.clone());
                if d.port == 0 {
                    if app.port > 0 {
                        prop.notes.push(format!(
                            "{}: listed without a port; its dev server takes {}: lodo set {} --port {}",
                            e.name, app.port, e.name, app.port
                        ));
                    } else {
                        prop.notes.push(format!(
                            "{}: listed without a port, and lodo can't tell its dev server's: lodo set {} --port <port>",
                            e.name, e.name
                        ));
                    }
                }
            }
            continue;
        }

        let app_port = e.app.as_ref().map_or(0, |a| a.port);
        let mut d = worktree::from_main(
            Domain {
                name: e.name.clone(),
                address: prop.address.clone(),
                port: app_port,
                enabled: true,
                ..Domain::default()
            },
            &p.name,
            main.as_ref(),
            domains,
        );
        if e.name == p.name {
            d.root.clone_from(&p.root);
            d.worktree = p.is_worktree();
        }
        if d.port > 0
            && let Some(other) = on_port(&list, &d.address, d.port)
        {
            prop.notes.push(format!(
                "{other} and {} both take port {}; lodo dev binds one app per port: give one of them another port",
                e.name, d.port
            ));
        }
        if let Err(err) = store::validate_port(d.port) {
            prop.notes.push(format!("{}: {err}", e.name));
            d.port = 0;
        }
        match store::add(&list, d.clone()) {
            Ok(next) => {
                list = next;
                if let Some(app) = &e.app {
                    prop.dirs.insert(d.name.clone(), app.dir.clone());
                }
                prop.add.push(d);
            }
            Err(err) => {
                prop.notes.push(format!("{}: {err}", e.name));
            }
        }
    }

    let mut used_compose = String::new();
    let owner_opt = store::find(&list, &p.name).cloned();
    if let Some(owner) = &owner_opt
        && !p.compose.is_empty()
    {
        used_compose.clone_from(&owner.compose);
        if !linked(owner, &p.compose) {
            if let Some(other) = list
                .iter()
                .find(|d| links_inside(d, &p.root) && linked(d, &p.compose))
            {
                used_compose.clone_from(&other.compose);
            } else {
                used_compose.clone_from(&p.compose[0]);
                prop.compose.clone_from(&used_compose);
            }
        }
        if same_folder(&used_compose, &p.compose[0]) {
            for pg in &p.pages {
                let d = worktree::from_main(
                    Domain {
                        name: format!("{}.{}", pg.label, p.name),
                        address: owner.address.clone(),
                        port: pg.port,
                        enabled: true,
                        ..Domain::default()
                    },
                    &p.name,
                    main.as_ref(),
                    domains,
                );
                if store::find(&list, &d.name).is_some()
                    || on_port(&list, &d.address, d.port).is_some()
                {
                    continue;
                }
                match store::add(&list, d.clone()) {
                    Ok(next) => {
                        list = next;
                        prop.add.push(d);
                    }
                    Err(err) => prop.notes.push(format!("{}: {err}", d.name)),
                }
            }
        }
    }

    if let Some(owner) = store::find(&list, &p.name) {
        let bound = (!p.compose.is_empty() && same_folder(&used_compose, &p.compose[0]))
            .then_some(&p.bound);
        let values = values_for(&list, owner);
        let siblings = env::sibling_names(&p, &list, &p.name);
        let (env_vars, env_notes) = env::env_vars(&p, &values, bound, &siblings);
        prop.env.extend(env_vars);
        prop.notes.extend(env_notes);
    }

    prop
}

struct Entry {
    dir: String,
    name: String,
    app: Option<App>,
}

fn entries(p: &Project, domains: &[Domain], address: &str) -> (Vec<Entry>, Vec<String>) {
    let mut notes = Vec::new();
    let mut out = vec![Entry {
        dir: ".".to_string(),
        name: p.name.clone(),
        app: None,
    }];
    let mut own = HashSet::new();
    let root_listed = store::find(domains, &p.name).is_some();

    for a in &p.apps {
        if a.dir != "." {
            let base = Path::new(&a.dir)
                .file_name()
                .and_then(|s| s.to_str())
                .unwrap_or("");
            own.insert(format!("{}.{}", label(base), p.name));
            continue;
        }
        out[0].app = Some(a.clone());
        own.insert(p.name.clone());
        if a.port == 0 && !root_listed {
            notes.push(format!("{}: {NO_PORT_NOTE}", p.name));
        }
    }

    for a in &p.apps {
        if a.dir == "." {
            continue;
        }
        let base = Path::new(&a.dir)
            .file_name()
            .and_then(|s| s.to_str())
            .unwrap_or("");
        let mut entry_name = format!("{}.{}", label(base), p.name);
        if store::find(domains, &entry_name).is_none()
            && a.port > 0
            && let Some(alias) = alias_of(domains, &p.name, address, a.port, &own)
        {
            own.insert(alias.clone());
            entry_name = alias;
        }
        if store::validate_name(&entry_name).is_err() {
            notes.push(format!("{}: its folder's name makes no valid name", a.dir));
            continue;
        }
        if store::find(domains, &entry_name).is_none() && a.port == 0 {
            notes.push(format!("{entry_name}: {NO_PORT_NOTE}"));
        }
        if let Some(prev) = out[1..].iter().find(|e| e.name == entry_name) {
            notes.push(format!("{}: {} has {entry_name}", a.dir, prev.dir));
            continue;
        }
        out.push(Entry {
            dir: a.dir.clone(),
            name: entry_name,
            app: Some(a.clone()),
        });
    }

    (out, notes)
}

fn alias_of(
    domains: &[Domain],
    project: &str,
    address: &str,
    port: u16,
    own: &HashSet<String>,
) -> Option<String> {
    domains
        .iter()
        .find(|d| {
            d.address == address
                && d.port == port
                && store::project(&d.name) == project
                && !own.contains(&d.name)
        })
        .map(|d| d.name.clone())
}

fn project_address(p: &Project, domains: &[Domain]) -> (String, bool) {
    if let Some(d) = store::find(domains, &p.name) {
        return (d.address.clone(), true);
    }
    if let Some(d) = domains.iter().find(|d| store::project(&d.name) == p.name) {
        return (d.address.clone(), true);
    }
    if !p.env_address.is_empty() && !domains.iter().any(|d| d.address == p.env_address) {
        return (p.env_address.clone(), false);
    }
    match store::next_free(domains) {
        Ok(free) => (free, false),
        Err(_) => (String::new(), false),
    }
}

fn on_port<'a>(domains: &'a [Domain], address: &str, port: u16) -> Option<&'a str> {
    domains
        .iter()
        .find(|d| d.address == address && d.port == port)
        .map(|d| d.name.as_str())
}

pub(crate) fn links_inside(d: &Domain, root: &str) -> bool {
    if root.is_empty() {
        return false;
    }
    let compose_path = Path::new(&d.compose);
    let root_path = Path::new(root);
    if compose_path.starts_with(root_path) && compose_path != root_path {
        return true;
    }
    if !store::has_compose(&d.compose) {
        return false;
    }
    let (Ok(rc), Ok(rr)) = (fs::canonicalize(compose_path), fs::canonicalize(root_path)) else {
        return false;
    };
    rc.starts_with(&rr) && rc != rr
}

fn linked(d: &Domain, found: &[String]) -> bool {
    if d.compose.is_empty() {
        return false;
    }
    if d.compose == store::NO_COMPOSE || found.contains(&d.compose) {
        return true;
    }
    !matches!(fs::metadata(&d.compose), Err(e) if e.kind() == std::io::ErrorKind::NotFound)
}

#[cfg(test)]
mod tests {
    use super::*;

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

    fn compose_file(image: &str, ports: &[&str]) -> String {
        format!(
            "services:\n  mail:\n    image: {image}\n    ports: [\"{}\"]\n",
            ports.join("\", \"")
        )
    }

    #[test]
    fn find_apps_env_address_pages_and_name() {
        type FilePair = (&'static str, &'static str);
        let app_cases: &[(&str, &[FilePair], Vec<App>)] = &[
            (
                "plain next app",
                &[(
                    "package.json",
                    r#"{"scripts": {"dev": "next dev --turbo"}}"#,
                )],
                vec![App {
                    dir: ".".into(),
                    port: 3000,
                }],
            ),
            (
                "dev.sh that reads the host from .env",
                &[
                    (
                        "package.json",
                        r#"{"scripts": {"dev": "./scripts/dev.sh"}}"#,
                    ),
                    (
                        "scripts/dev.sh",
                        "docker compose -f docker/compose.dev.yaml up -d\nexec next dev ${HOST_IP:+--hostname \"$HOST_IP\"}\n",
                    ),
                ],
                vec![App {
                    dir: ".".into(),
                    port: 3000,
                }],
            ),
            (
                "a dev script lodo doesn't know",
                &[(
                    "package.json",
                    r#"{"scripts": {"dev": "tsx watch src/server.ts"}}"#,
                )],
                vec![App {
                    dir: ".".into(),
                    port: 0,
                }],
            ),
            (
                "pnpm monorepo",
                &[
                    ("package.json", r#"{"scripts": {"dev": "turbo dev"}}"#),
                    (
                        "pnpm-workspace.yaml",
                        "packages:\n  - apps/*\n  - packages/*\n  - '!**/test/**'\n",
                    ),
                    (
                        "apps/storefront/package.json",
                        r#"{"scripts": {"dev": "next dev -p 3002"}}"#,
                    ),
                    (
                        "apps/admin/package.json",
                        r#"{"scripts": {"dev": "vite --host 0.0.0.0 --port 3001"}}"#,
                    ),
                    (
                        "apps/api/package.json",
                        r#"{"scripts": {"dev": "nest start --watch"}}"#,
                    ),
                    (
                        "apps/docs/package.json",
                        r#"{"scripts": {"build": "mintlify build"}}"#,
                    ),
                    (
                        "packages/ui/package.json",
                        r#"{"scripts": {"dev": "tsc --watch"}}"#,
                    ),
                    (
                        "packages/webapp/package.json",
                        r#"{"scripts": {"dev": "vite"}}"#,
                    ),
                ],
                vec![
                    App {
                        dir: "apps/admin".into(),
                        port: 3001,
                    },
                    App {
                        dir: "apps/api".into(),
                        port: 0,
                    },
                    App {
                        dir: "apps/storefront".into(),
                        port: 3002,
                    },
                    App {
                        dir: "packages/webapp".into(),
                        port: 5173,
                    },
                ],
            ),
            (
                "package.json workspaces object",
                &[
                    (
                        "package.json",
                        r#"{"workspaces": {"packages": ["apps/*"]}, "scripts": {"dev": "turbo dev"}}"#,
                    ),
                    (
                        "apps/client/package.json",
                        r#"{"scripts": {"dev": "vite"}}"#,
                    ),
                    (
                        "apps/client/vite.config.ts",
                        "export default { server: { port: 5175 } }",
                    ),
                ],
                vec![App {
                    dir: "apps/client".into(),
                    port: 5175,
                }],
            ),
            (
                "single app with a workspace file for pnpm settings",
                &[
                    ("package.json", r#"{"scripts": {"dev": "next dev"}}"#),
                    (
                        "pnpm-workspace.yaml",
                        "onlyBuiltDependencies:\n  - esbuild\n",
                    ),
                ],
                vec![App {
                    dir: ".".into(),
                    port: 3000,
                }],
            ),
            ("no package.json", &[("go.mod", "module x\n")], vec![]),
        ];

        for (case_name, files, want) in app_cases {
            let tmp = tempfile::tempdir().unwrap();
            let root = make_project(tmp.path(), "shop", files);
            let p = find(&root).unwrap();
            assert_eq!(&p.apps, want, "case {case_name}");
        }

        // EnvAddress cases.
        let addr_cases: &[(&str, &[FilePair], &str)] = &[
            (
                ".env",
                &[(".env", "SECRET=x\nDOCKER_HOST_IP=127.0.1.2\n")],
                "127.0.1.2",
            ),
            (
                ".env first",
                &[
                    (".env", "DOCKER_HOST_IP=127.0.1.4\n"),
                    (".env.example", "DOCKER_HOST_IP=127.0.1.2\n"),
                ],
                "127.0.1.4",
            ),
            (
                ".env.example",
                &[(".env.example", "export DOCKER_HOST_IP=\"127.0.1.7\"\n")],
                "127.0.1.7",
            ),
            (
                "not an own address",
                &[(".env", "DOCKER_HOST_IP=127.0.0.1\n")],
                "",
            ),
            (
                "commented out",
                &[(".env", "# DOCKER_HOST_IP=127.0.1.2\n")],
                "",
            ),
        ];
        for (case_name, files, want) in addr_cases {
            let tmp = tempfile::tempdir().unwrap();
            let root = make_project(tmp.path(), "shop", files);
            assert_eq!(find(&root).unwrap().env_address, *want, "case {case_name}");
        }

        // Pages cases.
        let mail_bound = compose_file(
            "axllent/mailpit:latest",
            &[
                "${DOCKER_HOST_IP:-127.0.0.1}:1025:1025",
                "${DOCKER_HOST_IP:-127.0.0.1}:8025:8025",
            ],
        );
        let mail_ghcr = compose_file("ghcr.io/axllent/mailpit:v1.21@sha256:0abc", &["18025:8025"]);
        let mail_smtp = compose_file("axllent/mailpit", &["1025:1025"]);
        let mailhog = compose_file("mailhog/mailhog", &["8025:8025"]);

        let page_cases: &[(&str, (&str, &str), Vec<Page>)] = &[
            (
                "Mailpit bound",
                ("docker/compose.dev.yaml", &mail_bound),
                vec![Page {
                    label: "mail".into(),
                    port: 8025,
                }],
            ),
            (
                "ghcr digest",
                ("compose.yaml", &mail_ghcr),
                vec![Page {
                    label: "mail".into(),
                    port: 18025,
                }],
            ),
            ("SMTP only", ("compose.yaml", &mail_smtp), vec![]),
            ("unknown image", ("compose.yaml", &mailhog), vec![]),
            ("invalid YAML", ("compose.yaml", "services: ["), vec![]),
        ];
        for (case_name, (rel, content), want) in page_cases {
            let tmp = tempfile::tempdir().unwrap();
            let root = make_project(tmp.path(), "shop", &[(rel, content)]);
            assert_eq!(&find(&root).unwrap().pages, want, "case {case_name}");
        }

        // Outside a project.
        let empty = tempfile::tempdir().unwrap();
        assert!(find(empty.path()).is_none());

        // Name helper.
        assert_eq!(name("/Users/sang/Projects/flowy"), "flowy.lodo");
        assert_eq!(name("/Users/sang/Projects/My_App"), "my-app.lodo");
        assert_eq!(name("/Users/sang/Projects/opscom.web"), "opscom-web.lodo");
        assert_eq!(name("/Users/sang/Projects/__"), "");
    }

    #[test]
    fn propose_comprehensive_cases() {
        let compose = "/p/shop/docker/compose.dev.yaml";
        let prod = "/p/shop/docker/compose.prod.yaml";
        let live_tmp = tempfile::tempdir().unwrap();
        let live_dir = live_tmp.path().join("other");
        let elsewhere = live_dir.join("compose.yaml");
        write_file(&elsewhere, "services: {}\n");
        let live_str = live_dir.to_string_lossy().into_owned();
        let elsewhere_str = elsewhere.to_string_lossy().into_owned();

        let inbox = vec![Page {
            label: "mail".into(),
            port: 8025,
        }];
        let monorepo = vec![
            App {
                dir: "apps/storefront".into(),
                port: 3000,
            },
            App {
                dir: "apps/admin".into(),
                port: 3000,
            },
            App {
                dir: "apps/api".into(),
                port: 0,
            },
            App {
                dir: "apps/email".into(),
                port: 3003,
            },
        ];

        // 1. New app with compose file.
        let got = propose(
            &Project {
                root: "/p/shop".into(),
                name: "shop.test".into(),
                compose: vec![compose.into()],
                apps: vec![App {
                    dir: ".".into(),
                    port: 3000,
                }],
                ..Project::default()
            },
            &[Domain {
                name: "blog.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            }],
            "",
            is_gone,
        );
        assert_eq!(got.address, "127.0.1.2");
        assert_eq!(got.compose, compose);
        assert_eq!(
            got.add,
            vec![Domain {
                name: "shop.test".into(),
                address: "127.0.1.2".into(),
                port: 3000,
                enabled: true,
                root: "/p/shop".into(),
                ..Domain::default()
            }]
        );

        // 2. .env address free vs taken.
        let env_addr_p = Project {
            root: "/p/shop".into(),
            name: "shop.test".into(),
            apps: vec![App {
                dir: ".".into(),
                port: 3000,
            }],
            env_address: "127.0.1.7".into(),
            ..Project::default()
        };
        assert_eq!(propose(&env_addr_p, &[], "", is_gone).address, "127.0.1.7");
        assert_eq!(
            propose(
                &env_addr_p,
                &[Domain {
                    name: "blog.test".into(),
                    address: "127.0.1.7".into(),
                    enabled: true,
                    ..Domain::default()
                }],
                "",
                is_gone
            )
            .address,
            "127.0.1.1"
        );

        // 3. Monorepo port collision and unknown port notes.
        let mono_got = propose(
            &Project {
                root: "/p/shop".into(),
                name: "shop.test".into(),
                apps: monorepo,
                ..Project::default()
            },
            &[],
            "",
            is_gone,
        );
        assert_eq!(mono_got.add.len(), 5);
        assert_eq!(
            mono_got.notes,
            vec![
                "api.shop.test: lodo can't tell its port; set it with lodo set --port",
                "storefront.shop.test and admin.shop.test both take port 3000; lodo dev binds one app per port: give one of them another port",
            ]
        );

        // 4. Mailpit inbox added, skipped when listed or port occupied or compose=none or prod compose.
        let mail_p = Project {
            root: "/p/shop".into(),
            name: "shop.test".into(),
            compose: vec![compose.into(), prod.into()],
            pages: inbox,
            ..Project::default()
        };
        let mail_got = propose(&mail_p, &[], "", is_gone);
        assert_eq!(mail_got.add.len(), 2);
        assert_eq!(mail_got.add[1].name, "mail.shop.test");
        assert_eq!(mail_got.add[1].port, 8025);

        let no_compose_got = propose(
            &mail_p,
            &[Domain {
                name: "shop.test".into(),
                address: "127.0.1.4".into(),
                enabled: true,
                compose: store::NO_COMPOSE.into(),
                ..Domain::default()
            }],
            "",
            is_gone,
        );
        assert!(no_compose_got.add.is_empty());
        assert!(no_compose_got.claim);

        let prod_got = propose(
            &mail_p,
            &[Domain {
                name: "shop.test".into(),
                address: "127.0.1.4".into(),
                enabled: true,
                compose: prod.into(),
                ..Domain::default()
            }],
            "",
            is_gone,
        );
        assert!(prod_got.add.is_empty());

        // 5. Link to elsewhere that still exists stays; moved link inside project re-offers best compose.
        let elsewhere_got = propose(
            &mail_p,
            &[Domain {
                name: "shop.test".into(),
                address: "127.0.1.4".into(),
                enabled: true,
                compose: elsewhere_str,
                ..Domain::default()
            }],
            "",
            is_gone,
        );
        assert!(elsewhere_got.compose.is_empty());

        let moved_got = propose(
            &Project {
                root: "/p/shop".into(),
                name: "shop.test".into(),
                compose: vec![compose.into()],
                ..Project::default()
            },
            &[Domain {
                name: "shop.test".into(),
                address: "127.0.1.4".into(),
                enabled: true,
                root: "/p/shop".into(),
                compose: "/p/shop/compose.yaml".into(),
                ..Domain::default()
            }],
            "",
            is_gone,
        );
        assert_eq!(moved_got.compose, compose);

        // 6. Chosen name belonging to live folder gives belongs_to note; invalid 3-label name gives validation note.
        let taken_got = propose(
            &Project {
                root: "/p/shop-v2".into(),
                name: "shop-v2.test".into(),
                apps: vec![App {
                    dir: ".".into(),
                    port: 3000,
                }],
                ..Project::default()
            },
            &[Domain {
                name: "shop.test".into(),
                address: "127.0.1.1".into(),
                port: 3000,
                enabled: true,
                root: live_str.clone(),
                ..Domain::default()
            }],
            "shop.test",
            is_gone,
        );
        assert_eq!(
            taken_got.notes,
            vec![format!(
                "shop.test belongs to {live_str}: give this project another name"
            )]
        );

        let bad_chosen = propose(
            &Project {
                root: "/p/shop".into(),
                name: "shop.test".into(),
                apps: vec![App {
                    dir: ".".into(),
                    port: 3000,
                }],
                ..Project::default()
            },
            &[],
            "web.shop.test",
            is_gone,
        );
        assert_eq!(bad_chosen.name, "");
        assert_eq!(
            bad_chosen.notes,
            vec!["web.shop.test: a project's name has two labels, like flowy.lodo"]
        );

        // 7. Alias preservation & Dirs tracking.
        let alias_got = propose(
            &Project {
                root: "/p/shop".into(),
                name: "shop.test".into(),
                apps: vec![App {
                    dir: "apps/web".into(),
                    port: 3001,
                }],
                ..Project::default()
            },
            &[Domain {
                name: "shop.test".into(),
                address: "127.0.1.4".into(),
                port: 3001,
                enabled: true,
                ..Domain::default()
            }],
            "",
            is_gone,
        );
        assert!(alias_got.add.is_empty());
        assert_eq!(
            alias_got.dirs,
            BTreeMap::from([("shop.test".to_string(), "apps/web".to_string())])
        );

        // 8. Proposal JSON serialization omits false/empty optional fields and skips dirs.
        let json_val = serde_json::to_value(&got).unwrap();
        let obj = json_val.as_object().unwrap();
        assert!(obj.contains_key("root"));
        assert!(obj.contains_key("name"));
        assert!(obj.contains_key("address"));
        assert!(obj.contains_key("listed"));
        assert!(obj.contains_key("add"));
        assert!(obj.contains_key("compose"));
        assert!(obj.contains_key("env"));
        assert!(obj.contains_key("notes"));
        assert!(!obj.contains_key("claim"));
        assert!(!obj.contains_key("worktree"));
        assert!(!obj.contains_key("main"));
        assert!(!obj.contains_key("dirs"));
    }

    #[test]
    fn propose_same_folder_name_and_case_insensitive_path() {
        let tmp = tempfile::tempdir().unwrap();
        let first = make_project(tmp.path(), "flowy", &[]);
        let second = Project {
            root: "/p/work/flowy".into(),
            name: "flowy.test".into(),
            apps: vec![
                App {
                    dir: "apps/web".into(),
                    port: 3000,
                },
                App {
                    dir: "apps/api".into(),
                    port: 4000,
                },
            ],
            ..Project::default()
        };

        let base = |root: &str| {
            vec![
                Domain {
                    name: "crm.test".into(),
                    address: "127.0.1.1".into(),
                    enabled: true,
                    ..Domain::default()
                },
                Domain {
                    name: "flowy.test".into(),
                    address: "127.0.1.2".into(),
                    enabled: true,
                    root: root.to_string(),
                    ..Domain::default()
                },
                Domain {
                    name: "web.flowy.test".into(),
                    address: "127.0.1.2".into(),
                    port: 3000,
                    enabled: true,
                    ..Domain::default()
                },
            ]
        };

        let first_str = first.to_string_lossy().into_owned();
        let got_live = propose(&second, &base(&first_str), "", is_gone);
        assert_eq!(got_live.name, "flowy-2.test");
        assert_eq!(got_live.address, "127.0.1.3");
        assert_eq!(got_live.add.len(), 3);

        let mut with_extra = base(&first_str);
        with_extra.push(Domain {
            name: "api.flowy-2.test".into(),
            address: "127.0.1.3".into(),
            port: 4000,
            enabled: true,
            ..Domain::default()
        });
        let got_3 = propose(&second, &with_extra, "", is_gone);
        assert_eq!(got_3.name, "flowy-3.test");
        assert_eq!(got_3.address, "127.0.1.4");

        let got_gone = propose(
            &second,
            &base(&tmp.path().join("moved").to_string_lossy()),
            "",
            is_gone,
        );
        assert_eq!(got_gone.name, "flowy.test");
        assert!(got_gone.claim);
        assert_eq!(got_gone.add.len(), 1);
        assert_eq!(got_gone.add[0].name, "api.flowy.test");

        // Case-insensitive path test (macOS APFS).
        let case_root = make_project(
            tmp.path(),
            "caseflow",
            &[
                ("package.json", r#"{"scripts": {"dev": "next dev"}}"#),
                (
                    "compose.yaml",
                    "services:\n  db:\n    image: postgres\n    ports: [\"${DOCKER_HOST_IP:-127.0.0.1}:5432:5432\"]\n",
                ),
                (".env", "DATABASE_URL=postgres://localhost:5432/flowy\n"),
            ],
        );
        let upper = PathBuf::from(case_root.to_string_lossy().to_uppercase());
        if fs::metadata(&upper).is_ok() {
            let p = find(&upper).unwrap();
            let domains = vec![Domain {
                name: "flowy.test".into(),
                address: "127.0.1.2".into(),
                port: 3000,
                enabled: true,
                root: case_root.to_string_lossy().into_owned(),
                compose: case_root
                    .join("compose.yaml")
                    .to_string_lossy()
                    .into_owned(),
                ..Domain::default()
            }];
            let got_upper = propose(&p, &domains, "", is_gone);
            assert_eq!(got_upper.name, "flowy.test");
            assert!(got_upper.listed);
            assert!(!got_upper.claim);
            assert!(got_upper.add.is_empty());
            assert!(got_upper.compose.is_empty());
            assert_eq!(got_upper.env.len(), 1);
            assert_eq!(got_upper.env[0].value, "postgres://flowy.test:5432/flowy");
        }
    }
}
