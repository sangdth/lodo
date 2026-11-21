//! Destructive full-lifecycle macOS E2E test (`setup` -> `menubar` -> `dev` ->
//! `ls` -> `doctor` -> `apply` -> `exec` -> `prune` -> `uninstall`).
//!
//! Run only on a disposable macOS VM or CI runner:
//! `LODO_E2E_DESTRUCTIVE=1 cargo test --locked --test e2e_system -- --ignored --nocapture`

use std::fs;
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process::Command;

use lodo::ls::LsReport;
use lodo::menubar;
use lodo::paths::Paths;
use serde::Deserialize;

struct UninstallGuard {
    bin: PathBuf,
}

impl Drop for UninstallGuard {
    fn drop(&mut self) {
        let _ = Command::new(&self.bin).arg("uninstall").status();
    }
}

fn run_lodo(dir: Option<&Path>, bin: &Path, args: &[&str]) -> String {
    let mut cmd = Command::new(bin);
    cmd.args(args);
    if let Some(d) = dir {
        cmd.current_dir(d);
    }
    let out = cmd.output().expect("spawn lodo");
    assert!(
        out.status.success(),
        "lodo {} failed:\nstdout:\n{}\nstderr:\n{}",
        args.join(" "),
        String::from_utf8_lossy(&out.stdout),
        String::from_utf8_lossy(&out.stderr),
    );
    String::from_utf8_lossy(&out.stdout).into_owned()
}

