use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::thread;
use std::time::Duration;

use nix::unistd::Uid;

use super::*;
use crate::check::{Check, ProbeResult};
use crate::ls::LsReport;
use crate::paths::Paths;
use crate::run::{self, FakeRunner, RunError, Runner};
use crate::store::Domain;

fn to_args(items: &[&str]) -> Vec<String> {
    items.iter().map(|s| (*s).to_string()).collect()
}

fn write_file(path: &Path, content: &str) {
    if let Some(dir) = path.parent() {
        fs::create_dir_all(dir).unwrap();
    }
    fs::write(path, content).unwrap();
}

fn shop_tree() -> Vec<(&'static str, &'static str)> {
    vec![
        (".git/HEAD", "ref: refs/heads/master\n"),
        ("package.json", r#"{"scripts": {"dev": "turbo dev"}}"#),
        ("pnpm-lock.yaml", ""),
        ("pnpm-workspace.yaml", "packages: [apps/*]\n"),
        (
            "apps/web/package.json",
            r#"{"scripts": {"dev": "next dev -p 3001"}}"#,
        ),
        (
            "apps/api/package.json",
            r#"{"scripts": {"dev": "nest start --watch"}}"#,
        ),
    ]
}

fn write_tree(base: &Path, folder: &str, files: &[(&str, &str)]) -> PathBuf {
    let root = base.join(folder);
    for (rel, content) in files {
        write_file(&root.join(rel), content);
    }
    root
}

fn listed_shop(root: &Path) -> Vec<Domain> {
    vec![
        Domain {
            name: "shop.lodo".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            root: root.to_string_lossy().into_owned(),
            ..Domain::default()
        },
        Domain {
            name: "web.shop.lodo".into(),
            address: "127.0.1.1".into(),
            port: 3001,
            enabled: true,
            https: true,
            ..Domain::default()
        },
        Domain {
            name: "docs.shop.lodo".into(),
            address: "127.0.1.1".into(),
            port: 3002,
            ..Domain::default()
        },
        Domain {
            name: "crm.lodo".into(),
            address: "127.0.1.2".into(),
            port: 3001,
            enabled: true,
            ..Domain::default()
        },
    ]
}

/// Sets up a fake Mac that satisfies `system::setup_done`, `system::caddy_set_up`,
/// and `check::Env::prerequisites` (checks 1..=5).
fn set_up_ready_mac(p: &Paths, r: &FakeRunner) {
    write_file(&p.brew, "");
    write_file(&p.dnsmasq, "");
    write_file(&p.caddy, "");
    write_file(&p.system_conf, &system::rewrite_system_conf("", p));
    write_file(
        &p.system_caddyfile,
        &system::rewrite_system_caddyfile("", p),
    );
    write_file(&p.script, &system::script(p));
    fs::set_permissions(&p.script, fs::Permissions::from_mode(0o755)).unwrap();
    write_file(&p.launch_agents.join("sh.brew.dnsmasq.plist"), "");
    write_file(&p.launch_agents.join("sh.brew.caddy.plist"), "");

    let launchctl = p.launchctl.to_string_lossy().into_owned();
    for label in system::DNS_SYSTEM_LABELS {
        r.fail(
            run::line(&launchctl, &["print", &format!("system/{label}")]),
            "Could not find service",
        );
    }
    r.set(
        run::line(&launchctl, &["print", "gui/501/sh.brew.dnsmasq"]),
        "\tstate = running\n\tpid = 42\n",
    );
    r.set(
        run::line(&launchctl, &["print", "gui/501/sh.brew.caddy"]),
        "\tstate = running\n\tpid = 43\n",
    );
    r.fail(
        run::line(&launchctl, &["print", "gui/501/io.lodo.menubar"]),
        "Could not find service",
    );

    let mut lo0 = String::new();
    for i in store::OWN_FIRST..=store::OWN_LAST {
        let _ = writeln!(lo0, "\tinet {} netmask 0xff000000", store::own_address(i));
    }
    r.set(run::line(&p.ifconfig.to_string_lossy(), &["lo0"]), lo0);
}

fn new_test_app<'a>(p: Paths, r: &'a dyn Runner, cwd: PathBuf) -> App<'a> {
    let mut app = App::new(p, r);
    app.cwd = cwd;
    app.root_uid = Uid::current().as_raw();
    app.timeout = Some(Duration::from_millis(20));
    app.look_path = Box::new(|file| Ok(PathBuf::from(format!("/opt/bin/{file}"))));
    app.environ = Some(vec![("PATH".into(), "/usr/bin:/bin".into())]);
    app
}

/// Wrapper around `FakeRunner` that emulates `sudo -n install ... <staged> <target>` and
/// `sudo -n [-k] <script>` writing resolver files so `system::setup` and `system::apply`
/// satisfy `prerequisites` in hermetic unit tests.
struct SetupFake {
    fake: FakeRunner,
    paths: Paths,
}

impl Runner for SetupFake {
    fn run(
        &self,
        timeout: Option<Duration>,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError> {
        if Path::new(name) == self.paths.sudo
            && args.len() == 10
            && args[0] == "-n"
            && Path::new(args[1]) == self.paths.install
            && let Ok(data) = fs::read(args[8])
        {
            let target = Path::new(args[9]);
            if let Some(dir) = target.parent() {
                let _ = fs::create_dir_all(dir);
            }
            let _ = fs::write(target, data);
            if let Ok(mode) = u32::from_str_radix(args[7], 8) {
                let _ = fs::set_permissions(target, fs::Permissions::from_mode(mode));
            }
        }
        self.fake.run(timeout, name, args)
    }

    fn run_input(
        &self,
        timeout: Option<Duration>,
        stdin: &str,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError> {
        let res = self.fake.run_input(timeout, stdin, name, args)?;
        let script_s = self.paths.script.to_string_lossy();
        if Path::new(name) == self.paths.sudo
            && (args == ["-n", script_s.as_ref()] || args == ["-n", "-k", script_s.as_ref()])
        {
            let _ = fs::create_dir_all(&self.paths.resolver_dir);
            if let Ok(entries) = fs::read_dir(&self.paths.resolver_dir) {
                for entry in entries.flatten() {
                    let _ = fs::remove_file(entry.path());
                }
            }
            for domain_name in stdin.lines().filter(|l| !l.is_empty()) {
                let _ = fs::write(
                    self.paths.resolver_dir.join(domain_name),
                    system::resolver_file(),
                );
            }
        }
        Ok(res)
    }

    fn run_tty(&self, name: &str, args: &[&str]) -> Result<(), RunError> {
        self.fake.run_tty(name, args)
    }

    fn replace(
        &self,
        dir: &Path,
        name: &str,
        args: &[String],
        env: &[(String, String)],
    ) -> Result<(), RunError> {
        self.fake.replace(dir, name, args, env)
    }
}

#[test]
fn dispatch_exit_codes_and_root_guard() {
    struct Case<'a> {
        name: &'a str,
        args: &'a [&'a str],
        want_code: u8,
        want_stdout: &'a str,
        want_stderr: &'a str,
    }

