use std::collections::BTreeMap;
use std::fs::{self, File, FileTimes};
use std::os::unix::fs::{PermissionsExt, symlink};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::LazyLock;
use std::time::{Duration, SystemTime};

use regex::Regex;

use super::apply::{apply_with, set_service_with};
use super::setup::setup_with;
use super::uninstall::uninstall_with;
use super::*;
use crate::paths::Paths;
use crate::run::{FakeRunner, Runner, line};
use crate::store::{self, Domain};

const THIS_MAC_CONF: &str = include_str!("../../tests/testdata/system/this-mac-dnsmasq.conf");
const THIS_MAC_GOLDEN: &str =
    include_str!("../../tests/testdata/system/TestRewriteSystemConf_ThisMac.golden");

const CONF_BLOCK_FAKE: &str = "# lodo\nconf-file=/fake/Users/tester/.config/lodo/dnsmasq.conf\nlisten-address=127.0.0.1\nport=53535\nbind-interfaces\n";
const ROOT_PEM: &str = "-----BEGIN CERTIFICATE-----\nstand-in\n-----END CERTIFICATE-----\n";

#[expect(clippy::expect_used)]
static PID_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"pid = ([0-9]+)").expect("valid PID_RE"));

struct MockDeps;

impl Deps for MockDeps {
    fn dnsmasq_config(&self, domains: &[Domain], log_path: &Path) -> String {
        let mut out = String::from(store::GENERATED_HEADER);
        out.push_str(&format!(
            "log-queries\nlog-facility={}\n",
            log_path.display()
        ));
        for d in domains {
            if d.enabled {
                out.push_str(&format!("address=/{}/{}\n", d.name, d.address));
            }
        }
        out
    }

    fn resolver_list(&self, domains: &[Domain]) -> String {
        let mut out = String::new();
        for d in domains {
            if d.enabled {
                out.push_str(&d.name);
                out.push('\n');
            }
        }
        out
    }

    fn caddy_config(&self, domains: &[Domain]) -> String {
        let mut out = String::from(store::GENERATED_HEADER);
        for d in domains {
            if d.has_site() {
                let scheme = if d.https { "https" } else { "http" };
                out.push_str(&format!(
                    "{scheme}://{} {{\n\treverse_proxy {}:{}\n}}\n",
                    d.name, d.address, d.port
                ));
            }
        }
        out
    }

    fn caddy_validate(&self, r: &dyn Runner, caddy_bin: &Path, path: &Path) -> Result<(), String> {
        r.run(
            None,
            &caddy_bin.to_string_lossy(),
            &[
                "validate",
                "--config",
                &path.to_string_lossy(),
                "--adapter",
                "caddyfile",
            ],
        )
        .map(|_| ())
        .map_err(|e| format!("validate Caddyfile: {e}"))
    }

    fn caddy_log_error(&self, path: &Path, since: SystemTime) -> String {
        let Ok(meta) = fs::metadata(path) else {
            return String::new();
        };
        let Ok(modified) = meta.modified() else {
            return String::new();
        };
        if modified < since {
            return String::new();
        }
        let Ok(data) = fs::read_to_string(path) else {
            return String::new();
        };
        let mut last = String::new();
        for l in data.lines() {
            if let Ok(v) = serde_json::from_str::<serde_json::Value>(l)
                && v.get("level").and_then(|x| x.as_str()) == Some("error")
                && let Some(msg) = v.get("msg").and_then(|x| x.as_str())
            {
                last = msg.to_string();
            }
        }
        last
    }

    fn brew_info(&self, r: &dyn Runner, p: &Paths, service: &str) -> brew::Status {
        let plist = p.launch_agents.join(format!("sh.brew.{service}.plist"));
        let registered = fsutil::exists(&plist);
        let tgt = format!("gui/{}/sh.brew.{service}", p.uid);
        match r.run(None, &p.launchctl.to_string_lossy(), &["print", &tgt]) {
            Ok(out) => {
                let pid = PID_RE
                    .captures(&out)
                    .and_then(|c| c.get(1))
                    .and_then(|m| m.as_str().parse::<u32>().ok())
                    .unwrap_or(0);
                brew::Status {
                    running: pid > 0,
                    user: p.user.clone(),
                    pid,
                    registered: true,
                }
            }
            Err(_) => brew::Status {
                running: false,
                user: String::new(),
                pid: 0,
                registered,
            },
        }
    }

    fn brew_restart(&self, r: &dyn Runner, p: &Paths, service: &str) -> Result<(), String> {
        let launchctl = p.launchctl.to_string_lossy();
        let tgt = format!("gui/{}/sh.brew.{service}", p.uid);
        if r.run(None, &launchctl, &["print", &tgt]).is_ok() {
            return r
                .run(None, &launchctl, &["kickstart", "-k", &tgt])
                .map(|_| ())
                .map_err(|e| format!("restart {service}: {e}"));
        }
        r.run(
            None,
            &p.brew.to_string_lossy(),
            &["services", "restart", service],
        )
        .map(|_| ())
        .map_err(|e| format!("restart {service}: {e}"))
    }

