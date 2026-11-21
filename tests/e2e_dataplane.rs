use std::collections::HashMap;
use std::fs;
use std::io::{self, Read, Write};
use std::net::{TcpListener, TcpStream, UdpSocket};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::Mutex;
use std::thread;
use std::time::Duration;

use lodo::cli::App;
use lodo::ls::LsReport;
use lodo::paths::Paths;
use lodo::run::{FakeRunner, RunError, Runner, line};
use lodo::store::{self, Domain};
use lodo::system;
use serde::Deserialize;

const DNSMASQ_BIN: &str = "/opt/homebrew/opt/dnsmasq/sbin/dnsmasq";
const CADDY_BIN: &str = "/opt/homebrew/bin/caddy";
const SWIFTC_BIN: &str = "/usr/bin/swiftc";

struct ChildGuard(Option<Child>);

impl ChildGuard {
    fn new(child: Child) -> Self {
        Self(Some(child))
    }

    fn stop(&mut self) {
        if let Some(mut c) = self.0.take() {
            let _ = c.kill();
            let _ = c.wait();
        }
    }
}

impl Drop for ChildGuard {
    fn drop(&mut self) {
        self.stop();
    }
}

struct E2eRunner {
    fake: FakeRunner,
    paths: Paths,
    node_bin: PathBuf,
    swiftc_bin: Option<PathBuf>,
    node_child: Mutex<Option<ChildGuard>>,
}

impl E2eRunner {
    fn stop_node(&self) {
        if let Ok(mut guard) = self.node_child.lock()
            && let Some(mut child) = guard.take()
        {
            child.stop();
        }
    }
}