    let cases = [
        Case {
            name: "version",
            args: &["version"],
            want_code: 0,
            want_stdout: crate::VERSION,
            want_stderr: "",
        },
        Case {
            name: "--version",
            args: &["--version"],
            want_code: 0,
            want_stdout: crate::VERSION,
            want_stderr: "",
        },
        Case {
            name: "help",
            args: &["help"],
            want_code: 0,
            want_stdout: "Usage:",
            want_stderr: "",
        },
        Case {
            name: "no command",
            args: &[],
            want_code: 0,
            want_stdout: "lodo ls",
            want_stderr: "",
        },
        Case {
            name: "unknown command",
            args: &["frobnicate"],
            want_code: 2,
            want_stdout: "",
            want_stderr: r#"lodo: unknown command "frobnicate""#,
        },
        Case {
            name: "extra argument on doctor",
            args: &["doctor", "now"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: doctor takes no arguments",
        },
        Case {
            name: "scan unknown flag",
            args: &["scan", "--yaml"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: scan: flag provided but not defined: -yaml",
        },
        Case {
            name: "scan unexpected argument",
            args: &["scan", "now"],
            want_code: 2,
            want_stdout: "",
            want_stderr: r#"lodo: scan: unexpected argument "now""#,
        },
        Case {
            name: "dev with scan flag",
            args: &["dev", "--json"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: dev: flag provided but not defined: -json",
        },
        Case {
            name: "dev with command after --",
            args: &["dev", "--", "next", "dev"],
            want_code: 2,
            want_stdout: "",
            want_stderr: r#"lodo: dev: unexpected argument "next""#,
        },
        Case {
            name: "prune unexpected argument",
            args: &["prune", "now"],
            want_code: 2,
            want_stdout: "",
            want_stderr: r#"lodo: prune: unexpected argument "now""#,
        },
        Case {
            name: "prune unknown flag",
            args: &["prune", "--all"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: prune: flag provided but not defined: -all",
        },
        Case {
            name: "exec without command",
            args: &["exec"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: exec needs a command to run",
        },
        Case {
            name: "exec with only --",
            args: &["exec", "--"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: exec needs a command to run",
        },
        Case {
            name: "menubar unexpected argument",
            args: &["menubar", "now"],
            want_code: 2,
            want_stdout: "",
            want_stderr: r#"lodo: menubar: unexpected argument "now""#,
        },
        Case {
            name: "menubar unknown flag",
            args: &["menubar", "--start"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: menubar: flag provided but not defined: -start",
        },
        Case {
            name: "add without name",
            args: &["add"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: add: missing domain name",
        },
        Case {
            name: "set conflicting --on --off",
            args: &["set", "flowy", "--on", "--off"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: set: cannot pass both --on and --off",
        },
        Case {
            name: "set conflicting --https --no-https",
            args: &["set", "flowy", "--https", "--no-https"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: set: cannot pass both --https and --no-https",
        },
        Case {
            name: "service unknown service",
            args: &["service", "nginx", "on"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: service: unknown service",
        },
        Case {
            name: "service bad state",
            args: &["service", "caddy", "restart"],
            want_code: 2,
            want_stdout: "",
            want_stderr: "lodo: service: expected on or off",
        },
    ];

    for tc in cases {
        let mut out = Vec::new();
        let mut err = Vec::new();
        let code = dispatch_with_euid(&to_args(tc.args), false, &mut out, &mut err);
        let stdout_s = String::from_utf8_lossy(&out);
        let stderr_s = String::from_utf8_lossy(&err);
        assert_eq!(code, tc.want_code, "case {}", tc.name);
        assert!(
            stdout_s.contains(tc.want_stdout),
            "case {}: stdout = {stdout_s:?}, want {want:?}",
            tc.name,
            want = tc.want_stdout
        );
        assert!(
            stderr_s.contains(tc.want_stderr),
            "case {}: stderr = {stderr_s:?}, want {want:?}",
            tc.name,
            want = tc.want_stderr
        );
    }

    // Running as root returns 2 for system commands.
    let mut out = Vec::new();
    let mut err = Vec::new();
    let root_code = dispatch_with_euid(&to_args(&["doctor"]), true, &mut out, &mut err);
    assert_eq!(root_code, 2);
    assert!(String::from_utf8_lossy(&err).contains("run lodo as your user, not with sudo"));

    // Invalid --name on dev returns 2 via App::dispatch.
    let tmp = tempfile::tempdir().unwrap();
    let r = FakeRunner::new();
    let app = new_test_app(Paths::for_test(tmp.path()), &r, tmp.path().to_path_buf());
    let mut out2 = Vec::new();
    let mut err2 = Vec::new();
    let code2 = app.dispatch(&to_args(&["dev", "--name", "a.b.c"]), &mut out2, &mut err2);
    assert_eq!(code2, 2);
    assert!(
        String::from_utf8_lossy(&err2).contains("✗ --name a.b.c: a project's name has two labels")
    );
}

#[test]
fn format_checks_and_results_match_go_goldens_byte_for_byte() {
    let checks_golden = include_str!("../../tests/testdata/cli/TestFormatChecks.golden");
    let got_checks = format_checks(&[
        Check {
            id: 1,
            name: "dnsmasq".into(),
            ok: true,
            detail: "running as tester, pid 42".into(),
            ..Check::default()
        },
        Check {
            id: 2,
            name: "dnsmasq config".into(),
            detail: "conf-file points at /old.conf".into(),
            fix: "lodo setup".into(),
            ..Check::default()
        },
        Check {
            id: 8,
            name: "caddy".into(),
            skipped: true,
            detail: "no enabled domain has a port".into(),
            ..Check::default()
        },
    ]);
    assert_eq!(got_checks, checks_golden);

    let none_golden = include_str!("../../tests/testdata/cli/TestFormatResults/none.golden");
    assert_eq!(format_results(&[]), none_golden);

    let mixed_golden = include_str!("../../tests/testdata/cli/TestFormatResults/mixed.golden");
    let got_mixed = format_results(&[
        ProbeResult {
            name: "crm.lodo".into(),
            address: "127.0.1.1".into(),
            direct: true,
            system: true,
            ..ProbeResult::default()
        },
        ProbeResult {
            name: "dashboard.crm.lodo".into(),
            address: "127.0.1.1".into(),
            port: 3000,
            direct: true,
            system: true,
            down: true,
            detail: "app down: nothing answers on 127.0.1.1:3000".into(),
            ..ProbeResult::default()
        },
        ProbeResult {
            name: "docs.crm.lodo".into(),
            address: "127.0.1.1".into(),
            port: 3001,
            direct: true,
            system: true,
            detail: "an app listens on 127.0.0.1:3001, not on the name's address: start it with lodo dev in its project".into(),
            ..ProbeResult::default()
        },
        ProbeResult {
            name: "flowy.lodo".into(),
            address: "127.0.1.3".into(),
            direct: true,
            detail: "macOS: no address".into(),
            ..ProbeResult::default()
        },
        ProbeResult {
            name: "secure.crm.lodo".into(),
            address: "127.0.1.1".into(),
            port: 3004,
            direct: true,
            system: true,
            http: true,
            secure: true,
            https: true,
            ..ProbeResult::default()
        },
        ProbeResult {
            name: "shop.crm.lodo".into(),
            address: "127.0.1.1".into(),
            port: 3005,
            direct: true,
            system: true,
            http: true,
            secure: true,
            detail: "https: Caddy's certificate isn't trusted: run lodo setup".into(),
            ..ProbeResult::default()
        },
    ]);
    assert_eq!(got_mixed, mixed_golden);

    let up = ProbeResult {
        name: "crm.lodo".into(),
        direct: true,
        system: true,
        ..ProbeResult::default()
    };
    let down = ProbeResult {
        name: "dashboard.crm.lodo".into(),
        port: 3000,
        direct: true,
        system: true,
        down: true,
        ..ProbeResult::default()
    };
    let broken = ProbeResult {
        name: "flowy.lodo".into(),
        direct: true,
        ..ProbeResult::default()
    };
    assert_eq!(probe_code(&[]), 0);
    assert_eq!(probe_code(std::slice::from_ref(&up)), 0);
    assert_eq!(probe_code(&[up, down.clone()]), 0);
    assert_eq!(probe_code(&[down, broken]), 1);
}

#[test]
fn doctor_apply_menubar_setup_and_uninstall() {
    // 1. Doctor on un-setup Mac returns 1 and prints checks 1..=8.
    let tmp = tempfile::tempdir().unwrap();
    let p = Paths::for_test(tmp.path());
    let r = FakeRunner::new();
    let app = new_test_app(p.clone(), &r, tmp.path().to_path_buf());

    let mut out = Vec::new();
    let mut err = Vec::new();
    assert_eq!(app.dispatch(&to_args(&["doctor"]), &mut out, &mut err), 1);
    let doctor_s = String::from_utf8_lossy(&out);
    for id in 1..=8 {
        assert!(
            doctor_s.contains(&format!(" {id} ")),
            "missing check {id} in:\n{doctor_s}"
        );
    }

    // 2. Apply before setup fails with ERR_NOT_SET_UP.
    out.clear();
    err.clear();
    assert_eq!(app.dispatch(&to_args(&["apply"]), &mut out, &mut err), 1);
    assert!(String::from_utf8_lossy(&err).contains(system::ERR_NOT_SET_UP));

    // 3. Menubar before setup fails with ERR_NOT_SET_UP.
    out.clear();
    err.clear();
    assert_eq!(app.dispatch(&to_args(&["menubar"]), &mut out, &mut err), 1);
    assert!(String::from_utf8_lossy(&err).contains(system::ERR_NOT_SET_UP));

    // 4. Apply with no enabled domains succeeds.
    set_up_ready_mac(&p, &r);
    out.clear();
    err.clear();
    assert_eq!(app.dispatch(&to_args(&["apply"]), &mut out, &mut err), 0);
    assert_eq!(String::from_utf8_lossy(&out), "no enabled domains\n");

    // 5. Menubar start & --stop.
    write_file(&p.swiftc, "");
    out.clear();
    err.clear();
    assert_eq!(app.dispatch(&to_args(&["menubar"]), &mut out, &mut err), 0);
    assert_eq!(String::from_utf8_lossy(&out), "menubar running\n");
    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(&to_args(&["menubar", "--stop"]), &mut out, &mut err),
        0
    );
    assert_eq!(String::from_utf8_lossy(&out), "menubar stopped\n");

    // 6. Setup auto-starts prebuilt Homebrew menubar binary when menubar_plist is absent.
    let tmp_brew = tempfile::tempdir().unwrap();
    let p_brew = Paths::for_test(tmp_brew.path());
    let sf = SetupFake {
        fake: FakeRunner::new(),
        paths: p_brew.clone(),
    };
    set_up_ready_mac(&p_brew, &sf.fake);
    write_file(&p_brew.brew_menubar_bin, "mach-o");
    let app_brew = new_test_app(p_brew.clone(), &sf, tmp_brew.path().to_path_buf());
    out.clear();
    err.clear();
    assert_eq!(
        app_brew.dispatch(&to_args(&["setup"]), &mut out, &mut err),
        0,
        "stderr: {}\nstdout: {}",
        String::from_utf8_lossy(&err),
        String::from_utf8_lossy(&out)
    );
    assert!(p_brew.menubar_plist.exists());
    assert!(String::from_utf8_lossy(&out).contains("✓ menu bar icon started"));

    // 6b. Setup warns without failing when menubar cannot start (e.g., headless SSH).
    let tmp_headless = tempfile::tempdir().unwrap();
    let p_headless = Paths::for_test(tmp_headless.path());
    let sf_headless = SetupFake {
        fake: FakeRunner::new(),
        paths: p_headless.clone(),
    };
    set_up_ready_mac(&p_headless, &sf_headless.fake);
    write_file(&p_headless.brew_menubar_bin, "mach-o");
    sf_headless.fake.fail(
        run::line(
            &p_headless.launchctl.to_string_lossy(),
            &[
                "bootstrap",
                "gui/501",
                &p_headless.menubar_plist.to_string_lossy(),
            ],
        ),
        "Bootstrap failed: 5: Input/output error",
    );
    let app_headless = new_test_app(p_headless, &sf_headless, tmp_headless.path().to_path_buf());
    out.clear();
    err.clear();
    assert_eq!(
        app_headless.dispatch(&to_args(&["setup"]), &mut out, &mut err),
        0,
        "stderr: {}\nstdout: {}",
        String::from_utf8_lossy(&err),
        String::from_utf8_lossy(&out)
    );
    assert!(
        String::from_utf8_lossy(&err).contains("warn  menubar: start menubar:"),
        "expected menubar warning on stderr, got: {}",
        String::from_utf8_lossy(&err)
    );
    assert!(String::from_utf8_lossy(&out).contains("✓ 1 dnsmasq"));

    // 7. Apply fails when sudo script fails, and probe fails on unreachable DNS.
    let tmp_fail = tempfile::tempdir().unwrap();
    let p_fail = Paths::for_test(tmp_fail.path());
    let r_fail = FakeRunner::new();
    set_up_ready_mac(&p_fail, &r_fail);
    r_fail.fail(
        run::line(
            &p_fail.sudo.to_string_lossy(),
            &["-n", &p_fail.script.to_string_lossy()],
        ),
        "sudo: a password is required",
    );
    let mut app_fail = new_test_app(p_fail.clone(), &r_fail, tmp_fail.path().to_path_buf());
    app_fail.dns = "127.0.0.1:1".into();
    out.clear();
    err.clear();
    assert_eq!(
        app_fail.dispatch(&to_args(&["apply"]), &mut out, &mut err),
        1
    );
    assert!(String::from_utf8_lossy(&err).contains("sudo: a password is required"));

    let r_probe = FakeRunner::new();
    set_up_ready_mac(&p_fail, &r_probe);
    store::save(
        &p_fail.domains_json,
        &[Domain {
            name: "crm.lodo".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            ..Domain::default()
        }],
    )
    .unwrap();
    let mut app_probe = new_test_app(p_fail, &r_probe, tmp_fail.path().to_path_buf());
    app_probe.dns = "127.0.0.1:1".into();
    out.clear();
    err.clear();
    assert_eq!(
        app_probe.dispatch(&to_args(&["apply"]), &mut out, &mut err),
        1
    );
    assert!(String::from_utf8_lossy(&out).contains("crm.lodo"));

    // 8. Uninstall succeeds cleanly.
    out.clear();
    err.clear();
    assert_eq!(
        app_probe.dispatch(&to_args(&["uninstall"]), &mut out, &mut err),
        0
    );
}

#[test]
fn scan_matches_golden_masks_passwords_and_checks_taken_names() {
    let scan_golden = include_str!("../../tests/testdata/cli/TestApp_Scan.golden");
    let tmp = tempfile::tempdir().unwrap();
    let root = write_tree(
        tmp.path(),
        "shop",
        &[
            (".git/HEAD", "ref: refs/heads/master\n"),
            ("package.json", r#"{"scripts": {"dev": "turbo dev"}}"#),
            ("pnpm-workspace.yaml", "packages: [apps/*]\n"),
            (
                "apps/web/package.json",
                "{\n  \"scripts\": {\"dev\": \"next dev -p 3001\"}\n}\n",
            ),
            (
                "apps/api/package.json",
                r#"{"scripts": {"dev": "nest start --watch"}}"#,
            ),
            (
                "apps/web/.env",
                "API_URL=http://localhost:3001\nDATABASE_URL=postgres://u:hunter2@localhost:5432/shop\n",
            ),
            (".env", "DOCKER_HOST_IP=127.0.1.1\n"),
            (
                "docker/compose.dev.yaml",
                r#"services: {db: {image: postgres, ports: ["${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"]}}"#,
            ),
        ],
    );

    let sys_tmp = tempfile::tempdir().unwrap();
    let p = Paths::for_test(sys_tmp.path());
    let r = FakeRunner::new();
    let mut app = new_test_app(p.clone(), &r, root.join("apps/web"));

    let mut out = Vec::new();
    let mut err = Vec::new();
    assert_eq!(app.dispatch(&to_args(&["scan"]), &mut out, &mut err), 0);
    let raw_out = String::from_utf8_lossy(&out);
    assert!(!raw_out.contains("hunter2"), "leaked password:\n{raw_out}");
    let normalized = raw_out.replace(&root.to_string_lossy().into_owned(), "<root>");
    assert_eq!(normalized, scan_golden);

    // --json output also masks password and writes nothing to domains.json.
    app.cwd = root.clone();
    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(&to_args(&["scan", "--json"]), &mut out, &mut err),
        0
    );
    let json_s = String::from_utf8_lossy(&out);
    assert!(!json_s.contains("hunter2"));
    let parsed: serde_json::Value = serde_json::from_str(&json_s).unwrap();
    assert_eq!(parsed["add"].as_array().unwrap().len(), 3);
    assert_eq!(parsed["env"].as_array().unwrap().len(), 2);
    assert!(!p.domains_json.exists());

    // --name store & --name store.custom & invalid 3-label --name.
    out.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["scan", "--json", "--name", "store"]),
            &mut out,
            &mut err
        ),
        0
    );
    let store_json: serde_json::Value = serde_json::from_slice(&out).unwrap();
    assert_eq!(store_json["name"], "store.lodo");
    assert_eq!(
        store_json["add"][0]["root"],
        root.to_string_lossy().as_ref()
    );

    out.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["scan", "--json", "--name", "store.custom"]),
            &mut out,
            &mut err
        ),
        0
    );
    let custom_json: serde_json::Value = serde_json::from_slice(&out).unwrap();
    assert_eq!(custom_json["name"], "store.custom");

    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["scan", "--name", "web.store.lodo"]),
            &mut out,
            &mut err
        ),
        2
    );

    // Outside a project returns 1.
    app.cwd = tmp.path().to_path_buf();
    out.clear();
    err.clear();
    assert_eq!(app.dispatch(&to_args(&["scan"]), &mut out, &mut err), 1);
    assert!(String::from_utf8_lossy(&err).contains("not in a project"));

    // Taken vs gone vs empty root under --name shop.
    let other_tmp = tempfile::tempdir().unwrap();
    let other = write_tree(other_tmp.path(), "shop", &shop_tree());
    let gone_dir = other_tmp.path().join("moved");

    for (owner_root, want_line, is_taken) in [
        (
            other.to_string_lossy().into_owned(),
            format!(
                "note  shop.lodo belongs to {}: give this project another name\n",
                other.display()
            ),
            true,
        ),
        (
            gone_dir.to_string_lossy().into_owned(),
            "save  this folder as shop.lodo's project\n".to_string(),
            false,
        ),
        (
            String::new(),
            "save  this folder as shop.lodo's project\n".to_string(),
            false,
        ),
    ] {
        let case_tmp = tempfile::tempdir().unwrap();
        let cp = Paths::for_test(case_tmp.path());
        store::save(
            &cp.domains_json,
            &[Domain {
                name: "shop.lodo".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                root: owner_root,
                ..Domain::default()
            }],
        )
        .unwrap();
        let cr = FakeRunner::new();
        let capp = new_test_app(cp, &cr, root.clone());
        out.clear();
        err.clear();
        assert_eq!(
            capp.dispatch(&to_args(&["scan", "--name", "shop"]), &mut out, &mut err),
            0
        );
        let text = String::from_utf8_lossy(&out);
        assert!(
            text.contains(&want_line),
            "missing {want_line:?} in:\n{text}"
        );
        if is_taken {
            assert!(!text.contains("add   "));
            assert!(!text.contains("run lodo dev"));
        }
    }
}

#[test]
fn prune_worktree_noop_and_concurrency() {
    let proj_tmp = tempfile::tempdir().unwrap();
    let main_dir = write_tree(proj_tmp.path(), "main", &shop_tree());
    let make_wt = |folder: &str| -> PathBuf {
        let mut files: Vec<(&str, String)> = shop_tree()
            .into_iter()
            .filter(|(k, _)| *k != ".git/HEAD")
            .map(|(k, v)| (k, v.to_string()))
            .collect();
        files.push((
            ".git",
            format!(
                "gitdir: {}\n",
                main_dir.join(".git/worktrees").join(folder).display()
            ),
        ));
        let root = proj_tmp.path().join(folder);
        for (rel, content) in files {
            write_file(&root.join(rel), &content);
        }
        root
    };
    let live_wt = make_wt("wt-live");
    let active_wt = make_wt("wt-active");
    let gone_wt = proj_tmp.path().join("wt-gone");

    let initial = vec![
        Domain {
            name: "shop.lodo".into(),
            address: "127.0.1.1".into(),
            port: 3000,
            enabled: true,
            root: main_dir.to_string_lossy().into_owned(),
            ..Domain::default()
        },
        Domain {
            name: "shop2.lodo".into(),
            address: "127.0.1.2".into(),
            enabled: true,
            root: gone_wt.to_string_lossy().into_owned(),
            ..Domain::default()
        },
        Domain {
            name: "web.shop2.lodo".into(),
            address: "127.0.1.2".into(),
            port: 3001,
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "shop3.lodo".into(),
            address: "127.0.1.3".into(),
            enabled: true,
            root: live_wt.to_string_lossy().into_owned(),
            ..Domain::default()
        },
        Domain {
            name: "web.shop3.lodo".into(),
            address: "127.0.1.3".into(),
            port: 3001,
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "shop4.lodo".into(),
            address: "127.0.1.4".into(),
            enabled: true,
            root: active_wt.to_string_lossy().into_owned(),
            ..Domain::default()
        },
        Domain {
            name: "web.shop4.lodo".into(),
            address: "127.0.1.4".into(),
            port: 3001,
            enabled: true,
            ..Domain::default()
        },
        Domain {
            name: "manual.lodo".into(),
            address: "127.0.1.5".into(),
            enabled: true,
            ..Domain::default()
        },
    ];

    // 1. Bare prune vs --worktree in subfolder vs --worktree in main checkout.
    for (cwd, wt_flag, want_out, want_left, want_removed_hooks) in [
        (
            active_wt.clone(),
            false,
            format!(
                "removed shop2.lodo ({})\nremoved web.shop2.lodo\n",
                gone_wt.display()
            ),
            vec![
                "manual.lodo",
                "shop.lodo",
                "shop3.lodo",
                "web.shop3.lodo",
                "shop4.lodo",
                "web.shop4.lodo",
            ],
            vec!["shop2.lodo"],
        ),
        (
            active_wt.join("apps/web"),
            true,
            format!(
                "removed shop2.lodo ({})\nremoved web.shop2.lodo\nremoved shop4.lodo ({})\nremoved web.shop4.lodo\n",
                gone_wt.display(),
                active_wt.display()
            ),
            vec!["manual.lodo", "shop.lodo", "shop3.lodo", "web.shop3.lodo"],
            vec!["shop2.lodo", "shop4.lodo"],
        ),
        (
            main_dir.clone(),
            true,
            format!(
                "removed shop2.lodo ({})\nremoved web.shop2.lodo\n",
                gone_wt.display()
            ),
            vec![
                "manual.lodo",
                "shop.lodo",
                "shop3.lodo",
                "web.shop3.lodo",
                "shop4.lodo",
                "web.shop4.lodo",
            ],
            vec!["shop2.lodo"],
        ),
    ] {
        let sys_tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(sys_tmp.path());
        let r = FakeRunner::new();
        set_up_ready_mac(&p, &r);
        store::save(&p.domains_json, &initial).unwrap();
        let port_map = BTreeMap::from([(3001u16, "127.0.1.1".to_string())]);
        for proj in ["shop.lodo", "shop2.lodo", "shop3.lodo", "shop4.lodo"] {
            crate::dev::write_hooks(&p, proj, &port_map).unwrap();
        }

        let app = new_test_app(p.clone(), &r, cwd);
        let mut out = Vec::new();
        let mut err = Vec::new();
        let args = if wt_flag {
            to_args(&["prune", "--worktree"])
        } else {
            to_args(&["prune"])
        };
        assert_eq!(app.dispatch(&args, &mut out, &mut err), 0);
        assert!(err.is_empty());
        assert_eq!(String::from_utf8_lossy(&out), want_out);

        let saved = store::load(&p.domains_json).unwrap();
        let saved_names: Vec<&str> = saved.iter().map(|d| d.name.as_str()).collect();
        assert_eq!(saved_names, want_left);
        assert_eq!(store::next_free(&saved).unwrap(), "127.0.1.2");
        for rem in want_removed_hooks {
            assert!(!p.dev_dir.join(format!("{rem}.cjs")).exists());
        }
        assert!(p.dev_dir.join("shop.lodo.cjs").exists());
    }

    // 2. No-op before setup does not create config_dir; before setup with gone folder fails cleanly.
    let noop_tmp = tempfile::tempdir().unwrap();
    let noop_p = Paths::for_test(noop_tmp.path());
    let noop_r = FakeRunner::new();
    let noop_app = new_test_app(noop_p.clone(), &noop_r, main_dir.clone());
    let mut out = Vec::new();
    let mut err = Vec::new();
    assert_eq!(
        noop_app.dispatch(&to_args(&["prune", "--worktree"]), &mut out, &mut err),
        0
    );
    assert_eq!(String::from_utf8_lossy(&out), "nothing to prune\n");
    assert!(!noop_p.config_dir.exists());

    store::save(
        &noop_p.domains_json,
        &[Domain {
            name: "gone.lodo".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            root: gone_wt.to_string_lossy().into_owned(),
            ..Domain::default()
        }],
    )
    .unwrap();
    out.clear();
    err.clear();
    assert_eq!(
        noop_app.dispatch(&to_args(&["prune"]), &mut out, &mut err),
        1
    );
    assert!(String::from_utf8_lossy(&err).contains(system::ERR_NOT_SET_UP));
    assert_eq!(store::load(&noop_p.domains_json).unwrap().len(), 1);

    // 3. Concurrent prune --worktree across two worktrees.
    let conc_tmp = tempfile::tempdir().unwrap();
    let conc_p = Paths::for_test(conc_tmp.path());
    let conc_r = FakeRunner::new();
    set_up_ready_mac(&conc_p, &conc_r);
    store::save(
        &conc_p.domains_json,
        &[
            Domain {
                name: "shop.lodo".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                root: main_dir.to_string_lossy().into_owned(),
                ..Domain::default()
            },
            Domain {
                name: "shop2.lodo".into(),
                address: "127.0.1.2".into(),
                enabled: true,
                root: live_wt.to_string_lossy().into_owned(),
                ..Domain::default()
            },
            Domain {
                name: "shop3.lodo".into(),
                address: "127.0.1.3".into(),
                enabled: true,
                root: active_wt.to_string_lossy().into_owned(),
                ..Domain::default()
            },
        ],
    )
    .unwrap();

    thread::scope(|s| {
        for wt_dir in [&live_wt, &active_wt] {
            let p_clone = conc_p.clone();
            let r_ref = &conc_r;
            let dir = wt_dir.clone();
            s.spawn(move || {
                let app = new_test_app(p_clone, r_ref, dir);
                let mut o = Vec::new();
                let mut e = Vec::new();
                assert_eq!(
                    app.dispatch(&to_args(&["prune", "--worktree"]), &mut o, &mut e),
                    0
                );
            });
        }
    });
    let after_conc = store::load(&conc_p.domains_json).unwrap();
    assert_eq!(after_conc.len(), 1);
    assert_eq!(after_conc[0].name, "shop.lodo");
}

#[test]
fn dev_golden_auto_setup_auto_register_rename_worktree_and_node_options() {
    // 1. Exact golden match for already-listed project (`TestApp_StartDev.golden`).
    let start_dev_golden = include_str!("../../tests/testdata/cli/TestApp_StartDev.golden");
    let proj_tmp = tempfile::tempdir().unwrap();
    let root = write_tree(proj_tmp.path(), "shop", &shop_tree());
    let sys_tmp = tempfile::tempdir().unwrap();
    let p = Paths::for_test(sys_tmp.path());
    let r = FakeRunner::new();
    let app = new_test_app(p.clone(), &r, root.join("apps/web"));

    let mut combined = Vec::new();
    let mut stderr_part = Vec::new();
    let mut stdout_part = Vec::new();
    app.start_dev(
        &root.join("apps/web"),
        listed_shop(&root),
        "",
        &mut stdout_part,
        &mut stderr_part,
    )
    .unwrap();
    combined.extend_from_slice(&stderr_part);
    combined.extend_from_slice(&stdout_part);
    assert_eq!(String::from_utf8_lossy(&combined), start_dev_golden);

    let hook = p.dev_dir.join("shop.lodo.cjs");
    let hook_content = fs::read_to_string(&hook).unwrap();
    assert!(hook_content.contains(r#"{"3001":"127.0.1.1"}"#));
    assert!(p.bind_hook.exists());
    let (rep_dir, rep_env) = r.replaced();
    assert_eq!(rep_dir.as_deref(), Some(root.as_path()));
    assert!(
        rep_env
            .iter()
            .any(|(k, v)| k == "DOCKER_HOST_IP" && v == "127.0.1.1")
    );
    assert!(!rep_env.iter().any(|(k, _)| k == "COMPOSE_PROJECT_NAME"));

    // 2. First-run auto-setup + auto-register + `--name` rename + second run skips setup.
    let mut full_shop = shop_tree();
    full_shop.push(("apps/api/.env", "PORT=4001\n"));
    let root2 = write_tree(proj_tmp.path(), "shop-auto", &full_shop);

    let auto_tmp = tempfile::tempdir().unwrap();
    let auto_p = Paths::for_test(auto_tmp.path());
    let sf = SetupFake {
        fake: FakeRunner::new(),
        paths: auto_p.clone(),
    };
    write_file(&auto_p.brew, "");
    write_file(&auto_p.dnsmasq, "");
    write_file(&auto_p.caddy, "");
    write_file(&auto_p.system_conf, "# dnsmasq\n");
    write_file(&auto_p.launch_agents.join("sh.brew.dnsmasq.plist"), "");
    write_file(&auto_p.launch_agents.join("sh.brew.caddy.plist"), "");
    let launchctl = auto_p.launchctl.to_string_lossy().into_owned();
    for label in system::DNS_SYSTEM_LABELS {
        sf.fake.fail(
            run::line(&launchctl, &["print", &format!("system/{label}")]),
            "not found",
        );
    }
    sf.fake.set(
        run::line(&launchctl, &["print", "gui/501/sh.brew.dnsmasq"]),
        "\tstate = running\n\tpid = 42\n",
    );
    sf.fake.set(
        run::line(&launchctl, &["print", "gui/501/sh.brew.caddy"]),
        "\tstate = running\n\tpid = 43\n",
    );
    let mut lo0 = String::new();
    for i in store::OWN_FIRST..=store::OWN_LAST {
        let _ = writeln!(lo0, "\tinet {} netmask 0xff000000", store::own_address(i));
    }
    sf.fake
        .set(run::line(&auto_p.ifconfig.to_string_lossy(), &["lo0"]), lo0);

    let auto_app = new_test_app(auto_p.clone(), &sf, root2);
    let mut out = Vec::new();
    let mut err = Vec::new();
    assert_eq!(
        auto_app.dispatch(&to_args(&["dev"]), &mut out, &mut err),
        0,
        "stderr: {}\nstdout: {}",
        String::from_utf8_lossy(&err),
        String::from_utf8_lossy(&out)
    );
    assert!(String::from_utf8_lossy(&out).contains("✓ resolver script installed\n"));
    let saved = store::load(&auto_p.domains_json).unwrap();
    assert!(store::find(&saved, "shop-auto.lodo").is_some());
    assert!(store::find(&saved, "web.shop-auto.lodo").is_some());
    assert!(store::find(&saved, "api.shop-auto.lodo").is_some());
    assert!(auto_p.dev_dir.join("shop-auto.lodo.cjs").exists());

    // Second run with `--name custom.domain` skips setup, renames project, removes old hook.
    out.clear();
    err.clear();
    assert_eq!(
        auto_app.dispatch(
            &to_args(&["dev", "--name", "custom.domain"]),
            &mut out,
            &mut err
        ),
        0,
        "stderr: {}",
        String::from_utf8_lossy(&err)
    );
    let sudo_v = run::line(&auto_p.sudo.to_string_lossy(), &["-v"]);
    assert_eq!(sf.fake.calls().iter().filter(|c| *c == &sudo_v).count(), 1);
    let renamed = store::load(&auto_p.domains_json).unwrap();
    assert!(store::find(&renamed, "web.custom.domain").is_some());
    assert!(store::find(&renamed, "shop-auto.lodo").is_none());
    assert!(!auto_p.dev_dir.join("shop-auto.lodo.cjs").exists());
    assert!(auto_p.dev_dir.join("custom.domain.cjs").exists());

    // 3. Worktree `.env` host swapping and secret redaction.
    let dotenv = "DATABASE_URL=postgres://u:hunter2@shop.lodo:5432/shop\nAPI_URL=http://web.shop.lodo\nSECRET=abc\n";
    let wt_dir = proj_tmp.path().join("wt-shop");
    for (rel, content) in shop_tree() {
        if rel != ".git/HEAD" {
            write_file(&wt_dir.join(rel), content);
        }
    }
    write_file(
        &wt_dir.join(".git"),
        &format!("gitdir: {}\n", root.join(".git/worktrees/wt").display()),
    );
    write_file(&wt_dir.join("apps/web/.env"), dotenv);

    let mut wt_domains = listed_shop(&root);
    wt_domains.push(Domain {
        name: "shop2.lodo".into(),
        address: "127.0.1.3".into(),
        enabled: true,
        root: wt_dir.to_string_lossy().into_owned(),
        ..Domain::default()
    });
    wt_domains.push(Domain {
        name: "web.shop2.lodo".into(),
        address: "127.0.1.3".into(),
        port: 3001,
        enabled: true,
        ..Domain::default()
    });

    let wt_r = FakeRunner::new();
    let wt_app = new_test_app(Paths::for_test(sys_tmp.path()), &wt_r, wt_dir.clone());
    out.clear();
    err.clear();
    wt_app
        .start_dev(&wt_dir, wt_domains, "", &mut out, &mut err)
        .unwrap();
    let wt_stdout = String::from_utf8_lossy(&out);
    assert!(
        !wt_stdout.contains("hunter2"),
        "leaked password:\n{wt_stdout}"
    );
    assert!(wt_stdout.contains("postgres://***@shop2.lodo:5432/shop"));
    let (_, wt_env) = wt_r.replaced();
    for (want_k, want_v) in [
        ("DOCKER_HOST_IP", "127.0.1.3"),
        ("COMPOSE_PROJECT_NAME", "shop2"),
        ("DATABASE_URL", "postgres://u:hunter2@shop2.lodo:5432/shop"),
        ("API_URL", "http://web.shop2.lodo"),
    ] {
        assert!(
            wt_env.iter().any(|(k, v)| k == want_k && v == want_v),
            "missing {want_k}={want_v} in {wt_env:?}"
        );
    }
    assert_eq!(
        fs::read_to_string(wt_dir.join("apps/web/.env")).unwrap(),
        dotenv
    );

    // 4. npm and pnpm node-options reading and error propagation.
    let npm_files: Vec<(&str, &str)> = shop_tree()
        .into_iter()
        .map(|(k, v)| {
            if k == "pnpm-lock.yaml" {
                ("package-lock.json", "{}")
            } else {
                (k, v)
            }
        })
        .collect();
    let npm_root = write_tree(proj_tmp.path(), "npm-shop", &npm_files);

    for (proj_root, pm, flag, setting, want_prefix) in [
        (
            &root,
            "pnpm",
            "--dir",
            "--max-old-space-size=4096\n",
            "--max-old-space-size=4096 ",
        ),
        (&root, "pnpm", "--dir", "undefined\n", ""),
        (
            &npm_root,
            "npm",
            "--prefix",
            "--max-old-space-size=2048\n",
            "--max-old-space-size=2048 ",
        ),
        (&npm_root, "npm", "--prefix", "null\n", ""),
    ] {
        let nr = FakeRunner::new();
        nr.set(
            run::line(
                &format!("/opt/bin/{pm}"),
                &[
                    flag,
                    &proj_root.to_string_lossy(),
                    "config",
                    "get",
                    "node-options",
                ],
            ),
            setting,
        );
        let np = Paths::for_test(tempfile::tempdir().unwrap().path());
        let napp = new_test_app(np.clone(), &nr, proj_root.clone());
        let mut o = Vec::new();
        let mut e = Vec::new();
        napp.start_dev(proj_root, listed_shop(proj_root), "", &mut o, &mut e)
            .unwrap();
        let (_, env_pairs) = nr.replaced();
        let node_opt = env_pairs
            .iter()
            .find(|(k, _)| k == "NODE_OPTIONS")
            .map(|(_, v)| v.as_str())
            .unwrap();
        let cfg_key = format!("{pm}_config_node_options");
        let cfg_opt = env_pairs
            .iter()
            .find(|(k, _)| k == &cfg_key)
            .map(|(_, v)| v.as_str())
            .unwrap();
        assert_eq!(node_opt, cfg_opt);
        assert!(node_opt.starts_with(want_prefix));
        assert!(!node_opt.contains("null") && !node_opt.contains("undefined"));
        assert!(node_opt.contains(&format!(
            r#"--require "{}""#,
            np.dev_dir.join("shop.lodo.cjs").display()
        )));
    }

    // pnpm config error stops before Replace.
    let err_r = FakeRunner::new();
    err_r.fail(
        run::line(
            "/opt/bin/pnpm",
            &[
                "--dir",
                &root.to_string_lossy(),
                "config",
                "get",
                "node-options",
            ],
        ),
        "ERR_PNPM_BAD_CONFIG",
    );
    let err_app = new_test_app(
        Paths::for_test(tempfile::tempdir().unwrap().path()),
        &err_r,
        root.clone(),
    );
    let err_res = err_app.start_dev(
        &root,
        listed_shop(&root),
        "",
        &mut Vec::new(),
        &mut Vec::new(),
    );
    assert!(err_res.unwrap_err().contains("read pnpm's node-options:"));
    assert!(err_r.replaced().0.is_none());
}

#[test]
fn exec_exports_project_env_without_hook_and_keeps_stdout_clean() {
    let proj_tmp = tempfile::tempdir().unwrap();
    let mut tree = shop_tree();
    tree.push(("apps/web/.env", "API_URL=http://localhost:3001\n"));
    let root = write_tree(proj_tmp.path(), "shop", &tree);
    let web = root.join("apps/web");

    let sys_tmp = tempfile::tempdir().unwrap();
    let p = Paths::for_test(sys_tmp.path());
    let r = FakeRunner::new();
    set_up_ready_mac(&p, &r);

    // Unlisted project refuses exec.
    let app = new_test_app(p.clone(), &r, web.clone());
    let mut out = Vec::new();
    let mut err = Vec::new();
    assert_eq!(
        app.dispatch(
            &to_args(&["exec", "--", "pnpm", "db:migrate"]),
            &mut out,
            &mut err
        ),
        1
    );
    assert!(String::from_utf8_lossy(&err).contains("has no names yet: run lodo dev there first"));
    assert!(r.replaced().0.is_none());

    // Listed project runs in `cwd` (web), writes env summary to stderr only, and skips bind hook.
    let domains = listed_shop(&root);
    store::save(&p.domains_json, &domains).unwrap();
    fs::create_dir_all(&p.resolver_dir).unwrap();
    for d in &domains {
        if d.enabled {
            write_file(&p.resolver_dir.join(&d.name), &system::resolver_file());
        }
    }

    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["exec", "--", "pnpm", "db:migrate"]),
            &mut out,
            &mut err
        ),
        0,
        "stderr: {}",
        String::from_utf8_lossy(&err)
    );
    assert!(out.is_empty(), "stdout must stay clean for child command");
    assert!(String::from_utf8_lossy(&err).contains("env   DOCKER_HOST_IP=127.0.1.1\n"));
    let (rep_dir, rep_env) = r.replaced();
    assert_eq!(rep_dir.as_deref(), Some(web.as_path()));
    assert!(
        rep_env
            .iter()
            .any(|(k, v)| k == "DOCKER_HOST_IP" && v == "127.0.1.1")
    );
    assert!(
        rep_env
            .iter()
            .any(|(k, v)| k == "API_URL" && v == "https://web.shop.lodo")
    );
    assert!(
        !rep_env
            .iter()
            .any(|(k, v)| k == "NODE_OPTIONS" && v.contains("shop.lodo.cjs"))
    );
}