    fn brew_stop(&self, r: &dyn Runner, p: &Paths, service: &str) -> Result<(), String> {
        r.run(
            None,
            &p.brew.to_string_lossy(),
            &["services", "stop", service],
        )
        .map(|_| ())
        .map_err(|e| format!("stop {service}: {e}"))
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum SvcState {
    Running,
    Crashed,
    Off,
}

fn fake_paths() -> Paths {
    Paths::for_test(Path::new("/fake"))
}

fn write_file(path: &Path, content: &str) {
    if let Some(dir) = path.parent() {
        fs::create_dir_all(dir).unwrap();
    }
    fs::write(path, content).unwrap();
}

fn print_job(p: &Paths, service: &str) -> String {
    line(
        &p.launchctl.to_string_lossy(),
        &["print", &format!("gui/501/sh.brew.{service}")],
    )
}

fn restart_calls(p: &Paths, service: &str) -> Vec<String> {
    vec![
        print_job(p, service),
        line(
            &p.launchctl.to_string_lossy(),
            &["kickstart", "-k", &format!("gui/501/sh.brew.{service}")],
        ),
    ]
}

fn set_mock_service(p: &Paths, r: &FakeRunner, service: &str, state: SvcState) {
    let plist = p.launch_agents.join(format!("sh.brew.{service}.plist"));
    let job = print_job(p, service);
    match state {
        SvcState::Running => {
            write_file(&plist, "");
            r.set(job, "\tstate = running\n\tpid = 42\n");
        }
        SvcState::Crashed => {
            write_file(&plist, "");
            r.set(job, "\tstate = not running\n");
        }
        SvcState::Off => {
            let _ = fs::remove_file(&plist);
            r.fail(job, "Could not find service");
        }
    }
}

fn new_mac(dir: &Path, with_caddy: bool) -> (Paths, FakeRunner) {
    let p = Paths::for_test(dir);
    write_file(&p.brew, "");
    write_file(&p.dnsmasq, "");
    write_file(&p.system_conf, "# dnsmasq\n#port=5353\n");
    if with_caddy {
        write_file(&p.caddy, "");
    }
    let r = FakeRunner::new();
    for label in DNS_SYSTEM_LABELS {
        r.fail(
            line(
                &p.launchctl.to_string_lossy(),
                &["print", &format!("system/{label}")],
            ),
            "Could not find service",
        );
    }
    set_mock_service(&p, &r, "dnsmasq", SvcState::Running);
    if with_caddy {
        set_mock_service(&p, &r, "caddy", SvcState::Running);
    }
    (p, r)
}

fn set_up_mac(dir: &Path, with_caddy: bool) -> (Paths, FakeRunner) {
    let (p, r) = new_mac(dir, with_caddy);
    write_file(&p.system_conf, &rewrite_system_conf("", &p));
    write_file(&p.script, &script(&p));
    (p, r)
}

fn setup_calls(p: &Paths, with_caddy: bool) -> Vec<String> {
    let sudo = p.sudo.to_string_lossy();
    let install_bin = p.install.to_string_lossy();
    let launchctl = p.launchctl.to_string_lossy();
    let visudo = p.visudo.to_string_lossy();

    let install_call = |mode: &str, name: &str, target: &Path| -> String {
        line(
            &sudo,
            &[
                "-n",
                &install_bin,
                "-o",
                "root",
                "-g",
                "wheel",
                "-m",
                mode,
                &p.staging.join(name).to_string_lossy(),
                &target.to_string_lossy(),
            ],
        )
    };

    let mut calls = vec![
        line(&sudo, &["-v"]),
        line(&launchctl, &["print", "system/homebrew.mxcl.dnsmasq"]),
        line(&launchctl, &["print", "system/sh.brew.dnsmasq"]),
        line(
            &sudo,
            &[
                "-n",
                &install_bin,
                "-d",
                "-o",
                "root",
                "-g",
                "wheel",
                "-m",
                "755",
                &p.script.parent().unwrap().to_string_lossy(),
            ],
        ),
        install_call("755", "apply-resolvers.sh", &p.script),
        line(
            &visudo,
            &["-c", "-f", &p.staging.join("lodo").to_string_lossy()],
        ),
        install_call("440", "lodo", &p.sudoers),
        install_call("644", "io.lodo.loopback.plist", &p.loopback_plist),
        line(
            &sudo,
            &["-n", &launchctl, "bootout", "system/io.lodo.loopback"],
        ),
        line(
            &sudo,
            &[
                "-n",
                &launchctl,
                "bootstrap",
                "system",
                &p.loopback_plist.to_string_lossy(),
            ],
        ),
    ];
    calls.extend(restart_calls(p, "dnsmasq"));
    if with_caddy {
        calls.push(line(
            &p.caddy.to_string_lossy(),
            &[
                "validate",
                "--config",
                &p.system_caddyfile.to_string_lossy(),
                "--adapter",
                "caddyfile",
            ],
        ));
        calls.extend(restart_calls(p, "caddy"));
        calls.push(print_job(p, "caddy"));
    }
    calls.push(line(&sudo, &["-n", "-k", &p.script.to_string_lossy()]));
    calls
}

fn trust_call(p: &Paths) -> String {
    line(
        &p.sudo.to_string_lossy(),
        &[
            &p.security.to_string_lossy(),
            "add-trusted-cert",
            "-d",
            "-r",
            "trustRoot",
            "-k",
            &p.system_keychain.to_string_lossy(),
            &p.trusted_ca.to_string_lossy(),
        ],
    )
}

fn untrust_call(p: &Paths) -> String {
    line(
        &p.sudo.to_string_lossy(),
        &[
            &p.security.to_string_lossy(),
            "remove-trusted-cert",
            "-d",
            &p.trusted_ca.to_string_lossy(),
        ],
    )
}

fn uninstall_calls(
    p: &Paths,
    with_script: bool,
    with_caddy: bool,
    active_addrs: &[&str],
) -> Vec<String> {
    let sudo = p.sudo.to_string_lossy();
    let brew_bin = p.brew.to_string_lossy();
    let launchctl = p.launchctl.to_string_lossy();
    let rm = p.rm.to_string_lossy();
    let rmdir = p.rmdir.to_string_lossy();
    let ifconfig = p.ifconfig.to_string_lossy();

    let mut calls = vec![line(&sudo, &["-v"])];
    if with_script {
        calls.push(line(&sudo, &["-n", &p.script.to_string_lossy()]));
    }
    calls.push(line(&brew_bin, &["services", "stop", "dnsmasq"]));
    if with_caddy {
        calls.push(line(&brew_bin, &["services", "stop", "caddy"]));
    }
    calls.extend([
        line(&launchctl, &["print", "gui/501/io.lodo.menubar"]),
        line(
            &sudo,
            &["-n", &launchctl, "bootout", "system/io.lodo.loopback"],
        ),
        line(
            &sudo,
            &["-n", &rm, "-f", &p.loopback_plist.to_string_lossy()],
        ),
        line(&ifconfig, &["lo0"]),
    ]);
    for addr in active_addrs {
        calls.push(line(&sudo, &["-n", &ifconfig, "lo0", "-alias", addr]));
    }
    calls.extend([
        line(
            &sudo,
            &[
                "-n",
                &rm,
                "-f",
                &p.sudoers.to_string_lossy(),
                &p.script.to_string_lossy(),
            ],
        ),
        line(
            &sudo,
            &["-n", &rmdir, &p.script.parent().unwrap().to_string_lossy()],
        ),
    ]);
    calls
}

fn snapshot_tree(root: &Path) -> BTreeMap<PathBuf, String> {
    fn walk(dir: &Path, out: &mut BTreeMap<PathBuf, String>) {
        let Ok(entries) = fs::read_dir(dir) else {
            return;
        };
        for e in entries.flatten() {
            let path = e.path();
            let Ok(ft) = e.file_type() else { continue };
            if ft.is_dir() {
                walk(&path, out);
            } else if ft.is_file() {
                out.insert(path.clone(), fs::read_to_string(path).unwrap_or_default());
            }
        }
    }
    let mut out = BTreeMap::new();
    walk(root, &mut out);
    out
}

#[test]
fn rewrite_and_strip_system_conf() {
    let p = fake_paths();
    let cases = [
        ("empty file", "", CONF_BLOCK_FAKE.to_string()),
        (
            "only comments",
            "# dnsmasq\n#port=5353\n",
            format!("# dnsmasq\n#port=5353\n\n{CONF_BLOCK_FAKE}"),
        ),
        (
            "replaces options lodo owns",
            "no-resolv\nport=53\nlisten-address=127.0.0.1,10.0.0.1\n  conf-file = /old.conf\nbind-dynamic\nserver=1.1.1.1\n\n\n",
            format!("no-resolv\nserver=1.1.1.1\n\n{CONF_BLOCK_FAKE}"),
        ),
        (
            "already lodo's",
            &format!("# dnsmasq\n\n{CONF_BLOCK_FAKE}"),
            format!("# dnsmasq\n\n{CONF_BLOCK_FAKE}"),
        ),
        (
            "lodo's block in the middle",
            &format!("a=1\n{CONF_BLOCK_FAKE}b=2\n"),
            format!("a=1\nb=2\n\n{CONF_BLOCK_FAKE}"),
        ),
    ];

    for (name, old, want) in cases {
        let got = rewrite_system_conf(old, &p);
        assert_eq!(got, want, "{name}");
        assert_eq!(rewrite_system_conf(&got, &p), got, "{name} idempotent");
    }

    assert_eq!(rewrite_system_conf(THIS_MAC_CONF, &p), THIS_MAC_GOLDEN);

    let with_block = format!("# dnsmasq\nserver=1.1.1.1\n\n{CONF_BLOCK_FAKE}");
    assert_eq!(
        strip_system_conf(&with_block),
        "# dnsmasq\nserver=1.1.1.1\n"
    );
    assert_eq!(strip_system_conf(CONF_BLOCK_FAKE), "");
}

#[test]
fn rewrite_strip_and_admin_disabled_caddyfile() {
    let p = fake_paths();
    let cblock = caddy_block(&p);
    let global = CADDY_GLOBAL_BLOCK;
    let site = "example.com {\n\trespond \"hi\"\n}\n";
    let user_global = format!("# mine\n{{\n\temail me@example.com\n}}\n\n{site}");
    let user_global_disabled =
        format!("# mine\n{{\n\tadmin off # lodo\n\temail me@example.com\n}}\n\n{site}");
    let user_global_own_off =
        format!("# mine\n{{\n\tadmin off\n\temail me@example.com\n}}\n\n{site}");
    let user_global_open =
        format!("# mine\n{{\n\tadmin localhost:2019\n\temail me@example.com\n}}\n\n{site}");

    let rewrite_cases = [
        ("no file", String::new(), format!("{global}\n{cblock}")),
        (
            "keeps other sites",
            site.to_string(),
            format!("{global}\n{site}\n{cblock}"),
        ),
        (
            "already lodo's",
            format!("{global}\n{site}\n{cblock}"),
            format!("{global}\n{site}\n{cblock}"),
        ),
        (
            "moves import to end",
            format!("{cblock}\n{site}"),
            format!("{global}\n{site}\n{cblock}"),
        ),
        (
            "moves global to top",
            format!("{site}\n{global}"),
            format!("{global}\n{site}\n{cblock}"),
        ),
        (
            "adds admin off to user global",
            user_global.clone(),
            format!("{user_global_disabled}\n{cblock}"),
        ),
        (
            "keeps user own admin off",
            user_global_own_off.clone(),
            format!("{user_global_own_off}\n{cblock}"),
        ),
        (
            "replaces open admin in user global",
            user_global_open,
            format!("{user_global_disabled}\n{cblock}"),
        ),
        (
            "user global after lodo's",
            format!("{global}\n{user_global}"),
            format!("{user_global_disabled}\n{cblock}"),
        ),
    ];
    for (name, old, want) in rewrite_cases {
        let got = rewrite_system_caddyfile(&old, &p);
        assert_eq!(got, want, "{name}");
        assert_eq!(rewrite_system_caddyfile(&got, &p), got, "{name} idempotent");
    }

    let unquoted = format!("{MARKER}\nimport {}\n", p.caddyfile.display());
    let ug = "{\n\temail me@example.com\n}\n";
    let ug_admin = "{\n\tadmin off # lodo\n\temail me@example.com\n}\n";
    let strip_cases = [
        (
            "import block",
            format!("{site}\n{cblock}"),
            site.to_string(),
        ),
        (
            "unquoted legacy import",
            format!("{site}\n{unquoted}"),
            site.to_string(),
        ),
        (
            "lodo blocks alone",
            format!("{global}\n{cblock}"),
            String::new(),
        ),
        (
            "both blocks around site",
            format!("{global}\n{site}\n{cblock}"),
            site.to_string(),
        ),
        (
            "strips injected admin off",
            format!("{ug_admin}\n{site}\n{cblock}"),
            format!("{ug}\n{site}"),
        ),
        (
            "marker before site keeps site",
            format!("# lodo\n{site}"),
            site.to_string(),
        ),
        (
            "unclosed block after marker stays",
            "# lodo\n{\n\tadmin off\n".to_string(),
            "{\n\tadmin off\n".to_string(),
        ),
    ];
    for (name, old, want) in strip_cases {
        assert_eq!(strip_system_caddyfile(&old, &p), want, "{name}");
    }

    let admin_cases = [
        (format!("{global}\n{cblock}"), true),
        (rewrite_system_caddyfile(ug, &p), true),
        (site.to_string(), false),
        (ug.to_string(), false),
        ("{\n\tadmin :2019\n}\n".to_string(), false),
    ];
    for (content, want) in admin_cases {
        let dir = tempfile::tempdir().unwrap();
        let tp = Paths::for_test(dir.path());
        write_file(&tp.system_caddyfile, &content);
        assert_eq!(caddy_admin_disabled(&tp), want);
    }
}

#[test]
fn real_caddy_and_dnsmasq_accept_rewritten_configs() {
    let caddy_bin = Path::new("/opt/homebrew/bin/caddy");
    if caddy_bin.exists() {
        for old in [
            "",
            "example.com {\n\trespond \"hi\"\n}\n",
            "{\n\temail me@example.com\n}\n",
            "{\n\tadmin localhost:2019\n}\n",
        ] {
            let tmp = tempfile::tempdir().unwrap();
            let dir = tmp.path().join("with spaces");
            let p = Paths::for_test(&dir);
            write_file(&p.caddyfile, "");
            write_file(&p.system_caddyfile, &rewrite_system_caddyfile(old, &p));
            let out = Command::new(caddy_bin)
                .args([
                    "adapt",
                    "--config",
                    &p.system_caddyfile.to_string_lossy(),
                    "--adapter",
                    "caddyfile",
                ])
                .env("HOME", &dir)
                .env("XDG_DATA_HOME", &dir)
                .env("XDG_CONFIG_HOME", &dir)
                .output()
                .unwrap();
            assert!(
                out.status.success(),
                "caddy adapt failed: {}",
                String::from_utf8_lossy(&out.stderr)
            );
            assert!(String::from_utf8_lossy(&out.stdout).contains("\"admin\":{\"disabled\":true}"));
        }
    }

    let dnsmasq_bin = Path::new("/opt/homebrew/opt/dnsmasq/sbin/dnsmasq");
    if dnsmasq_bin.exists() {
        let tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(tmp.path());
        let sample = vec![Domain {
            name: "crm.test".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            ..Domain::default()
        }];
        write_files_with(&MockDeps, &p, &sample).unwrap();
        let conf = tmp.path().join("dnsmasq.conf");
        write_file(&conf, &rewrite_system_conf(THIS_MAC_CONF, &p));
        let out = Command::new(dnsmasq_bin)
            .args(["--test", &format!("--conf-file={}", conf.display())])
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "dnsmasq --test failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
    }
}

#[test]
fn write_files_tracks_changed_paths() {
    let dir = tempfile::tempdir().unwrap();
    let p = Paths::for_test(dir.path());
    let sample = vec![
        Domain {
            name: "crm.test".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "dashboard.crm.test".into(),
            address: "127.0.1.1".into(),
            port: 3000,
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "flowy.test".into(),
            address: "127.0.1.3".into(),
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "old.test".into(),
            address: "127.0.0.1".into(),
            ..Domain::default()
        },
    ];

    let changed = write_files_with(&MockDeps, &p, &sample).unwrap();
    assert_eq!(
        changed,
        vec![
            p.dnsmasq_conf.clone(),
            p.resolvers.clone(),
            p.caddyfile.clone()
        ]
    );
    assert!(write_files_with(&MockDeps, &p, &sample).unwrap().is_empty());

    let mut port_changed = sample;
    port_changed[1].port = 3001;
    assert_eq!(
        write_files_with(&MockDeps, &p, &port_changed).unwrap(),
        vec![p.caddyfile.clone()]
    );

    let toggled = store::toggle(&port_changed, "crm.test").unwrap();
    assert_eq!(
        write_files_with(&MockDeps, &p, &toggled).unwrap(),
        vec![p.dnsmasq_conf.clone(), p.resolvers.clone()]
    );
}

#[test]
fn setup_with_and_without_caddy() {
    for with_caddy in [true, false] {
        let dir = tempfile::tempdir().unwrap();
        let (p, r) = new_mac(dir.path(), with_caddy);
        let mut out = Vec::new();
        setup_with(&MockDeps, &p, &r, &mut out).unwrap();
        let out_str = String::from_utf8(out).unwrap();

        assert_eq!(r.calls(), setup_calls(&p, with_caddy));
        assert_eq!(
            fs::read_to_string(&p.system_conf_backup).unwrap(),
            "# dnsmasq\n#port=5353\n"
        );
        assert_eq!(
            fs::read_to_string(&p.system_conf).unwrap(),
            format!("# dnsmasq\n#port=5353\n\n{}", conf_block(&p))
        );
        assert_eq!(
            fs::read_to_string(&p.domains_json).unwrap(),
            "{\n  \"version\": 1,\n  \"domains\": []\n}\n"
        );
        for f in [&p.dnsmasq_conf, &p.resolvers, &p.caddyfile] {
            assert!(f.exists());
        }
        if with_caddy {
            assert_eq!(
                fs::read_to_string(&p.system_caddyfile).unwrap(),
                format!("{CADDY_GLOBAL_BLOCK}\n{}", caddy_block(&p))
            );
            assert!(!p.system_caddyfile_backup.exists());
        } else {
            assert!(
                out_str.contains("– Caddy serves lodo's sites on port 80: Caddy is not installed")
            );
        }
        assert!(!p.staging.exists());
    }
}

#[test]
fn setup_idempotency_backups_root_dnsmasq_leftovers_and_failures() {
    // Enabled names passed to prove_script.
    let dir = tempfile::tempdir().unwrap();
    let (p, r) = new_mac(dir.path(), false);
    store::save(
        &p.domains_json,
        &[
            Domain {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "old.test".into(),
                address: "127.0.1.2".into(),
                ..Domain::default()
            },
        ],
    )
    .unwrap();
    setup_with(&MockDeps, &p, &r, &mut Vec::new()).unwrap();
    let prove_cmd = line(
        &p.sudo.to_string_lossy(),
        &["-n", "-k", &p.script.to_string_lossy()],
    );
    assert_eq!(r.inputs(&prove_cmd), vec!["crm.test\n".to_string()]);

    // Three consecutive runs with HTTPS domain keep files and commands stable.
    let dir3 = tempfile::tempdir().unwrap();
    let (p3, r3) = new_mac(dir3.path(), true);
    write_file(&p3.system_caddyfile, "example.com {\n\trespond \"hi\"\n}\n");
    write_file(&p3.caddy_root, ROOT_PEM);
    store::save(
        &p3.domains_json,
        &[Domain {
            name: "crm.test".into(),
            address: "127.0.1.1".into(),
            port: 3000,
            enabled: true,
            https: true,
            ..Domain::default()
        }],
    )
    .unwrap();

    let mut outs = Vec::new();
    let mut call_slices = Vec::new();
    let mut snaps = Vec::new();
    for _ in 0..3 {
        let before = r3.calls().len();
        let mut out = Vec::new();
        setup_with(&MockDeps, &p3, &r3, &mut out).unwrap();
        outs.push(String::from_utf8(out).unwrap());
        call_slices.push(r3.calls()[before..].to_vec());
        snaps.push(snapshot_tree(dir3.path()));
    }
    assert_eq!(
        outs[1],
        outs[0].replacen("(backup in ", "(backup kept in ", 1)
    );
    assert_eq!(outs[2], outs[1]);
    let mut want_run2 = call_slices[0].clone();
    let idx = want_run2
        .iter()
        .position(|c| *c == trust_call(&p3))
        .unwrap();
    want_run2[idx] = line(
        &p3.security.to_string_lossy(),
        &["verify-cert", "-c", &p3.trusted_ca.to_string_lossy()],
    );
    assert_eq!(call_slices[1], want_run2);
    assert_eq!(call_slices[2], want_run2);
    assert_eq!(snaps[1], snaps[0]);
    assert_eq!(snaps[2], snaps[0]);

    // Repairs backups lodo had polluted, including markerless Caddy backup.
    let dir_rep = tempfile::tempdir().unwrap();
    let (pr, rr) = new_mac(dir_rep.path(), true);
    let lodo_only = format!("{MARKER}\nimport {}\n", pr.caddyfile.display());
    write_file(&pr.system_caddyfile, &lodo_only);
    write_file(&pr.system_caddyfile_backup, &lodo_only);
    write_file(
        &pr.system_conf_backup,
        &format!(
            "# dnsmasq\nport=5353\n{MARKER}\nconf-file={}\n",
            pr.dnsmasq_conf.display()
        ),
    );
    setup_with(&MockDeps, &pr, &rr, &mut Vec::new()).unwrap();
    assert!(!pr.system_caddyfile_backup.exists());
    assert_eq!(
        fs::read_to_string(&pr.system_conf_backup).unwrap(),
        "# dnsmasq\n"
    );

    let dir_ml = tempfile::tempdir().unwrap();
    let (pml, rml) = new_mac(dir_ml.path(), true);
    write_file(&pml.system_conf, "# dnsmasq\nport=5353\n");
    let markerless = format!("import \"{}\"\n", pml.caddyfile.display());
    write_file(
        &pml.system_caddyfile,
        &format!("{CADDY_GLOBAL_BLOCK}\n{markerless}\n{}", caddy_block(&pml)),
    );
    write_file(&pml.system_caddyfile_backup, &markerless);
    for _ in 0..2 {
        setup_with(&MockDeps, &pml, &rml, &mut Vec::new()).unwrap();
    }
    assert!(!pml.system_caddyfile_backup.exists());
    assert_eq!(
        fs::read_to_string(&pml.system_caddyfile).unwrap(),
        format!("{CADDY_GLOBAL_BLOCK}\n{}", caddy_block(&pml))
    );
    assert_eq!(
        fs::read_to_string(&pml.system_conf_backup).unwrap(),
        "# dnsmasq\nport=5353\n"
    );

    // Root dnsmasq, resolver/local, menubar refresh, and leftovers.
    let dir_misc = tempfile::tempdir().unwrap();
    let (pm, rm_run) = new_mac(dir_misc.path(), false);
    rm_run.set(
        line(
            &pm.launchctl.to_string_lossy(),
            &["print", "system/homebrew.mxcl.dnsmasq"],
        ),
        "state = running",
    );
    let daemon_plist = pm.launch_daemons.join("homebrew.mxcl.dnsmasq.plist");
    write_file(&daemon_plist, "<plist/>");
    write_file(&pm.resolver_local, "nameserver 127.0.0.1\n");
    write_file(&pm.leftovers[0].join("dnsmasq.conf"), "");
    write_file(&pm.swiftc, "");
    write_file(&pm.menubar_plist, "<plist/>");
    write_file(&pm.menubar_bin, "old");
    write_file(
        &PathBuf::from(format!("{}.swift", pm.menubar_bin.display())),
        "// stale\n",
    );
    rm_run.set(
        line(
            &pm.launchctl.to_string_lossy(),
            &["print", "gui/501/io.lodo.menubar"],
        ),
        "\tstate = running\n\tpid = 999\n",
    );
    let mut misc_out = Vec::new();
    setup_with(&MockDeps, &pm, &rm_run, &mut misc_out).unwrap();
    let misc_str = String::from_utf8(misc_out).unwrap();
    assert!(misc_str.contains(&format!(
        "stopped system/homebrew.mxcl.dnsmasq, removed {}",
        daemon_plist.display()
    )));
    assert!(misc_str.contains("✓ menu bar icon up to date"));
    assert!(rm_run.calls().contains(&line(
        &pm.sudo.to_string_lossy(),
        &[
            "-n",
            &pm.rm.to_string_lossy(),
            "-f",
            &pm.resolver_local.to_string_lossy(),
        ]
    )));
    assert!(misc_str.contains("Left alone, from LocalDNS"));
    assert!(pm.leftovers[0].exists());

    // Failure cases.
    struct SetupFailCase {
        name: &'static str,
        prep: fn(&Paths, &FakeRunner),
        want_step: &'static str,
        want_fix: &'static str,
        want_calls: usize,
    }
    let fail_cases = [
        SetupFailCase {
            name: "dnsmasq missing",
            prep: |p, _| fs::remove_file(&p.dnsmasq).unwrap(),
            want_step: "Homebrew and dnsmasq are installed",
            want_fix: "brew install dnsmasq",
            want_calls: 0,
        },
        SetupFailCase {
            name: "password refused",
            prep: |p, r| {
                r.fail(
                    line(&p.sudo.to_string_lossy(), &["-v"]),
                    "Sorry, try again.",
                )
            },
            want_step: "admin password",
            want_fix: "run lodo setup again and enter your password",
            want_calls: 1,
        },
        SetupFailCase {
            name: "invalid domains.json",
            prep: |p, _| {
                write_file(
                    &p.domains_json,
                    r#"{"version":1,"domains":[{"name":"X","address":"127.0.0.1","enabled":true}]}"#,
                );
            },
            want_step: "lodo's files in",
            want_fix: "fix or remove",
            want_calls: 1,
        },
        SetupFailCase {
            name: "visudo rejects rule",
            prep: |p, r| {
                r.fail(
                    line(
                        &p.visudo.to_string_lossy(),
                        &["-c", "-f", &p.staging.join("lodo").to_string_lossy()],
                    ),
                    "syntax error",
                );
            },
            want_step: "sudoers rule installed",
            want_fix: "lodo setup",
            want_calls: 6,
        },
        SetupFailCase {
            name: "sudo rule does not work",
            prep: |p, r| {
                r.fail(
                    line(
                        &p.sudo.to_string_lossy(),
                        &["-n", "-k", &p.script.to_string_lossy()],
                    ),
                    "sudo: a password is required",
                );
            },
            want_step: "sudo runs the resolver script without a password",
            want_fix: "lodo setup",
            want_calls: 13,
        },
    ];

    for c in fail_cases {
        let fdir = tempfile::tempdir().unwrap();
        let (fp, fr) = new_mac(fdir.path(), false);
        (c.prep)(&fp, &fr);
        let err = setup_with(&MockDeps, &fp, &fr, &mut Vec::new()).unwrap_err();
        assert!(err.step.starts_with(c.want_step), "{}: {err:?}", c.name);
        assert!(err.fix.starts_with(c.want_fix), "{}: {err:?}", c.name);
        assert_eq!(
            fr.calls().len(),
            c.want_calls,
            "{}: {:?}",
            c.name,
            fr.calls()
        );
    }
}

#[test]
fn apply_and_set_service_matrix() {
    let dns_only = vec![Domain {
        name: "crm.test".into(),
        address: "127.0.1.1".into(),
        enabled: true,
        ..Domain::default()
    }];
    let with_port = vec![
        Domain {
            name: "crm.test".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "dashboard.crm.test".into(),
            address: "127.0.1.1".into(),
            port: 3000,
            enabled: true,
            ..Domain::default()
        },
    ];

    struct ApplyCase<'a> {
        name: &'a str,
        caddy_set_up: bool,
        before: Option<&'a [Domain]>,
        domains: &'a [Domain],
        dnsmasq: Option<SvcState>,
        caddy: Option<SvcState>,
        keep_dnsmasq: bool,
        want_caddy: &'a [&'a str],
        want_err: &'a str,
    }

