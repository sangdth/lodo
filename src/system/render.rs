//! Template rendering for `apply-resolvers.sh`, `io.lodo.loopback.plist`,
//! `/etc/sudoers.d/lodo`, and `lodo` blocks in Homebrew's configs.

use crate::dnsmasq;
use crate::paths::Paths;
use crate::store;

/// First line of every file and block `lodo` writes outside its own directory.
pub const MARKER: &str = "# lodo";

/// `launchd` label of the job that adds the own block to `lo0`.
pub const LOOPBACK_LABEL: &str = "io.lodo.loopback";

/// Maximum bytes (`1 MiB`) the resolver script reads on standard input.
pub const MAX_SCRIPT_INPUT: usize = 1 << 20;

/// Global options block `lodo` places at the top of Homebrew's `Caddyfile`.
pub const CADDY_GLOBAL_BLOCK: &str = "# lodo\n{\n\tadmin off\n}\n";

const SCRIPT_TMPL: &str = include_str!("templates/apply-resolvers.sh.tmpl");
const LOOPBACK_TMPL: &str = include_str!("templates/io.lodo.loopback.plist.tmpl");
const SUDOERS_TMPL: &str = include_str!("templates/sudoers.tmpl");

/// Lines of `/etc/resolver/<name>` after the marker.
pub fn resolver_lines() -> Vec<String> {
    vec![
        format!("nameserver {}", dnsmasq::LISTEN_ADDRESS),
        format!("port {}", dnsmasq::PORT),
    ]
}

/// Complete content of every `/etc/resolver/<name>` file `lodo` writes.
pub fn resolver_file() -> String {
    format!("{MARKER}\n{}\n", resolver_lines().join("\n"))
}

/// Renders the root resolver script for `p`.
pub fn script(p: &Paths) -> String {
    let max_input = MAX_SCRIPT_INPUT.to_string();
    let read_input = (MAX_SCRIPT_INPUT + 1).to_string();
    let max_len = store::MAX_NAME_LEN.to_string();
    let resolver_dir = shell_quote(&p.resolver_dir.to_string_lossy());
    let pattern = shell_quote(store::NAME_PATTERN);
    let marker = shell_quote(MARKER);
    let mut lines_part = String::new();
    for line in resolver_lines() {
        lines_part.push(' ');
        lines_part.push_str(&shell_quote(&line));
    }

    render_template(
        SCRIPT_TMPL,
        &[
            ("{{.MaxInput}}", &max_input),
            ("{{.ReadInput}}", &read_input),
            ("{{.MaxLen}}", &max_len),
            ("{{sh .ResolverDir}}", &resolver_dir),
            ("{{sh .Pattern}}", &pattern),
            ("{{sh .Marker}}", &marker),
            ("{{range .Lines}} {{sh .}}{{end}}", &lines_part),
        ],
    )
}

/// Renders the `launchd` job that adds the own block to `lo0` at boot.
pub fn loopback_plist() -> String {
    let first = store::OWN_FIRST.to_string();
    let last = store::OWN_LAST.to_string();
    render_template(
        LOOPBACK_TMPL,
        &[
            ("{{.Label}}", LOOPBACK_LABEL),
            ("{{.First}}", &first),
            ("{{.Last}}", &last),
            ("{{.Prefix}}", store::OWN_PREFIX),
        ],
    )
}

/// Renders the `/etc/sudoers.d/lodo` rule letting `p.user` run `p.script` as
/// root without a password, and nothing else.
pub fn sudoers(p: &Paths) -> Result<String, String> {
    if !valid_sudoers_user(&p.user) {
        return Err(format!(
            "user name {:?} has characters lodo won't write into a sudoers rule",
            p.user
        ));
    }
    let escaped_script = sudoers_escape(&p.script.to_string_lossy());
    Ok(render_template(
        SUDOERS_TMPL,
        &[("{{.User}}", &p.user), ("{{.Script}}", &escaped_script)],
    ))
}

/// `lodo`'s block in Homebrew's `dnsmasq.conf`.
pub fn conf_block(p: &Paths) -> String {
    format!(
        "{MARKER}\nconf-file={}\nlisten-address={}\nport={}\nbind-interfaces\n",
        p.dnsmasq_conf.display(),
        dnsmasq::LISTEN_ADDRESS,
        dnsmasq::PORT,
    )
}