#[test]
fn add_set_rm_service_and_ls_lifecycle() {
    let tmp = tempfile::tempdir().unwrap();
    let p = Paths::for_test(tmp.path());
    let sf = SetupFake {
        fake: FakeRunner::new(),
        paths: p.clone(),
    };
    let mut app = new_test_app(p.clone(), &sf, tmp.path().to_path_buf());
    app.dns = "127.0.0.1:1".into();

    // 1. Before setup: `ls --json` succeeds with setup_done = false; `ls` without --json fails ready check;
    //    `add` fails with ERR_NOT_SET_UP without writing domains.json.
    let mut out = Vec::new();
    let mut err = Vec::new();
    assert_eq!(
        app.dispatch(&to_args(&["ls", "--json"]), &mut out, &mut err),
        0
    );
    let pre_report: LsReport = serde_json::from_slice(&out).unwrap();
    assert!(!pre_report.setup_done);
    assert!(pre_report.domains.is_empty());

    out.clear();
    err.clear();
    assert_eq!(app.dispatch(&to_args(&["ls"]), &mut out, &mut err), 1);
    assert!(String::from_utf8_lossy(&err).contains("lodo ls runs once these pass"));

    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(&to_args(&["add", "crm"]), &mut out, &mut err),
        1
    );
    assert!(String::from_utf8_lossy(&err).contains(system::ERR_NOT_SET_UP));
    assert!(!p.domains_json.exists());

    // 2. Set up Mac; add parent + subdomain + compose file.
    set_up_ready_mac(&p, &sf.fake);
    let compose_file = p.home.join("crm/compose.yaml");
    write_file(&compose_file, "services: {}\n");

    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["add", "crm", "--compose", "~/crm/compose.yaml"]),
            &mut out,
            &mut err
        ),
        0,
        "stderr: {}",
        String::from_utf8_lossy(&err)
    );
    assert_eq!(
        String::from_utf8_lossy(&out),
        "added crm.lodo -> 127.0.1.1\n"
    );

    // Subdomain inherits parent's address automatically!
    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["add", "web.crm.lodo", "--port", "3000", "--https"]),
            &mut out,
            &mut err
        ),
        0,
        "stderr: {}",
        String::from_utf8_lossy(&err)
    );
    assert_eq!(
        String::from_utf8_lossy(&out),
        "added web.crm.lodo -> 127.0.1.1:3000\n"
    );

    // 3. `set` compose-only change does NOT call `apply`; `set --port none` clears port and https;
    //    `set --off` disables domain.
    let calls_before = sf.fake.calls().len();
    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["set", "crm", "--compose", "none"]),
            &mut out,
            &mut err
        ),
        0
    );
    assert_eq!(String::from_utf8_lossy(&out), "updated crm.lodo\n");
    assert_eq!(sf.fake.calls().len(), calls_before);

    out.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["set", "web.crm.lodo", "--port", "none"]),
            &mut out,
            &mut err
        ),
        0
    );
    let web_after = store::find(&store::load(&p.domains_json).unwrap(), "web.crm.lodo")
        .unwrap()
        .clone();
    assert_eq!(web_after.port, 0);
    assert!(!web_after.https);

    out.clear();
    assert_eq!(
        app.dispatch(
            &to_args(&["set", "web.crm.lodo", "--off"]),
            &mut out,
            &mut err
        ),
        0
    );
    assert!(
        !store::find(&store::load(&p.domains_json).unwrap(), "web.crm.lodo")
            .unwrap()
            .enabled
    );

    // 4. `ls` table (colored + `--no-color`) and `ls -json`.
    app.color = true;
    out.clear();
    err.clear();
    assert_eq!(app.dispatch(&to_args(&["ls"]), &mut out, &mut err), 0);
    let ls_colored = String::from_utf8_lossy(&out).into_owned();
    assert!(ls_colored.contains("\x1b["));
    assert!(ls_colored.contains("crm.lodo"));
    assert!(ls_colored.contains("  "));

    out.clear();
    assert_eq!(
        app.dispatch(&to_args(&["ls", "--no-color"]), &mut out, &mut err),
        0
    );
    let ls_plain = String::from_utf8_lossy(&out).into_owned();
    assert!(!ls_plain.contains("\x1b["));
    assert!(ls_plain.contains("NAME"));
    assert!(ls_plain.contains("web.crm.lodo"));

    out.clear();
    assert_eq!(
        app.dispatch(&to_args(&["ls", "-json"]), &mut out, &mut err),
        0
    );
    let post_report: LsReport = serde_json::from_slice(&out).unwrap();
    assert!(post_report.setup_done);
    assert_eq!(post_report.domains.len(), 2);

    // 5. `service` command turns caddy off and on.
    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(&to_args(&["service", "caddy", "off"]), &mut out, &mut err),
        0
    );
    assert_eq!(String::from_utf8_lossy(&out), "caddy off\n");

    // 6. `rm` removes parent + subdomain, deletes `<project>.cjs`, and applies.
    crate::dev::write_hooks(&p, "crm.lodo", &BTreeMap::new()).unwrap();
    assert!(p.dev_dir.join("crm.lodo.cjs").exists());
    out.clear();
    err.clear();
    assert_eq!(
        app.dispatch(&to_args(&["rm", "crm"]), &mut out, &mut err),
        0
    );
    assert_eq!(
        String::from_utf8_lossy(&out),
        "removed crm.lodo\nremoved web.crm.lodo\n"
    );
    assert!(store::load(&p.domains_json).unwrap().is_empty());
    assert!(!p.dev_dir.join("crm.lodo.cjs").exists());
}