    let cases = [
        ApplyCase {
            name: "dns only, no caddy",
            caddy_set_up: false,
            before: None,
            domains: &dns_only,
            dnsmasq: None,
            caddy: None,
            keep_dnsmasq: false,
            want_caddy: &[],
            want_err: "",
        },
        ApplyCase {
            name: "port without caddy set up",
            caddy_set_up: false,
            before: None,
            domains: &with_port,
            dnsmasq: None,
            caddy: None,
            keep_dnsmasq: false,
            want_caddy: &[],
            want_err: "Caddy is not set up for lodo",
        },
        ApplyCase {
            name: "dnsmasq turned off",
            caddy_set_up: false,
            before: None,
            domains: &dns_only,
            dnsmasq: Some(SvcState::Off),
            caddy: None,
            keep_dnsmasq: false,
            want_caddy: &[],
            want_err: "",
        },
        ApplyCase {
            name: "dnsmasq config unchanged",
            caddy_set_up: false,
            before: Some(&dns_only),
            domains: &dns_only,
            dnsmasq: None,
            caddy: None,
            keep_dnsmasq: true,
            want_caddy: &[],
            want_err: "",
        },
        ApplyCase {
            name: "dnsmasq config unchanged, dnsmasq crashed",
            caddy_set_up: false,
            before: Some(&dns_only),
            domains: &dns_only,
            dnsmasq: Some(SvcState::Crashed),
            caddy: None,
            keep_dnsmasq: false,
            want_caddy: &[],
            want_err: "",
        },
        ApplyCase {
            name: "port added",
            caddy_set_up: true,
            before: None,
            domains: &with_port,
            dnsmasq: None,
            caddy: None,
            keep_dnsmasq: false,
            want_caddy: &["info", "validate", "restart", "info"],
            want_err: "",
        },
        ApplyCase {
            name: "ports unchanged, caddy running",
            caddy_set_up: true,
            before: Some(&with_port),
            domains: &with_port,
            dnsmasq: None,
            caddy: None,
            keep_dnsmasq: true,
            want_caddy: &["info"],
            want_err: "",
        },
        ApplyCase {
            name: "ports unchanged, caddy crashed",
            caddy_set_up: true,
            before: Some(&with_port),
            domains: &with_port,
            dnsmasq: None,
            caddy: Some(SvcState::Crashed),
            keep_dnsmasq: true,
            want_caddy: &["info", "validate", "restart", "info"],
            want_err: "caddy stopped right after it started",
        },
        ApplyCase {
            name: "port added, caddy turned off",
            caddy_set_up: true,
            before: None,
            domains: &with_port,
            dnsmasq: None,
            caddy: Some(SvcState::Off),
            keep_dnsmasq: false,
            want_caddy: &["info"],
            want_err: "",
        },
        ApplyCase {
            name: "last port removed",
            caddy_set_up: true,
            before: Some(&with_port),
            domains: &dns_only,
            dnsmasq: None,
            caddy: None,
            keep_dnsmasq: false,
            want_caddy: &["info", "validate", "restart", "info"],
            want_err: "",
        },
    ];