/// `lodo`'s import block at the end of Homebrew's `Caddyfile`.
pub fn caddy_block(p: &Paths) -> String {
    format!(
        "{MARKER}\nimport {}\n",
        caddy_quote(&p.caddyfile.to_string_lossy())
    )
}

pub(super) fn caddy_quote(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for ch in s.chars() {
        match ch {
            '\\' => out.push_str("\\\\"),
            '"' => out.push_str("\\\""),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            _ => out.push(ch),
        }
    }
    out.push('"');
    out
}

fn render_template(mut tmpl: &str, vars: &[(&str, &str)]) -> String {
    let mut out = String::with_capacity(tmpl.len() + 128);
    while let Some(open) = tmpl.find("{{") {
        out.push_str(&tmpl[..open]);
        let rest = &tmpl[open..];
        if let Some((token, val)) = vars.iter().find(|(k, _)| rest.starts_with(k)) {
            out.push_str(val);
            tmpl = &rest[token.len()..];
        } else {
            out.push_str("{{");
            tmpl = &rest[2..];
        }
    }
    out.push_str(tmpl);
    out
}

fn valid_sudoers_user(u: &str) -> bool {
    let mut bytes = u.bytes();
    let Some(first) = bytes.next() else {
        return false;
    };
    let is_start = first.is_ascii_lowercase() || first.is_ascii_digit() || first == b'_';
    is_start
        && bytes.all(|b| {
            b.is_ascii_lowercase() || b.is_ascii_digit() || matches!(b, b'_' | b'.' | b'-')
        })
}

fn shell_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