#[test]
fn dev_error_paths_parallel_worktrees_and_project_env() {
    let proj_tmp = tempfile::tempdir().unwrap();
    let root = write_tree(proj_tmp.path(), "shop", &shop_tree());

    // 1. Outside a project before setup: fails immediately without running commands.
    let empty_dir = tempfile::tempdir().unwrap();
    let sys_tmp = tempfile::tempdir().unwrap();
    let r1 = FakeRunner::new();
    let app1 = new_test_app(
        Paths::for_test(sys_tmp.path()),
        &r1,
        empty_dir.path().to_path_buf(),
    );
    let mut out = Vec::new();
    let mut err = Vec::new();
    assert_eq!(app1.dispatch(&to_args(&["dev"]), &mut out, &mut err), 1);
    assert!(String::from_utf8_lossy(&err).contains(scan::ERR_NO_PROJECT));
    assert!(r1.calls().is_empty());

    // 2. Setup fails when sudo -v password prompt is canceled.
    let cancel_tmp = tempfile::tempdir().unwrap();
    let cancel_p = Paths::for_test(cancel_tmp.path());
    write_file(&cancel_p.brew, "");
    write_file(&cancel_p.dnsmasq, "");
    write_file(&cancel_p.system_conf, "");
    let r2 = FakeRunner::new();
    r2.fail(
        run::line(&cancel_p.sudo.to_string_lossy(), &["-v"]),
        "Sorry, try again.",
    );
    let app2 = new_test_app(cancel_p.clone(), &r2, root.clone());
    out.clear();
    err.clear();
    assert_eq!(app2.dispatch(&to_args(&["dev"]), &mut out, &mut err), 1);
    assert!(String::from_utf8_lossy(&err).contains("✗ admin password"));
    assert!(!cancel_p.domains_json.exists());
    assert!(r2.replaced().0.is_none());

    // 3. Refuses when prerequisite check 1 fails after setup_done is true.
    let prereq_tmp = tempfile::tempdir().unwrap();
    let prereq_p = Paths::for_test(prereq_tmp.path());
    write_file(
        &prereq_p.system_conf,
        &system::rewrite_system_conf("", &prereq_p),
    );
    write_file(&prereq_p.script, &system::script(&prereq_p));
    let r3 = FakeRunner::new();
    let app3 = new_test_app(prereq_p.clone(), &r3, root.clone());
    out.clear();
    err.clear();
    assert_eq!(app3.dispatch(&to_args(&["dev"]), &mut out, &mut err), 1);
    assert!(
        !r3.calls()
            .contains(&run::line(&prereq_p.sudo.to_string_lossy(), &["-v"]))
    );
    let err_s = String::from_utf8_lossy(&err);
    assert!(err_s.contains("✗ 1 dnsmasq"));
    assert!(err_s.contains("lodo dev starts once these pass"));
    assert!(r3.replaced().0.is_none());

    // 4. Error paths in start_dev: no package.json, no dev script, missing package manager.
    let no_pkg_root = write_tree(
        proj_tmp.path(),
        "no-pkg",
        &[(".git/HEAD", ""), ("apps/web/package.json", "{}")],
    );
    let no_dev_root = write_tree(
        proj_tmp.path(),
        "no-dev",
        &[
            (".git/HEAD", ""),
            (
                "package.json",
                r#"{"scripts": {"lint": "biome", "build": "tsc"}}"#,
            ),
        ],
    );
    for (dir, missing_pm, want_sub) in [
        (&no_pkg_root, false, "has no package.json"),
        (
            &no_dev_root,
            false,
            "package.json has no dev script, only build, lint",
        ),
        (&root, true, "find pnpm:"),
    ] {
        let ep = Paths::for_test(tempfile::tempdir().unwrap().path());
        let er = FakeRunner::new();
        let mut eapp = new_test_app(ep.clone(), &er, dir.clone());
        if missing_pm {
            eapp.look_path = Box::new(|_| Err("not found in PATH".into()));
        }
        let got_err = eapp
            .start_dev(dir, listed_shop(dir), "", &mut Vec::new(), &mut Vec::new())
            .unwrap_err();
        assert!(
            got_err.contains(want_sub),
            "got {got_err:?}, want {want_sub:?}"
        );
        assert!(!ep.dev_dir.exists());
        assert!(er.replaced().0.is_none());
    }

    // 5. Concurrent `lodo dev` in two worktrees picks distinct slots (shop2.lodo & shop3.lodo).
    let conc_sys = tempfile::tempdir().unwrap();
    let conc_p = Paths::for_test(conc_sys.path());
    let conc_sf = SetupFake {
        fake: FakeRunner::new(),
        paths: conc_p.clone(),
    };
    set_up_ready_mac(&conc_p, &conc_sf.fake);
    let main_domains = vec![
        Domain {
            name: "shop.lodo".into(),
            address: "127.0.1.1".into(),
            enabled: true,
            root: root.to_string_lossy().into_owned(),
            ..Domain::default()
        },
        Domain {
            name: "web.shop.lodo".into(),
            address: "127.0.1.1".into(),
            port: 3001,
            enabled: true,
            ..Domain::default()
        },
    ];
    store::save(&conc_p.domains_json, &main_domains).unwrap();
    fs::create_dir_all(&conc_p.resolver_dir).unwrap();
    for d in &main_domains {
        write_file(&conc_p.resolver_dir.join(&d.name), &system::resolver_file());
    }

    let make_wt = |name: &str| -> PathBuf {
        let dir = proj_tmp.path().join(name);
        for (rel, content) in shop_tree() {
            if rel != ".git/HEAD" {
                write_file(&dir.join(rel), content);
            }
        }
        write_file(
            &dir.join(".git"),
            &format!(
                "gitdir: {}\n",
                root.join(".git/worktrees").join(name).display()
            ),
        );
        dir
    };
    let wt_a = make_wt("wt-conc-a");
    let wt_b = make_wt("wt-conc-b");

    thread::scope(|s| {
        for wt in [&wt_a, &wt_b] {
            let p_c = conc_p.clone();
            let sf_ref = &conc_sf;
            let cwd_c = wt.clone();
            s.spawn(move || {
                let app = new_test_app(p_c, sf_ref, cwd_c);
                let mut o = Vec::new();
                let mut e = Vec::new();
                assert_eq!(app.dispatch(&to_args(&["dev"]), &mut o, &mut e), 0);
            });
        }
    });
    let after_wt = store::load(&conc_p.domains_json).unwrap();
    let s2 = store::find(&after_wt, "shop2.lodo").unwrap();
    let s3 = store::find(&after_wt, "shop3.lodo").unwrap();
    assert_eq!(s2.address, "127.0.1.2");
    assert_eq!(s3.address, "127.0.1.3");
    assert_ne!(s2.root, s3.root);

    // 6. `project_env` replaces matching keys and preserves input immutability.
    let single_root = write_tree(
        proj_tmp.path(),
        "single",
        &[
            (".git/HEAD", "ref: refs/heads/master\n"),
            ("package.json", r#"{"scripts": {"dev": "next dev"}}"#),
            (".env", "API_URL=http://localhost:3000\n"),
        ],
    );
    let single_domains = vec![Domain {
        name: "single.lodo".into(),
        address: "127.0.1.1".into(),
        port: 3000,
        enabled: true,
        root: single_root.to_string_lossy().into_owned(),
        ..Domain::default()
    }];
    let sp = scan::find(&single_root).unwrap();
    let sprop = scan::propose(&sp, &single_domains, "", domains::is_gone);
    let input_env = vec![
        ("PATH".to_string(), "/bin".to_string()),
        ("DOCKER_HOST_IP".to_string(), "127.0.0.9".to_string()),
        ("API_URL".to_string(), "http://old".to_string()),
    ];
    let got_env = project_env(&input_env, &sprop);
    assert_eq!(
        got_env,
        vec![
            ("PATH".into(), "/bin".into()),
            ("DOCKER_HOST_IP".into(), "127.0.1.1".into()),
            ("API_URL".into(), "http://single.lodo".into()),
        ]
    );
    assert_eq!(input_env[1].1, "127.0.0.9");
}

#[test]
fn dev_auto_starts_menubar_skips_running_and_warns_on_error() {
    let proj_tmp = tempfile::tempdir().unwrap();
    let root = write_tree(proj_tmp.path(), "shop", &shop_tree());

    // 1. Starts the menu bar LaunchAgent when stopped and swiftc is available.
    let sys_tmp = tempfile::tempdir().unwrap();
    let p = Paths::for_test(sys_tmp.path());
    let r = FakeRunner::new();
    set_up_ready_mac(&p, &r);
    write_file(&p.swiftc, "");
    let app = new_test_app(p.clone(), &r, root.clone());
    let mut out = Vec::new();
    let mut err = Vec::new();
    app.start_dev(&root, listed_shop(&root), "", &mut out, &mut err)
        .unwrap();
    let launchctl = p.launchctl.to_string_lossy().into_owned();
    let bootstrap_line = run::line(
        &launchctl,
        &["bootstrap", "gui/501", &p.menubar_plist.to_string_lossy()],
    );
    assert!(p.menubar_plist.exists(), "menubar plist should be written");
    assert!(
        r.calls().contains(&bootstrap_line),
        "expected menubar bootstrap in {:?}",
        r.calls()
    );
    assert!(
        !String::from_utf8_lossy(&err).contains("warn  menubar:"),
        "unexpected menubar warning: {}",
        String::from_utf8_lossy(&err)
    );
    assert_eq!(r.replaced().0.as_deref(), Some(root.as_path()));

    // 2. Does not kickstart when the menu bar is already up to date and running.
    let r2 = FakeRunner::new();
    r2.set(
        run::line(&launchctl, &["print", "gui/501/io.lodo.menubar"]),
        "\tstate = running\n\tpid = 999\n",
    );
    let app2 = new_test_app(p, &r2, root.clone());
    out.clear();
    err.clear();
    app2.start_dev(&root, listed_shop(&root), "", &mut out, &mut err)
        .unwrap();
    assert!(
        !r2.calls().iter().any(|c| c.contains("kickstart")),
        "running menubar should not be restarted: {:?}",
        r2.calls()
    );
    assert!(
        !r2.calls().iter().any(|c| c.contains("bootstrap")),
        "running menubar should not be bootstrapped again: {:?}",
        r2.calls()
    );
    assert_eq!(r2.replaced().0.as_deref(), Some(root.as_path()));

    // 3. Warns on stderr and still launches `<pm> run dev` when menubar startup fails.
    let fail_tmp = tempfile::tempdir().unwrap();
    let p_fail = Paths::for_test(fail_tmp.path());
    let r_fail = FakeRunner::new();
    set_up_ready_mac(&p_fail, &r_fail);
    write_file(&p_fail.brew_menubar_bin, "mach-o");
    let fail_launchctl = p_fail.launchctl.to_string_lossy().into_owned();
    r_fail.fail(
        run::line(
            &fail_launchctl,
            &[
                "bootstrap",
                "gui/501",
                &p_fail.menubar_plist.to_string_lossy(),
            ],
        ),
        "Bootstrap failed: 5: Input/output error",
    );
    let app_fail = new_test_app(p_fail, &r_fail, root.clone());
    out.clear();
    err.clear();
    app_fail
        .start_dev(&root, listed_shop(&root), "", &mut out, &mut err)
        .unwrap();
    let err_s = String::from_utf8_lossy(&err);
    assert!(
        err_s.contains("warn  menubar: start menubar:"),
        "expected menubar warning, got: {err_s:?}"
    );
    assert_eq!(r_fail.replaced().0.as_deref(), Some(root.as_path()));
}