    for c in cases {
        let dir = tempfile::tempdir().unwrap();
        let (p, r) = set_up_mac(dir.path(), true);
        if c.caddy_set_up {
            write_file(&p.system_caddyfile, &caddy_block(&p));
        }
        if let Some(before) = c.before {
            write_files_with(&MockDeps, &p, before).unwrap();
        }
        if let Some(st) = c.dnsmasq {
            set_mock_service(&p, &r, "dnsmasq", st);
        }
        if let Some(st) = c.caddy {
            set_mock_service(&p, &r, "caddy", st);
        }

        let res = apply_with(&MockDeps, &p, &r, c.domains);
        if c.want_err.is_empty() {
            res.unwrap_or_else(|e| panic!("{}: {e}", c.name));
        } else {
            let err = res.unwrap_err();
            assert!(err.contains(c.want_err), "{}: err={err}", c.name);
        }

        let mut want = vec![print_job(&p, "dnsmasq")];
        if c.dnsmasq != Some(SvcState::Off) && !c.keep_dnsmasq {
            want.extend(restart_calls(&p, "dnsmasq"));
        }
        let script_call = line(
            &p.sudo.to_string_lossy(),
            &["-n", &p.script.to_string_lossy()],
        );
        want.push(script_call.clone());
        for w in c.want_caddy {
            match *w {
                "info" => want.push(print_job(&p, "caddy")),
                "validate" => want.push(line(
                    &p.caddy.to_string_lossy(),
                    &[
                        "validate",
                        "--config",
                        &p.system_caddyfile.to_string_lossy(),
                        "--adapter",
                        "caddyfile",
                    ],
                )),
                _ => want.extend(restart_calls(&p, "caddy")),
            }
        }
        assert_eq!(r.calls(), want, "{}", c.name);
        let want_input = if c.dnsmasq == Some(SvcState::Off) {
            String::new()
        } else {
            MockDeps.resolver_list(c.domains)
        };
        assert_eq!(r.inputs(&script_call), vec![want_input], "{}", c.name);
    }