fn sudoers_escape(s: &str) -> String {
    let mut b = String::with_capacity(s.len() + 4);
    for ch in s.chars() {
        if matches!(
            ch,
            '\\' | ' ' | ',' | ':' | '=' | '(' | ')' | '!' | '"' | '#'
        ) {
            b.push('\\');
        }
        b.push(ch);
    }
    b
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;
    use std::fs;
    use std::io::Write;
    use std::os::unix::fs::{FileTypeExt, PermissionsExt, symlink};
    use std::path::Path;
    use std::process::{Command, Stdio};

    use nix::sys::stat::Mode;
    use nix::unistd::mkfifo;

    use super::*;

    const SCRIPT_GOLDEN: &str = include_str!("../../tests/testdata/system/TestScript.golden");
    const LOOPBACK_GOLDEN: &str =
        include_str!("../../tests/testdata/system/TestLoopbackPlist.golden");
    const SUDOERS_GOLDEN: &str = include_str!("../../tests/testdata/system/TestSudoers.golden");

    fn fake_paths() -> Paths {
        Paths::for_test(Path::new("/fake"))
    }

    fn write_file(path: &Path, content: &str) {
        if let Some(dir) = path.parent() {
            fs::create_dir_all(dir).unwrap();
        }
        fs::write(path, content).unwrap();
    }

    fn write_dir(dir: &Path, files: &[(&str, &str)]) {
        for (name, content) in files {
            write_file(&dir.join(name), content);
        }
    }

    fn read_dir_map(dir: &Path) -> BTreeMap<String, String> {
        let mut out = BTreeMap::new();
        let entries = match fs::read_dir(dir) {
            Ok(e) => e,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return out,
            Err(e) => panic!("read_dir {}: {e}", dir.display()),
        };
        for entry in entries {
            let entry = entry.unwrap();
            let name = entry.file_name().to_string_lossy().into_owned();
            let content = fs::read_to_string(entry.path()).unwrap();
            out.insert(name, content);
        }
        out
    }

    fn run_script_sh(p: &Paths, stdin_data: &str) -> (bool, String, String) {
        let dir = tempfile::tempdir().unwrap();
        let script_path = dir.path().join("apply-resolvers.sh");
        write_file(&script_path, &script(p));
        let mut child = Command::new("/bin/sh")
            .arg(&script_path)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        if let Some(mut stdin) = child.stdin.take() {
            let _ = stdin.write_all(stdin_data.as_bytes());
        }
        let output = child.wait_with_output().unwrap();
        (
            output.status.success(),
            String::from_utf8_lossy(&output.stdout).into_owned(),
            String::from_utf8_lossy(&output.stderr).into_owned(),
        )
    }

    #[test]
    fn resolver_file_matches_expected_lines() {
        assert_eq!(
            resolver_file(),
            "# lodo\nnameserver 127.0.0.1\nport 53535\n"
        );
    }

    #[test]
    fn script_matches_golden_byte_for_byte() {
        assert_eq!(script(&fake_paths()), SCRIPT_GOLDEN);
    }

    #[test]
    fn script_run_cases() {
        let res = resolver_file();
        let long = format!("{}abcde.test", "abc.".repeat(61)); // 254 chars

        struct Case<'a> {
            name: &'a str,
            list: String,
            before: Vec<(&'a str, String)>,
            after: BTreeMap<String, String>,
            want_stdout: &'a str,
            want_stderr: &'a str,
            want_fail: bool,
        }

        let map_of = |pairs: &[(&str, &str)]| -> BTreeMap<String, String> {
            pairs
                .iter()
                .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
                .collect()
        };

        let cases = [
            Case {
                name: "writes valid names",
                list: "crm.test\ntest.crm.test\n".into(),
                before: vec![],
                after: map_of(&[("crm.test", &res), ("test.crm.test", &res)]),
                want_stdout: "lodo: 2 resolver files written, 0 removed, 0 lines skipped\n",
                want_stderr: "",
                want_fail: false,
            },
            Case {
                name: "skips binary lines with null bytes",
                list: "a\0b\ncrm.test\n".into(),
                before: vec![],
                after: map_of(&[("crm.test", &res)]),
                want_stdout: "lodo: 1 resolver files written, 0 removed, 1 lines skipped\n",
                want_stderr: "",
                want_fail: false,
            },
            Case {
                name: "skips invalid lines",
                list: format!(
                    "lodo\n../x.lodo\nX.LODO\nx.local\n\nx.lodo \n-x.lodo\nflowy.lodo\na/b.lodo\n{long}\n"
                ),
                before: vec![],
                after: map_of(&[("flowy.lodo", &res)]),
                want_stdout: "lodo: 1 resolver files written, 0 removed, 9 lines skipped\n",
                want_stderr: "",
                want_fail: false,
            },
            Case {
                name: "last line without newline",
                list: "crm.test\nflowy.test".into(),
                before: vec![],
                after: map_of(&[("crm.test", &res), ("flowy.test", &res)]),
                want_stdout: "lodo: 2 resolver files written, 0 removed, 0 lines skipped\n",
                want_stderr: "",
                want_fail: false,
            },
            Case {
                name: "removes its files no longer listed",
                list: "crm.test\n".into(),
                before: vec![("old.test", res.clone()), ("crm.test", res.clone())],
                after: map_of(&[("crm.test", &res)]),
                want_stdout: "lodo: 1 resolver files written, 1 removed, 0 lines skipped\n",
                want_stderr: "",
                want_fail: false,
            },
            Case {
                name: "empty list removes all its files",
                list: String::new(),
                before: vec![("old.test", res.clone()), ("older.test", res.clone())],
                after: BTreeMap::new(),
                want_stdout: "lodo: 0 resolver files written, 2 removed, 0 lines skipped\n",
                want_stderr: "",
                want_fail: false,
            },
            Case {
                name: "leaves files it did not write",
                list: "crm.test\n".into(),
                before: vec![
                    ("crm.test", "nameserver 10.0.0.1\n".into()),
                    ("other.test", "nameserver 10.0.0.2\n".into()),
                    ("local", "nameserver 127.0.0.1\n".into()),
                ],
                after: map_of(&[
                    ("crm.test", "nameserver 10.0.0.1\n"),
                    ("other.test", "nameserver 10.0.0.2\n"),
                    ("local", "nameserver 127.0.0.1\n"),
                ]),
                want_stdout: "lodo: 0 resolver files written, 0 removed, 0 lines skipped\n",
                want_stderr: "crm.test alone: lodo did not write it",
                want_fail: false,
            },
            Case {
                name: "list as long as the cap",
                list: format!(
                    "crm.test\n{}",
                    "_".repeat(MAX_SCRIPT_INPUT - "crm.test\n".len())
                ),
                before: vec![],
                after: map_of(&[("crm.test", &res)]),
                want_stdout: "lodo: 1 resolver files written, 0 removed, 1 lines skipped\n",
                want_stderr: "",
                want_fail: false,
            },
            Case {
                name: "list longer than the cap",
                list: format!(
                    "crm.test\n{}",
                    "_".repeat(MAX_SCRIPT_INPUT - "crm.test\n".len() + 1)
                ),
                before: vec![("old.test", res.clone())],
                after: map_of(&[("old.test", &res)]),
                want_stdout: "",
                want_stderr: "is longer than",
                want_fail: true,
            },
        ];

        for c in cases {
            let dir = tempfile::tempdir().unwrap();
            let p = Paths::for_test(dir.path());
            let before_refs: Vec<(&str, &str)> =
                c.before.iter().map(|(k, v)| (*k, v.as_str())).collect();
            write_dir(&p.resolver_dir, &before_refs);

            let (ok, stdout, stderr) = run_script_sh(&p, &c.list);
            assert_eq!(!ok, c.want_fail, "{}: stderr={stderr}", c.name);
            if !c.want_fail {
                assert_eq!(stdout, c.want_stdout, "{}", c.name);
            }
            assert!(
                stderr.contains(c.want_stderr),
                "{}: stderr={stderr:?}, want={:?}",
                c.name,
                c.want_stderr
            );
            assert_eq!(read_dir_map(&p.resolver_dir), c.after, "{}", c.name);
            assert!(!p.resolver_dir.parent().unwrap().join("x.lodo").exists());
        }
    }

    #[test]
    fn script_run_sets_0644_file_mode() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        let (ok, _, stderr) = run_script_sh(&p, "crm.test\n");
        assert!(ok, "{stderr}");
        let mode = fs::metadata(p.resolver_dir.join("crm.test"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o644);
    }

    #[test]
    fn script_never_opens_fifo_paths_in_input() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        let cwd = tempfile::tempdir().unwrap();
        let fifo_dir = tempfile::tempdir().unwrap();
        let abs_fifo = fifo_dir.path().join("list.test");
        for fifo in [&abs_fifo, &cwd.path().join("crm.test")] {
            mkfifo(fifo, Mode::from_bits_truncate(0o644)).unwrap();
        }

        let script_dir = tempfile::tempdir().unwrap();
        let script_path = script_dir.path().join("apply-resolvers.sh");
        write_file(&script_path, &script(&p));
        let mut child = Command::new("/bin/sh")
            .arg(&script_path)
            .current_dir(cwd.path())
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        if let Some(mut stdin) = child.stdin.take() {
            let input = format!("{}\n/dev/stdin\ncrm.test\n", abs_fifo.display());
            stdin.write_all(input.as_bytes()).unwrap();
        }
        let out = child.wait_with_output().unwrap();
        assert!(
            out.status.success(),
            "{}",
            String::from_utf8_lossy(&out.stderr)
        );
        assert_eq!(
            String::from_utf8_lossy(&out.stdout),
            "lodo: 1 resolver files written, 0 removed, 2 lines skipped\n"
        );
        let got = read_dir_map(&p.resolver_dir);
        assert_eq!(got.len(), 1);
        assert_eq!(got.get("crm.test"), Some(&resolver_file()));
    }

    #[test]
    fn script_never_opens_special_targets_in_resolver_dir() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        fs::create_dir_all(&p.resolver_dir).unwrap();
        let fifo = p.resolver_dir.join("fifo.test");
        mkfifo(&fifo, Mode::from_bits_truncate(0o644)).unwrap();
        let sym = p.resolver_dir.join("sym.test");
        let real_target = dir.path().join("real");
        write_file(&real_target, &format!("{MARKER}\n"));
        symlink(&real_target, &sym).unwrap();

        let (ok, _, stderr) = run_script_sh(&p, "fifo.test\nsym.test\n");
        assert!(ok, "{stderr}");
        assert!(stderr.contains("fifo.test alone"));
        assert!(stderr.contains("sym.test alone"));
        assert!(fs::symlink_metadata(&fifo).unwrap().file_type().is_fifo());
        assert!(fs::symlink_metadata(&sym).unwrap().file_type().is_symlink());
    }

    #[test]
    fn script_handles_quoted_path_and_idempotence() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(&dir.path().join("it's here"));
        for _ in 0..2 {
            let (ok, _, stderr) = run_script_sh(&p, "crm.test\nflowy.test\n");
            assert!(ok, "{stderr}");
        }
        let got = read_dir_map(&p.resolver_dir);
        assert_eq!(got.len(), 2);
        assert_eq!(got.get("crm.test"), Some(&resolver_file()));
        assert_eq!(got.get("flowy.test"), Some(&resolver_file()));
    }

    #[test]
    fn script_agrees_with_store_validate_name() {
        let l63 = format!("{}.test", "a".repeat(63));
        let l64 = format!("{}.test", "a".repeat(64));
        let l250 = format!("{}a.test", "abc.".repeat(61));
        let l254 = format!("{}abcde.test", "abc.".repeat(61));
        let names = [
            "flowy.lodo",
            "a.custom",
            "a.test",
            "1.test",
            "a--b.test",
            "xn--bcher-kva.test",
            "a.b.c.d.test",
            "a.local.test",
            &l63,
            &l64,
            &l250,
            &l254,
            "test",
            "lodo",
            "a-.test",
            "-a.test",
            "TEST",
            "a.TEST",
            "a.test.",
            "a..test",
            ".a.test",
            "flowy.local",
            "flowy.localhost",
            "127.0.0.1",
            "a.tests",
            "localhost",
            "a.test\r",
            "a\tb.test",
            "a b.test",
            "é.test",
            "a/b.test",
            "..test",
        ];

        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        let input = format!("{}\n", names.join("\n"));
        let (ok, _, stderr) = run_script_sh(&p, &input);
        assert!(ok, "{stderr}");
        let written = read_dir_map(&p.resolver_dir);

        for name in names {
            let got_written = written.contains_key(name);
            let want_written = store::validate_name(name).is_ok();
            assert_eq!(got_written, want_written, "name {name:?}");
        }
    }

    #[test]
    fn loopback_plist_matches_golden_and_passes_plutil() {
        let plist = loopback_plist();
        assert_eq!(plist, LOOPBACK_GOLDEN);

        if !Path::new("/usr/bin/plutil").exists() {
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("io.lodo.loopback.plist");
        write_file(&path, &plist);
        let out = Command::new("/usr/bin/plutil")
            .args(["-lint", &path.to_string_lossy()])
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "plutil -lint failed: {}",
            String::from_utf8_lossy(&out.stdout)
        );
    }

    #[test]
    fn sudoers_matches_golden_and_passes_visudo() {
        let rule = sudoers(&fake_paths()).unwrap();
        assert_eq!(rule, SUDOERS_GOLDEN);

        if !Path::new("/usr/sbin/visudo").exists() {
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("lodo");
        write_file(&path, &rule);
        let out = Command::new("/usr/sbin/visudo")
            .args(["-c", "-f", &path.to_string_lossy()])
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "visudo -c failed: {}{}",
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        );
    }

    #[test]
    fn sudoers_refuses_odd_user() {
        for user in ["", "a b", "root,ALL", "x:y", "a\nALL ALL=(ALL) ALL", "-bad"] {
            let mut p = fake_paths();
            p.user = user.to_string();
            assert!(sudoers(&p).is_err(), "expected error for {user:?}");
        }
    }

    #[test]
    fn conf_and_caddy_blocks_match_expected() {
        let p = fake_paths();
        assert_eq!(
            conf_block(&p),
            "# lodo\nconf-file=/fake/Users/tester/.config/lodo/dnsmasq.conf\nlisten-address=127.0.0.1\nport=53535\nbind-interfaces\n"
        );
        assert_eq!(
            caddy_block(&p),
            "# lodo\nimport \"/fake/Users/tester/.config/lodo/Caddyfile\"\n"
        );
    }
}