impl Runner for E2eRunner {
    fn run(
        &self,
        timeout: Option<Duration>,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError> {
        if let Some(ref swiftc) = self.swiftc_bin
            && Path::new(name) == self.paths.swiftc
        {
            let out = Command::new(swiftc)
                .args(args)
                .output()
                .map_err(|e| RunError {
                    cmd: line(name, args),
                    stderr: String::new(),
                    stdout: String::new(),
                    timed_out: false,
                    source: e,
                })?;
            if !out.status.success() {
                return Err(RunError {
                    cmd: line(name, args),
                    stderr: String::from_utf8_lossy(&out.stderr).trim().to_string(),
                    stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
                    timed_out: false,
                    source: io::Error::other("swiftc failed"),
                });
            }
            return Ok(String::from_utf8_lossy(&out.stdout).into_owned());
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
        let script_str = self.paths.script.to_string_lossy();
        if Path::new(name) == self.paths.sudo && args == ["-n", script_str.as_ref()] {
            let mut child = Command::new("/bin/sh")
                .arg(&self.paths.script)
                .stdin(Stdio::piped())
                .stdout(Stdio::piped())
                .stderr(Stdio::piped())
                .spawn()
                .map_err(|e| RunError {
                    cmd: line(name, args),
                    stderr: String::new(),
                    stdout: String::new(),
                    timed_out: false,
                    source: e,
                })?;
            if let Some(mut pipe) = child.stdin.take() {
                let _ = pipe.write_all(stdin.as_bytes());
            }
            let out = child.wait_with_output().map_err(|e| RunError {
                cmd: line(name, args),
                stderr: String::new(),
                stdout: String::new(),
                timed_out: false,
                source: e,
            })?;
            if !out.status.success() {
                return Err(RunError {
                    cmd: line(name, args),
                    stderr: String::from_utf8_lossy(&out.stderr).trim().to_string(),
                    stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
                    timed_out: false,
                    source: io::Error::other("apply-resolvers.sh failed"),
                });
            }
            return Ok(String::from_utf8_lossy(&out.stdout).into_owned());
        }
        self.fake.run_input(timeout, stdin, name, args)
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
        self.fake.replace(dir, name, args, env)?;
        let child = Command::new(&self.node_bin)
            .arg("server.js")
            .current_dir(dir)
            .env_clear()
            .envs(env.iter().cloned())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|e| RunError {
                cmd: "node server.js".into(),
                stderr: String::new(),
                stdout: String::new(),
                timed_out: false,
                source: e,
            })?;
        *self.node_child.lock().unwrap() = Some(ChildGuard::new(child));
        Ok(())
    }
}

fn find_in_path(bin: &str) -> Option<PathBuf> {
    let path_var = std::env::var_os("PATH")?;
    std::env::split_paths(&path_var)
        .map(|d| d.join(bin))
        .find(|p| p.is_file())
}

fn can_listen(addr: &str) -> bool {
    TcpListener::bind((addr, 0)).is_ok()
}

fn free_tcp_port(addr: &str) -> u16 {
    TcpListener::bind((addr, 0))
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

fn free_udp_port() -> u16 {
    UdpSocket::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

fn write_tree(base: &Path, files: &[(&str, &str)]) -> PathBuf {
    let root = base.join("shop");
    for (rel, content) in files {
        let p = root.join(rel);
        if let Some(parent) = p.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(&p, content).unwrap();
    }
    root
}

fn set_up_fake_mac(p: &Paths, fake: &FakeRunner) {
    use std::os::unix::fs::PermissionsExt;

    for (path, content, mode) in [
        (&p.dnsmasq, String::new(), 0o755),
        (&p.caddy, String::new(), 0o755),
        (&p.system_conf, system::rewrite_system_conf("", p), 0o644),
        (
            &p.system_caddyfile,
            system::rewrite_system_caddyfile("", p),
            0o644,
        ),
        (&p.script, system::script(p), 0o755),
        (
            &p.launch_agents.join("sh.brew.dnsmasq.plist"),
            String::new(),
            0o644,
        ),
        (
            &p.launch_agents.join("sh.brew.caddy.plist"),
            String::new(),
            0o644,
        ),
    ] {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(path, content).unwrap();
        fs::set_permissions(path, fs::Permissions::from_mode(mode)).unwrap();
    }

    for label in system::DNS_SYSTEM_LABELS {
        fake.fail(
            line(
                &p.launchctl.to_string_lossy(),
                &["print", &format!("system/{label}")],
            ),
            "Could not find service",
        );
    }
    fake.set(
        line(
            &p.launchctl.to_string_lossy(),
            &["print", &format!("system/{}", system::LOOPBACK_LABEL)],
        ),
        "system/io.lodo.loopback = {\n}\n",
    );
    let mut lo0 = String::from("\tinet 127.0.0.1 netmask 0xff000000\n");
    for i in store::OWN_FIRST..=store::OWN_LAST {
        lo0.push_str(&format!(
            "\tinet {} netmask 0xff000000\n",
            store::own_address(i)
        ));
    }
    fake.set(line(&p.ifconfig.to_string_lossy(), &["lo0"]), lo0);
    fake.set(
        line(
            &p.launchctl.to_string_lossy(),
            &["print", "gui/501/sh.brew.dnsmasq"],
        ),
        "\tstate = running\n\tpid = 42\n",
    );
    fake.set(
        line(
            &p.launchctl.to_string_lossy(),
            &["print", "gui/501/sh.brew.caddy"],
        ),
        "\tstate = running\n\tpid = 43\n",
    );
    fake.set(
        line(
            &p.sudo.to_string_lossy(),
            &["-n", "-k", "-l", &p.script.to_string_lossy()],
        ),
        format!("{}\n", p.script.display()),
    );
}

#[test]
fn e2e_dataplane_dnsmasq_caddy_node_and_ls() {
    system::set_timings(Duration::ZERO, Duration::ZERO);

    if !Path::new(DNSMASQ_BIN).is_file() || !Path::new(CADDY_BIN).is_file() {
        return;
    }
    let Some(node_bin) = find_in_path("node") else {
        return;
    };

    let addr = if can_listen("127.0.1.1") {
        "127.0.1.1"
    } else {
        "127.0.0.1"
    };
    let app_port = free_tcp_port(addr);
    let caddy_port = free_tcp_port("127.0.0.1");
    let dns_port = free_udp_port();

    let server_js = format!(
        r#"const http = require("node:http");
const srv = http.createServer((req, res) => {{
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify({{
    host: req.headers.host,
    localAddress: req.socket.localAddress,
    dockerHostIP: process.env.DOCKER_HOST_IP,
    apiURL: process.env.API_URL,
    databaseURL: process.env.DATABASE_URL,
    viteHosts: process.env.__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS,
  }}));
}});
srv.listen({app_port}, "localhost");
"#
    );

    let proj_tmp = tempfile::tempdir().unwrap();
    let root = write_tree(
        proj_tmp.path(),
        &[
            (".git/HEAD", "ref: refs/heads/master\n"),
            (".env", &format!("DOCKER_HOST_IP={addr}\n")),
            ("package.json", r#"{"scripts": {"dev": "node server.js"}}"#),
            ("pnpm-lock.yaml", ""),
            ("pnpm-workspace.yaml", "packages: [apps/*]\n"),
            (
                "apps/web/package.json",
                &format!(r#"{{"scripts": {{"dev": "next dev -p {app_port}"}}}}"#),
            ),
            (
                "apps/web/.env",
                &format!(
                    "API_URL=http://localhost:{app_port}\nDATABASE_URL=postgres://u:hunter2@localhost:5432/shop\n"
                ),
            ),
            (
                "compose.yaml",
                r#"services: {db: {image: postgres, ports: ["${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"]}}"#,
            ),
            ("server.js", &server_js),
        ],
    );

    let sys_tmp = tempfile::tempdir().unwrap();
    let p = Paths::for_test(sys_tmp.path());
    if addr == "127.0.0.1" {
        store::save(
            &p.domains_json,
            &[Domain {
                name: "shop.lodo".into(),
                address: "127.0.0.1".into(),
                enabled: true,
                ..Domain::default()
            }],
        )
        .unwrap();
    }

    let fake = FakeRunner::new();
    set_up_fake_mac(&p, &fake);
    // Write empty resolver files for pre-seeded domains so Prerequisite check 4 passes.
    if addr == "127.0.0.1" {
        fs::create_dir_all(&p.resolver_dir).unwrap();
        fs::write(p.resolver_dir.join("shop.lodo"), system::resolver_file()).unwrap();
    }

    let runner = E2eRunner {
        fake,
        paths: p.clone(),
        node_bin,
        swiftc_bin: None,
        node_child: Mutex::new(None),
    };

    let my_uid = nix::unistd::Uid::current().as_raw();
    let mut app = App::new(p.clone(), &runner);
    app.root_uid = my_uid;
    app.cwd = root;
    app.look_path = Box::new(|bin| Ok(PathBuf::from(format!("/opt/bin/{bin}"))));
    app.dns = format!("127.0.0.1:{dns_port}");
    app.http_port = caddy_port;

    let mut stdout = Vec::new();
    let mut stderr = Vec::new();
    let code = app.dispatch(&["dev".to_string()], &mut stdout, &mut stderr);
    let out_str = String::from_utf8_lossy(&stdout);
    let err_str = String::from_utf8_lossy(&stderr);
    assert_eq!(
        code, 0,
        "lodo dev failed:\nstdout: {out_str}\nstderr: {err_str}"
    );
    assert!(
        !out_str.contains("hunter2"),
        "password leaked in stdout: {out_str}"
    );
    assert!(
        out_str.contains("postgres://***@shop.lodo:5432/shop"),
        "expected masked DATABASE_URL in stdout:\n{out_str}"
    );

    for name in ["shop.lodo", "web.shop.lodo"] {
        let got = fs::read_to_string(p.resolver_dir.join(name)).unwrap();
        assert_eq!(got, system::resolver_file());
    }

    // Start real dnsmasq on dns_port.
    let dns_child = Command::new(DNSMASQ_BIN)
        .args([
            "--keep-in-foreground",
            "--no-resolv",
            "--no-hosts",
            "--listen-address=127.0.0.1",
            &format!("--port={dns_port}"),
            &format!("--conf-file={}", p.dnsmasq_conf.display()),
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    let _dns_guard = ChildGuard::new(dns_child);

    // Start real Caddy on caddy_port importing lodo's generated Caddyfile.
    let caddy_home = tempfile::tempdir().unwrap();
    let main_cf = caddy_home.path().join("Caddyfile");
    let quoted_cf = serde_json::to_string(&p.caddyfile.to_string_lossy()).unwrap();
    fs::write(
        &main_cf,
        format!("{{\n\tadmin off\n\thttp_port {caddy_port}\n}}\nimport {quoted_cf}\n"),
    )
    .unwrap();
    let caddy_child = Command::new(CADDY_BIN)
        .args(["run", "--config"])
        .arg(&main_cf)
        .args(["--adapter", "caddyfile"])
        .env("HOME", caddy_home.path())
        .env("XDG_DATA_HOME", caddy_home.path())
        .env("XDG_CONFIG_HOME", caddy_home.path())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    let _caddy_guard = ChildGuard::new(caddy_child);

    let saved = store::load(&p.domains_json).unwrap();
    for d in &saved {
        runner.fake.set(
            line(
                &p.dscacheutil.to_string_lossy(),
                &["-q", "host", "-a", "name", &d.name],
            ),
            format!("name: {}\nip_address: {}\n\n", d.name, d.address),
        );
    }
    runner.fake.set(
        line(
            &p.lsof.to_string_lossy(),
            &["-nP", &format!("-iTCP:{caddy_port}"), "-sTCP:LISTEN"],
        ),
        format!("COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\ncaddy 43 tester 7u IPv4 0x1 0t0 TCP *:{caddy_port} (LISTEN)\n"),
    );

    #[derive(Deserialize, Default)]
    #[serde(rename_all = "camelCase")]
    struct EchoBody {
        host: String,
        local_address: String,
        #[serde(rename = "dockerHostIP")]
        docker_host_ip: String,
        #[serde(rename = "apiURL")]
        api_url: String,
        #[serde(rename = "databaseURL")]
        database_url: String,
        vite_hosts: String,
    }

    let mut via_header = String::new();
    let mut parsed_body = EchoBody::default();
    for _ in 0..50 {
        if let Ok(mut stream) = TcpStream::connect((addr, caddy_port)) {
            let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
            let _ = write!(
                stream,
                "GET / HTTP/1.1\r\nHost: web.shop.lodo\r\nConnection: close\r\n\r\n"
            );
            let mut resp = String::new();
            if stream.read_to_string(&mut resp).is_ok()
                && resp.starts_with("HTTP/1.1 200")
                && let Some((hdrs, json_part)) = resp.split_once("\r\n\r\n")
            {
                for l in hdrs.lines() {
                    if let Some((k, v)) = l.split_once(':')
                        && k.eq_ignore_ascii_case("via")
                    {
                        via_header = v.trim().to_string();
                    }
                }
                if let Ok(b) = serde_json::from_str::<EchoBody>(json_part.trim()) {
                    parsed_body = b;
                    break;
                }
            }
        }
        thread::sleep(Duration::from_millis(50));
    }

    assert_eq!(parsed_body.host, "web.shop.lodo");
    assert!(via_header.contains("Caddy"), "via = {via_header}");
    assert_eq!(parsed_body.local_address, addr);
    assert_eq!(parsed_body.docker_host_ip, addr);
    assert_eq!(parsed_body.api_url, "http://web.shop.lodo");
    assert_eq!(
        parsed_body.database_url,
        "postgres://u:hunter2@shop.lodo:5432/shop"
    );
    assert_eq!(parsed_body.vite_hosts, ".shop.lodo");

    // Verify `lodo ls` and `lodo ls --json` against live dnsmasq + Caddy + Node!
    let mut ls_out = Vec::new();
    let mut ls_err = Vec::new();
    assert_eq!(
        app.dispatch(&["ls".into(), "--json".into()], &mut ls_out, &mut ls_err),
        0
    );
    let report: LsReport = serde_json::from_slice(&ls_out).unwrap();
    assert!(report.setup_done);
    assert!(report.caddy_ready);
    assert_eq!(report.domains.len(), 2);
    for dom in &report.domains {
        assert_eq!(dom.status, "ok", "domain failed in ls --json: {dom:?}");
    }

    let mut table_out = Vec::new();
    assert_eq!(
        app.dispatch(
            &["ls".into(), "--no-color".into()],
            &mut table_out,
            &mut ls_err
        ),
        0
    );
    let table_text = String::from_utf8_lossy(&table_out);
    assert!(table_text.contains("web.shop.lodo"));
    assert!(table_text.contains("dns ✓"));
}

#[test]
fn e2e_menubar_compile_dump_and_cli_mutations() {
    system::set_timings(Duration::ZERO, Duration::ZERO);

    if !Path::new(SWIFTC_BIN).is_file() {
        return;
    }
    let Some(node_bin) = find_in_path("node") else {
        return;
    };

    let addr = if can_listen("127.0.1.1") {
        "127.0.1.1"
    } else {
        "127.0.0.1"
    };
    let web_port = free_tcp_port(addr);
    let api_port = free_tcp_port(addr); // never started

    let server_js = format!(
        r#"const http = require("node:http");
http.createServer((_, res) => res.end("ok")).listen({web_port}, "localhost");
"#
    );

    let proj_tmp = tempfile::tempdir().unwrap();
    let root = write_tree(
        proj_tmp.path(),
        &[
            (".git/HEAD", "ref: refs/heads/master\n"),
            (".env", &format!("DOCKER_HOST_IP={addr}\n")),
            ("package.json", r#"{"scripts": {"dev": "node server.js"}}"#),
            ("pnpm-lock.yaml", ""),
            ("pnpm-workspace.yaml", "packages: [apps/*]\n"),
            (
                "apps/web/package.json",
                &format!(r#"{{"scripts": {{"dev": "next dev -p {web_port}"}}}}"#),
            ),
            (
                "apps/api/package.json",
                &format!(r#"{{"scripts": {{"dev": "vite --port {api_port}"}}}}"#),
            ),
            ("server.js", &server_js),
        ],
    );

    let sys_tmp = tempfile::tempdir().unwrap();
    let p = Paths::for_test(sys_tmp.path());
    if addr == "127.0.0.1" {
        store::save(
            &p.domains_json,
            &[Domain {
                name: "shop.lodo".into(),
                address: "127.0.0.1".into(),
                enabled: true,
                ..Domain::default()
            }],
        )
        .unwrap();
        fs::create_dir_all(&p.resolver_dir).unwrap();
        fs::write(p.resolver_dir.join("shop.lodo"), system::resolver_file()).unwrap();
    }

    let fake = FakeRunner::new();
    set_up_fake_mac(&p, &fake);
    fs::create_dir_all(p.swiftc.parent().unwrap()).unwrap();
    fs::write(&p.swiftc, b"").unwrap();

    let menubar_print = line(
        &p.launchctl.to_string_lossy(),
        &["print", "gui/501/io.lodo.menubar"],
    );
    fake.fail(&menubar_print, "Could not find service");

    let runner = E2eRunner {
        fake,
        paths: p.clone(),
        node_bin,
        swiftc_bin: Some(PathBuf::from(SWIFTC_BIN)),
        node_child: Mutex::new(None),
    };

    let mut app = App::new(p.clone(), &runner);
    app.root_uid = nix::unistd::Uid::current().as_raw();
    app.cwd = root;
    app.look_path = Box::new(|bin| Ok(PathBuf::from(format!("/opt/bin/{bin}"))));

    let mut stdout = Vec::new();
    let mut stderr = Vec::new();
    assert_eq!(app.dispatch(&["dev".into()], &mut stdout, &mut stderr), 0);

    stdout.clear();
    stderr.clear();
    assert_eq!(
        app.dispatch(&["menubar".into()], &mut stdout, &mut stderr),
        0,
        "stderr: {}",
        String::from_utf8_lossy(&stderr)
    );
    assert_eq!(String::from_utf8_lossy(&stdout), "menubar running\n");
    assert!(p.menubar_bin.is_file());

    if Path::new("/usr/bin/plutil").is_file() {
        let lint = Command::new("/usr/bin/plutil")
            .arg("-lint")
            .arg(&p.menubar_plist)
            .output()
            .unwrap();
        assert!(lint.status.success());
    }

    let run_dump = || -> String {
        let out = Command::new(&p.menubar_bin)
            .arg("--dump")
            .env("LODO_DOMAINS_JSON", &p.domains_json)
            .output()
            .unwrap();
        assert!(out.status.success());
        String::from_utf8_lossy(&out.stdout).into_owned()
    };

    let want_header = format!("shop.lodo · {addr}");
    let want_row = format!("● web.shop.lodo  (http :{web_port})");
    let mut got = String::new();
    for _ in 0..40 {
        got = run_dump();
        if got.contains(&want_row) {
            break;
        }
        thread::sleep(Duration::from_millis(50));
    }
    assert!(
        got.contains(&want_header) && got.contains(&want_row),
        "dump =\n{got}"
    );
    assert!(
        !got.contains("api.shop.lodo"),
        "stopped service listed:\n{got}"
    );

    // Exercise CLI domain management (`add`, `set`, `rm`).
    let mut cmd_out = Vec::new();
    let mut cmd_err = Vec::new();
    assert_eq!(
        app.dispatch(
            &[
                "add".into(),
                "docs.shop.lodo".into(),
                "--address".into(),
                addr.into(),
                "--port".into(),
                "4321".into()
            ],
            &mut cmd_out,
            &mut cmd_err
        ),
        0
    );
    assert_eq!(
        app.dispatch(
            &["set".into(), "docs.shop.lodo".into(), "--https".into()],
            &mut cmd_out,
            &mut cmd_err
        ),
        0
    );
    let domains_after_set: HashMap<String, Domain> = store::load(&p.domains_json)
        .unwrap()
        .into_iter()
        .map(|d| (d.name.clone(), d))
        .collect();
    assert!(domains_after_set["docs.shop.lodo"].https);

    assert_eq!(
        app.dispatch(
            &["rm".into(), "docs.shop.lodo".into()],
            &mut cmd_out,
            &mut cmd_err
        ),
        0
    );

    runner.stop_node();
    assert_eq!(run_dump(), "No running services\n");

    runner
        .fake
        .set(&menubar_print, "\tstate = running\n\tpid = 777\n");
    stdout.clear();
    stderr.clear();
    assert_eq!(
        app.dispatch(
            &["menubar".into(), "--stop".into()],
            &mut stdout,
            &mut stderr
        ),
        0
    );
    assert_eq!(String::from_utf8_lossy(&stdout), "menubar stopped\n");
    assert!(!p.menubar_plist.exists());
}