    // Apply before setup refuses without touching anything (all 3 incomplete states).
    for prep in [
        (|_| {}) as fn(&Paths),
        |p| write_file(&p.script, "#!/bin/sh\n"),
        |p| write_file(&p.system_conf, &conf_block(p)),
    ] {
        let dir_pre = tempfile::tempdir().unwrap();
        let (pp, rp) = new_mac(dir_pre.path(), false);
        prep(&pp);
        assert_eq!(
            apply(&pp, &rp, &dns_only).unwrap_err(),
            ERR_NOT_SET_UP.to_string()
        );
        assert!(rp.calls().is_empty());
        assert!(!pp.resolvers.exists());
    }

    // Apply stops at first failure (dnsmasq restart or resolver script).
    let dir_f1 = tempfile::tempdir().unwrap();
    let (pf1, rf1) = set_up_mac(dir_f1.path(), false);
    let rc1 = restart_calls(&pf1, "dnsmasq");
    rf1.fail(&rc1[1], "Could not kickstart service");
    let err_f1 = apply_with(&MockDeps, &pf1, &rf1, &dns_only).unwrap_err();
    assert!(err_f1.contains("restart dnsmasq"));
    assert_eq!(fs::read_to_string(&pf1.resolvers).unwrap(), "crm.test\n");