#[test]
#[ignore = "modifies real macOS /etc/resolver, /Library/LaunchDaemons, lo0, and Homebrew services"]
fn system_e2e_full_lifecycle() {
    if std::env::var("LODO_E2E_DESTRUCTIVE").as_deref() != Ok("1") {
        eprintln!("skipping destructive system e2e: set LODO_E2E_DESTRUCTIVE=1");
        return;
    }

    let p = Paths::default_paths().expect("default paths");
    let bin = PathBuf::from(env!("CARGO_BIN_EXE_lodo"));
    let _cleanup = UninstallGuard { bin: bin.clone() };

    // 1. lodo setup
    let setup_out = run_lodo(None, &bin, &["setup"]);
    for want in [
        "✓ resolver script installed",
        "✓ sudoers rule installed",
        "✓ sudo runs the resolver script without a password",
        "✓ 1 dnsmasq",
    ] {
        assert!(
            setup_out.contains(want),
            "lodo setup output lacks {want:?}:\n{setup_out}"
        );
    }

    // 1b. lodo menubar compiles the helper and starts its LaunchAgent.
    let menubar_target = format!("gui/{}/{}", p.uid, menubar::LABEL);
    let mb_out = run_lodo(None, &bin, &["menubar"]);
    assert_eq!(mb_out.trim(), "menubar running");
    assert!(p.menubar_plist.is_file());
    let lc_out = Command::new(&p.launchctl)
        .args(["print", &menubar_target])
        .output()
        .expect("launchctl print");
    assert!(lc_out.status.success());

    // 2. lodo dev in a real Node project, verifying macOS DNS + Caddy + bind.cjs + ls + doctor + apply + menubar
    let app_port = TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let res_tmp = tempfile::tempdir().unwrap();
    let result_file = res_tmp.path().join("result.json");
    let bin_json = serde_json::to_string(&bin.to_string_lossy()).unwrap();
    let mb_bin_json =
        serde_json::to_string(&menubar::active_binary_path(&p).to_string_lossy()).unwrap();
    let res_file_json = serde_json::to_string(&result_file.to_string_lossy()).unwrap();

    let server_js = format!(
        r#"const http = require("node:http");
const fs = require("node:fs");
const {{ execFile }} = require("node:child_process");
const {{ promisify }} = require("node:util");

const execFileAsync = promisify(execFile);
const bin = {bin_json};
const menubarBin = {mb_bin_json};
const resultFile = {res_file_json};

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

srv.listen({app_port}, "127.0.0.1", async () => {{
  let lastErr = "";
  for (let i = 0; i < 30; i++) {{
    try {{
      const resp = await fetch("http://web.shop.lodo/");
      if (resp.ok) {{
        const body = await resp.json();
        const {{ stdout: doctorOut }} = await execFileAsync(bin, ["doctor"]);
        const {{ stdout: applyOut }} = await execFileAsync(bin, ["apply"]);
        const {{ stdout: lsJson }} = await execFileAsync(bin, ["ls", "--json"]);
        const {{ stdout: menubarDump }} = await execFileAsync(menubarBin, ["--dump"]);
        fs.writeFileSync(resultFile, JSON.stringify({{
          status: resp.status,
          via: resp.headers.get("via") || "",
          ...body,
          doctorOut,
          applyOut,
          lsJson,
          menubarDump,
        }}));
        srv.close(() => process.exit(0));
        return;
      }}
      lastErr = "HTTP " + resp.status;
    }} catch (err) {{
      lastErr = String(err && err.message ? err.message : err);
    }}
    await new Promise((r) => setTimeout(r, 100));
  }}
  console.error("server.js failed:", lastErr);
  process.exit(1);
}});
"#
    );

    let proj_tmp = tempfile::tempdir().unwrap();
    let root = proj_tmp.path().join("shop");
    for (rel, content) in [
        (".git/HEAD", "ref: refs/heads/master\n".to_string()),
        (
            "package.json",
            r#"{"workspaces": ["apps/*"], "scripts": {"dev": "node server.js"}}"#.to_string(),
        ),
        (
            "apps/web/package.json",
            format!(r#"{{"scripts": {{"dev": "next dev -p {app_port}"}}}}"#),
        ),
        (
            "apps/web/.env",
            format!(
                "API_URL=http://localhost:{app_port}\nDATABASE_URL=postgres://u:hunter2@localhost:5432/shop\n"
            ),
        ),
        (
            "compose.yaml",
            r#"services: {db: {image: postgres, ports: ["${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"]}}"#
                .to_string(),
        ),
        ("server.js", server_js),
    ] {
        let file = root.join(rel);
        if let Some(parent) = file.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(&file, content).unwrap();
    }

    run_lodo(Some(&root), &bin, &["dev"]);

    #[derive(Deserialize)]
    #[serde(rename_all = "camelCase")]
    struct GotResult {
        status: u16,
        via: String,
        host: String,
        local_address: String,
        #[serde(rename = "dockerHostIP")]
        docker_host_ip: String,
        #[serde(rename = "apiURL")]
        api_url: String,
        #[serde(rename = "databaseURL")]
        database_url: String,
        vite_hosts: String,
        doctor_out: String,
        apply_out: String,
        ls_json: String,
        menubar_dump: String,
    }

    let got: GotResult =
        serde_json::from_slice(&fs::read(&result_file).expect("read result.json")).unwrap();
    assert_eq!(got.status, 200);
    assert!(got.via.contains("Caddy"));
    assert_eq!(got.host, "web.shop.lodo");
    assert_eq!(got.local_address, "127.0.1.1");
    assert_eq!(got.docker_host_ip, "127.0.1.1");
    assert_eq!(got.api_url, "http://web.shop.lodo");
    assert_eq!(got.database_url, "postgres://u:hunter2@shop.lodo:5432/shop");
    assert_eq!(got.vite_hosts, ".shop.lodo");
    assert!(got.doctor_out.contains("✓ 7 names resolve"));
    assert!(got.doctor_out.contains("✓ 8 caddy"));
    assert!(got.apply_out.contains("✓ shop.lodo"));
    assert!(got.apply_out.contains("✓ web.shop.lodo"));

    let ls_report: LsReport = serde_json::from_str(&got.ls_json).unwrap();
    assert!(ls_report.setup_done);
    assert!(
        got.menubar_dump.contains("shop.lodo · 127.0.1.1")
            && got
                .menubar_dump
                .contains(&format!("● web.shop.lodo  (http :{app_port})"))
    );

    // 3. lodo exec exports variables without the listen hook.
    let exec_out = run_lodo(
        Some(&root),
        &bin,
        &[
            "exec",
            "node",
            "-e",
            r#"console.log(JSON.stringify({ip: process.env.DOCKER_HOST_IP, api: process.env.API_URL, opts: process.env.NODE_OPTIONS || ""}))"#,
        ],
    );
    #[derive(Deserialize)]
    struct ExecGot {
        ip: String,
        api: String,
        opts: String,
    }
    let exec_got: ExecGot = serde_json::from_str(exec_out.trim()).unwrap();
    assert_eq!(exec_got.ip, "127.0.1.1");
    assert_eq!(exec_got.api, "http://web.shop.lodo");
    assert!(!exec_got.opts.contains("bind.cjs"));

    // 4. lodo prune removes names when folder is deleted.
    fs::remove_dir_all(&root).unwrap();
    let prune_out = run_lodo(None, &bin, &["prune"]);
    assert!(prune_out.contains("shop.lodo") && prune_out.contains("web.shop.lodo"));
    assert!(!p.resolver_dir.join("shop.lodo").exists());

    // 5. lodo uninstall removes all system parts.
    let uninstall_out = run_lodo(None, &bin, &["uninstall"]);
    assert!(uninstall_out.contains("✓ sudoers rule and resolver script removed"));
    for gone in [&p.script, &p.sudoers, &p.loopback_plist, &p.menubar_plist] {
        assert!(!gone.exists(), "{} still exists", gone.display());
    }
    let doctor_after = Command::new(&bin).arg("doctor").status().unwrap();
    assert!(!doctor_after.success());
}