    let dir_f2 = tempfile::tempdir().unwrap();
    let (pf2, rf2) = set_up_mac(dir_f2.path(), false);
    rf2.fail(
        line(
            &pf2.sudo.to_string_lossy(),
            &["-n", &pf2.script.to_string_lossy()],
        ),
        "sudo: a password is required",
    );
    let err_f2 = apply_with(&MockDeps, &pf2, &rf2, &dns_only).unwrap_err();
    assert!(err_f2.contains("sudo: a password is required"));
    assert!(err_f2.contains(&pf2.resolver_dir.display().to_string()));

    // Trim long log triggers restart.
    let dir_log = tempfile::tempdir().unwrap();
    let (pl, rl) = set_up_mac(dir_log.path(), false);
    write_files_with(&MockDeps, &pl, &dns_only).unwrap();
    let log_line = "Oct  8 18:27:22 dnsmasq[1]: query[A] crm.test from 127.0.0.1\n";
    let big = format!(
        "{}FINAL_LINE\n",
        log_line.repeat((1 << 20) / log_line.len() + 10)
    );
    write_file(&pl.log, &big);
    apply_with(&MockDeps, &pl, &rl, &dns_only).unwrap();
    let trimmed = fs::read_to_string(&pl.log).unwrap();
    assert!(trimmed.len() as u64 <= dnsmasq::MAX_TAIL);
    assert!(trimmed.ends_with("FINAL_LINE\n"));
    assert!(rl.calls().contains(&restart_calls(&pl, "dnsmasq")[1]));

    // Refreshes stale menubar during apply.
    let dir_mb = tempfile::tempdir().unwrap();
    let (pmb, rmb) = set_up_mac(dir_mb.path(), false);
    write_file(&pmb.swiftc, "");
    write_file(&pmb.menubar_plist, "<plist/>");
    write_file(&pmb.menubar_bin, "old");
    write_file(
        &PathBuf::from(format!("{}.swift", pmb.menubar_bin.display())),
        "// stale\n",
    );
    rmb.set(
        line(
            &pmb.launchctl.to_string_lossy(),
            &["print", "gui/501/io.lodo.menubar"],
        ),
        "\tstate = running\n\tpid = 999\n",
    );
    apply_with(&MockDeps, &pmb, &rmb, &dns_only).unwrap();
    assert!(
        rmb.calls()
            .iter()
            .any(|c| c.contains("kickstart -k gui/501/io.lodo.menubar"))
    );

    // SetService + Caddy log error after start.
    let bind_err = "loading initial config: bind: permission denied";
    let caddy_log_text = format!(
        "{{\"level\":\"error\",\"ts\":1,\"msg\":\"older\"}}\n{{\"level\":\"error\",\"ts\":3,\"msg\":\"{bind_err}\"}}\n"
    );
    for (state, log_age, want_err) in [
        (SvcState::Running, Duration::ZERO, ""),
        (
            SvcState::Crashed,
            Duration::ZERO,
            "caddy stopped right after it started: loading initial config: bind: permission denied",
        ),
        (SvcState::Crashed, Duration::from_secs(3600), "; see "),
    ] {
        let d = tempfile::tempdir().unwrap();
        let (ps, rs) = set_up_mac(d.path(), true);
        write_file(&ps.system_caddyfile, &caddy_block(&ps));
        write_file(&ps.caddy_log, &caddy_log_text);
        let written = (SystemTime::now() + Duration::from_secs(60))
            .checked_sub(log_age)
            .unwrap();
        File::open(&ps.caddy_log)
            .unwrap()
            .set_times(FileTimes::new().set_modified(written))
            .unwrap();
        set_mock_service(&ps, &rs, "caddy", state);
        let res = set_service_with(&MockDeps, &ps, &rs, "caddy", true);
        if want_err.is_empty() {
            res.unwrap();
        } else {
            assert!(res.unwrap_err().contains(want_err));
        }
    }

    // SetService validation and dnsmasq/menubar branches.
    let ds = tempfile::tempdir().unwrap();
    let (ps2, rs2) = set_up_mac(ds.path(), true);
    write_file(&ps2.system_caddyfile, &caddy_block(&ps2));
    write_file(&ps2.swiftc, "");
    store::save(&ps2.domains_json, &dns_only).unwrap();
    assert!(set_service_with(&MockDeps, &ps2, &rs2, "nginx", false).is_err());
    set_service_with(&MockDeps, &ps2, &rs2, "dnsmasq", false).unwrap();
    set_service_with(&MockDeps, &ps2, &rs2, "dnsmasq", true).unwrap();
    set_service_with(&MockDeps, &ps2, &rs2, "caddy", false).unwrap();
    rs2.fail(
        line(
            &ps2.launchctl.to_string_lossy(),
            &["print", "gui/501/io.lodo.menubar"],
        ),
        "Could not find service",
    );
    set_service(&ps2, &rs2, "menubar", true).unwrap();
    set_service(&ps2, &rs2, "menubar", false).unwrap();
}

#[test]
fn trust_and_uninstall_flows() {
    let secure = Domain {
        name: "secure.test".into(),
        address: "127.0.1.1".into(),
        port: 3000,
        enabled: true,
        https: true,
        ..Domain::default()
    };

    // Trust failures: missing root, symlink, directory, security error.
    struct TrustFailCase {
        name: &'static str,
        prep: fn(&Paths, &FakeRunner),
        want_err: &'static str,
        want_trust_ran: bool,
    }
    let fail_preps = [
        TrustFailCase {
            name: "missing root",
            prep: |_, _| {},
            want_err: "caddy has not made its root",
            want_trust_ran: false,
        },
        TrustFailCase {
            name: "symlink root",
            prep: |p, _| {
                let other = p.caddy_root.parent().unwrap().join("other.crt");
                write_file(&other, ROOT_PEM);
                symlink(&other, &p.caddy_root).unwrap();
            },
            want_err: "not a regular file",
            want_trust_ran: false,
        },
        TrustFailCase {
            name: "dir root",
            prep: |p, _| fs::create_dir_all(&p.caddy_root).unwrap(),
            want_err: "not a regular file",
            want_trust_ran: false,
        },
        TrustFailCase {
            name: "security fails",
            prep: |p, r| {
                write_file(&p.caddy_root, ROOT_PEM);
                r.fail(trust_call(p), "authorization denied");
            },
            want_err: "authorization denied",
            want_trust_ran: true,
        },
    ];

    for c in fail_preps {
        let dir = tempfile::tempdir().unwrap();
        let (p, r) = new_mac(dir.path(), true);
        store::save(&p.domains_json, std::slice::from_ref(&secure)).unwrap();
        (c.prep)(&p, &r);
        let err = setup_with(&MockDeps, &p, &r, &mut Vec::new()).unwrap_err();
        assert_eq!(
            err.step, "Caddy's local CA trusted for HTTPS names",
            "{}",
            c.name
        );
        assert!(err.to_string().contains(c.want_err), "{}: {err}", c.name);
        assert_eq!(
            r.calls().contains(&trust_call(&p)),
            c.want_trust_ran,
            "{}",
            c.name
        );
        assert!(!p.trusted_ca.exists(), "{}", c.name);
    }

    // CA rotation untrusts old before trusting new, and sets 0644 on trusted_ca.
    let dir_rot = tempfile::tempdir().unwrap();
    let (prot, rrot) = new_mac(dir_rot.path(), true);
    store::save(&prot.domains_json, std::slice::from_ref(&secure)).unwrap();
    write_file(&prot.trusted_ca, ROOT_PEM);
    let new_pem = "-----BEGIN CERTIFICATE-----\nnew-ca\n-----END CERTIFICATE-----\n";
    write_file(&prot.caddy_root, new_pem);
    setup_with(&MockDeps, &prot, &rrot, &mut Vec::new()).unwrap();
    let calls = rrot.calls();
    let u_pos = calls
        .iter()
        .position(|c| *c == untrust_call(&prot))
        .unwrap();
    let t_pos = calls.iter().position(|c| *c == trust_call(&prot)).unwrap();
    assert!(u_pos < t_pos);
    assert_eq!(fs::read_to_string(&prot.trusted_ca).unwrap(), new_pem);
    assert_eq!(
        fs::metadata(&prot.trusted_ca).unwrap().permissions().mode() & 0o777,
        0o644
    );

    // Full setup -> uninstall restores backups, removes active lo0 aliases, keeps domains.json.
    let dir_un = tempfile::tempdir().unwrap();
    let (pun, run_setup) = new_mac(dir_un.path(), true);
    write_file(
        &pun.system_caddyfile,
        "example.com {\n\trespond \"hi\"\n}\n",
    );
    setup_with(&MockDeps, &pun, &run_setup, &mut Vec::new()).unwrap();
    store::save(
        &pun.domains_json,
        &[Domain {
            name: "crm.test".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            ..Domain::default()
        }],
    )
    .unwrap();
    write_file(&pun.script, "#!/bin/sh\n");

    let run_un = FakeRunner::new();
    run_un.set(
        line(&pun.ifconfig.to_string_lossy(), &["lo0"]),
        "\tinet 127.0.1.1 netmask 0xff000000\n\tinet 127.0.1.3 netmask 0xff000000\n",
    );
    let mut un_out = Vec::new();
    uninstall_with(&MockDeps, &pun, &run_un, &mut un_out).unwrap();
    let un_str = String::from_utf8(un_out).unwrap();

    assert_eq!(
        run_un.calls(),
        uninstall_calls(&pun, true, true, &["127.0.1.1", "127.0.1.3"])
    );
    assert_eq!(
        fs::read_to_string(&pun.system_conf).unwrap(),
        "# dnsmasq\n#port=5353\n"
    );
    assert_eq!(
        fs::read_to_string(&pun.system_caddyfile).unwrap(),
        "example.com {\n\trespond \"hi\"\n}\n"
    );
    assert!(!pun.system_conf_backup.exists());
    assert!(!pun.system_caddyfile_backup.exists());
    assert_eq!(store::load(&pun.domains_json).unwrap().len(), 1);
    assert!(un_str.contains("sudo brew services start dnsmasq"));

    // Uninstall untrust warning when security fails keeps trusted_ca and continues.
    let dir_uw = tempfile::tempdir().unwrap();
    let (puw, ruw) = new_mac(dir_uw.path(), true);
    write_file(&puw.system_caddyfile, &caddy_block(&puw));
    write_file(&puw.trusted_ca, ROOT_PEM);
    ruw.fail(untrust_call(&puw), "item not found");
    let mut uw_out = Vec::new();
    uninstall_with(&MockDeps, &puw, &ruw, &mut uw_out).unwrap();
    let uw_str = String::from_utf8(uw_out).unwrap();
    assert!(puw.trusted_ca.exists());
    assert!(uw_str.contains("! Caddy's local CA no longer trusted:"));
    assert!(uw_str.contains(&format!(
        "remove it by hand: sudo security remove-trusted-cert -d {}",
        puw.trusted_ca.display()
    )));

    // Uninstall without backups & menubar stop failure.
    let dir_nb = tempfile::tempdir().unwrap();
    let (pnb, rnb) = new_mac(dir_nb.path(), true);
    write_file(
        &pnb.system_conf,
        &format!("server=1.1.1.1\n\n{}", conf_block(&pnb)),
    );
    write_file(&pnb.system_caddyfile, &caddy_block(&pnb));
    uninstall_with(&MockDeps, &pnb, &rnb, &mut Vec::new()).unwrap();
    assert_eq!(
        fs::read_to_string(&pnb.system_conf).unwrap(),
        "server=1.1.1.1\n"
    );
    assert!(!pnb.system_caddyfile.exists());

    let dir_mbf = tempfile::tempdir().unwrap();
    let (pmbf, rmbf) = new_mac(dir_mbf.path(), false);
    write_file(&pmbf.menubar_plist, "<plist/>");
    let launchctl_s = pmbf.launchctl.to_string_lossy().into_owned();
    rmbf.set(
        line(&launchctl_s, &["print", "gui/501/io.lodo.menubar"]),
        "\tstate = running\n\tpid = 999\n",
    );
    rmbf.fail(
        line(&launchctl_s, &["bootout", "gui/501/io.lodo.menubar"]),
        "bootout failed",
    );
    let mb_err = uninstall_with(&MockDeps, &pmbf, &rmbf, &mut Vec::new()).unwrap_err();
    assert_eq!(mb_err.step, "menu bar icon stopped");

    // Public entrypoints smoke check.
    let dir_pub = tempfile::tempdir().unwrap();
    let (ppub, rpub) = new_mac(dir_pub.path(), false);
    setup(&ppub, &rpub, &mut Vec::new()).unwrap();
    write_file(&ppub.script, &script(&ppub));
    assert!(setup_done(&ppub));
    assert_eq!(generated(&ppub, &[]).len(), 3);
    let _ = write_files(&ppub, &[]).unwrap();
    apply(&ppub, &rpub, &[]).unwrap();
    uninstall(&ppub, &rpub, &mut Vec::new()).unwrap();
}
