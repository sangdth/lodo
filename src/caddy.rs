//! `Caddyfile` generation, `caddy validate`, and log error extraction.

use std::fmt::Write as _;
use std::fs::{self, File};
use std::io::{Read as _, Seek as _, SeekFrom};
use std::path::Path;
use std::time::SystemTime;

use serde::Deserialize;

use crate::run::Runner;
use crate::store::{self, Domain};

/// Aborts connections coming from outside loopback without sending a response.
const LOCAL_ONLY: &str = "\t@outside not remote_ip 127.0.0.0/8 ::1\n\tabort @outside\n";

/// Maximum tail window `log_error` inspects at the end of Caddy's log.
const LOG_TAIL: u64 = 64 << 10;

/// Generates `lodo`'s `Caddyfile`: one site block per enabled domain with a port,
/// sorted in `store::sort` order.
pub fn config(domains: &[Domain]) -> String {
    let mut b = String::from(store::GENERATED_HEADER);
    for d in store::sort(domains) {
        if !d.has_site() {
            continue;
        }
        if d.https {
            let _ = write!(
                b,
                "\nhttp://{}, https://{} {{\n{LOCAL_ONLY}\ttls internal\n{}}}\n",
                d.name,
                d.name,
                proxy(&d)
            );
        } else {
            let _ = write!(b, "\nhttp://{} {{\n{LOCAL_ONLY}{}}}\n", d.name, proxy(&d));
        }
    }
    b
}

fn proxy(d: &Domain) -> String {
    let scheme = if d.https { "https?" } else { "http" };
    let escaped = d.name.replace('.', "\\.");
    format!(
        "\t@own_dev path */_next/* */__nextjs*\n\
         \trequest_header @own_dev Origin \"^{scheme}://{escaped}$\" http://localhost:{}\n\
         \treverse_proxy {}:{}\n",
        d.port, d.address, d.port
    )
}

/// Runs `caddy validate` on the `Caddyfile` at `path`. When `caddy` rejects it,
/// extracts Caddy's own error message without surrounding log lines.
pub fn validate(r: &dyn Runner, caddy_bin: &Path, path: &Path) -> Result<(), String> {
    let bin = caddy_bin.to_string_lossy();
    let cfg = path.to_string_lossy();
    match r.run(
        None,
        &bin,
        &["validate", "--config", &cfg, "--adapter", "caddyfile"],
    ) {
        Ok(_) => Ok(()),
        Err(re) => {
            let msg = last_error(&re.stderr);
            if !msg.is_empty() {
                Err(format!("caddy validate: {msg}"))
            } else {
                Err(format!("caddy validate: {re}"))
            }
        }
    }
}

/// Returns the last error message in the final `LOG_TAIL` bytes of Caddy's log at `path`,
/// or `""` when there is none, the log cannot be read, or it has not been modified since `since`.
pub fn log_error(path: &Path, since: SystemTime) -> String {
    // Stat first: opening a FIFO blocks.
    let Ok(info) = fs::metadata(path) else {
        return String::new();
    };
    if !info.file_type().is_file() {
        return String::new();
    }
    let Ok(modified) = info.modified() else {
        return String::new();
    };
    if modified < since {
        return String::new();
    }

    let Ok(mut f) = File::open(path) else {
        return String::new();
    };
    let start = info.len().saturating_sub(LOG_TAIL);
    if f.seek(SeekFrom::Start(start)).is_err() {
        return String::new();
    }
    let mut tail = Vec::new();
    if f.take(LOG_TAIL).read_to_end(&mut tail).is_err() {
        return String::new();
    }
    last_error(&String::from_utf8_lossy(&tail))
}

#[derive(Deserialize)]
struct LogEntry {
    level: Option<String>,
    msg: Option<String>,
}

fn last_error(stderr: &str) -> String {
    let mut msg = String::new();
    for raw in stderr.lines() {
        let line = raw.trim();
        if let Some(text) = line.strip_prefix("Error:") {
            msg = text.trim().to_string();
            continue;
        }
        if let Ok(entry) = serde_json::from_str::<LogEntry>(line)
            && entry.level.as_deref() == Some("error")
            && let Some(m) = entry.msg
            && !m.is_empty()
        {
            msg = m;
        }
    }
    msg
}

#[cfg(test)]
mod tests {
    use std::io::{BufRead, BufReader, Write as _};
    use std::net::{TcpListener, TcpStream};
    use std::process::{Child, Command, Stdio};
    use std::thread;
    use std::time::Duration;

    use super::*;
    use crate::paths::Paths;
    use crate::run::FakeRunner;

    const CADDY_BIN: &str = "/opt/homebrew/bin/caddy";

    fn sample_domains() -> Vec<Domain> {
        vec![
            Domain {
                name: "flowy.test".into(),
                address: "127.0.1.3".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "service.crm.test".into(),
                address: "127.0.1.1".into(),
                port: 3002,
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
                name: "old.test".into(),
                address: "127.0.0.1".into(),
                ..Domain::default()
            },
            Domain {
                name: "secure.crm.test".into(),
                address: "127.0.1.1".into(),
                port: 3004,
                enabled: true,
                https: true,
                ..Domain::default()
            },
            Domain {
                name: "old-secure.crm.test".into(),
                address: "127.0.1.1".into(),
                port: 3005,
                https: true,
                ..Domain::default()
            },
            Domain {
                name: "docs.crm.test".into(),
                address: "127.0.1.1".into(),
                port: 3003,
                ..Domain::default()
            },
            Domain {
                name: "a.api.crm.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "test.crm.test".into(),
                address: "127.0.1.4".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "api.crm.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
        ]
    }

    fn no_ports_domains() -> Vec<Domain> {
        vec![
            Domain {
                name: "crm.test".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "flowy.test".into(),
                address: "127.0.1.3".into(),
                enabled: true,
                ..Domain::default()
            },
        ]
    }

    #[test]
    fn config_matches_go_goldens() {
        let sample_golden = include_str!("../tests/testdata/caddy/TestConfig/sample.golden");
        let no_ports_golden = include_str!("../tests/testdata/caddy/TestConfig/no_ports.golden");

        assert_eq!(config(&sample_domains()), sample_golden);
        assert_eq!(config(&no_ports_domains()), no_ports_golden);
    }

    #[test]
    fn config_caddy_accepts() {
        let bin = Path::new(CADDY_BIN);
        if !bin.is_file() {
            return;
        }
        let hyphenated = vec![Domain {
            name: "my-app.crm.test".into(),
            address: "127.0.1.1".into(),
            port: 3000,
            enabled: true,
            ..Domain::default()
        }];
        for (name, domains) in [
            ("sample", sample_domains()),
            ("no ports", no_ports_domains()),
            ("hyphenated", hyphenated),
        ] {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join("Caddyfile");
            let rendered = config(&domains);
            fs::write(&path, &rendered).unwrap();

            let out = Command::new(bin)
                .args(["validate", "--config"])
                .arg(&path)
                .args(["--adapter", "caddyfile"])
                .env("HOME", dir.path())
                .env("XDG_DATA_HOME", dir.path())
                .env("XDG_CONFIG_HOME", dir.path())
                .output()
                .unwrap();
            assert!(
                out.status.success(),
                "case {name}: caddy rejected config:\n{}\n{}\nconfig:\n{rendered}",
                String::from_utf8_lossy(&out.stdout),
                String::from_utf8_lossy(&out.stderr),
            );
        }
    }

    struct ChildGuard(Child);

    impl Drop for ChildGuard {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }

    #[test]
    fn config_origin_rewrite_live() {
        let bin = Path::new(CADDY_BIN);
        if !bin.is_file() {
            return;
        }

        let upstream = TcpListener::bind("127.0.0.1:0").unwrap();
        let up_port = upstream.local_addr().unwrap().port();
        thread::spawn(move || {
            for stream in upstream.incoming().flatten() {
                let mut reader = BufReader::new(&stream);
                let mut got_host = String::new();
                let mut got_origin = String::new();
                loop {
                    let mut line = String::new();
                    if reader.read_line(&mut line).unwrap_or(0) == 0 || line == "\r\n" {
                        break;
                    }
                    let trimmed = line.trim_end_matches(['\r', '\n']);
                    if let Some((k, v)) = trimmed.split_once(':') {
                        if k.eq_ignore_ascii_case("host") {
                            got_host = v.trim().to_string();
                        } else if k.eq_ignore_ascii_case("origin") {
                            got_origin = v.trim().to_string();
                        }
                    }
                }
                let mut writer = &stream;
                let _ = write!(
                    writer,
                    "HTTP/1.1 200 OK\r\nX-Got-Host: {got_host}\r\nX-Got-Origin: {got_origin}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                );
            }
        });

        let probe = TcpListener::bind("127.0.0.1:0").unwrap();
        let caddy_port = probe.local_addr().unwrap().port();
        drop(probe);

        let dir = tempfile::tempdir().unwrap();
        let sites = dir.path().join("lodo.Caddyfile");
        fs::write(
            &sites,
            config(&[Domain {
                name: "dashboard.crm.test".into(),
                address: "127.0.0.1".into(),
                port: up_port,
                enabled: true,
                ..Domain::default()
            }]),
        )
        .unwrap();
        let main_cf = dir.path().join("Caddyfile");
        let quoted_sites = serde_json::to_string(&sites.to_string_lossy()).unwrap();
        fs::write(
            &main_cf,
            format!("{{\n\tadmin off\n\thttp_port {caddy_port}\n}}\nimport {quoted_sites}\n"),
        )
        .unwrap();

        let child = Command::new(bin)
            .args(["run", "--config"])
            .arg(&main_cf)
            .args(["--adapter", "caddyfile"])
            .env("HOME", dir.path())
            .env("XDG_DATA_HOME", dir.path())
            .env("XDG_CONFIG_HOME", dir.path())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let _guard = ChildGuard(child);

        let send_req = |path: &str, origin: &str| -> Option<(String, String)> {
            let mut stream = TcpStream::connect(("127.0.0.1", caddy_port)).ok()?;
            stream.set_read_timeout(Some(Duration::from_secs(2))).ok()?;
            write!(
                stream,
                "GET {path} HTTP/1.1\r\nHost: dashboard.crm.test\r\nOrigin: {origin}\r\nConnection: close\r\n\r\n"
            )
            .ok()?;
            let mut resp = String::new();
            stream.read_to_string(&mut resp).ok()?;
            if !resp.starts_with("HTTP/1.1 200") {
                return None;
            }
            let mut host = String::new();
            let mut got_origin = String::new();
            for line in resp.lines() {
                if let Some((k, v)) = line.split_once(':') {
                    if k.eq_ignore_ascii_case("x-got-host") {
                        host = v.trim().to_string();
                    } else if k.eq_ignore_ascii_case("x-got-origin") {
                        got_origin = v.trim().to_string();
                    }
                }
            }
            Some((host, got_origin))
        };

        let mut ready = None;
        for _ in 0..40 {
            if let Some(pair) = send_req("/_next/webpack-hmr", "http://dashboard.crm.test") {
                ready = Some(pair);
                break;
            }
            thread::sleep(Duration::from_millis(50));
        }
        let (got_host, got_origin) = ready.expect("caddy did not start listening");
        assert_eq!(got_origin, format!("http://localhost:{up_port}"));
        assert_eq!(got_host, "dashboard.crm.test");

        let (_, evil_origin) =
            send_req("/_next/webpack-hmr", "https://evil.example").expect("evil request failed");
        assert_eq!(evil_origin, "https://evil.example");
    }

    #[test]
    fn validate_extracts_caddy_errors_and_falls_back_to_runner_error() {
        struct Case<'a> {
            name: &'a str,
            fail: bool,
            stderr: &'a str,
            want_err: Option<&'a str>,
        }

        let json_stderr = concat!(
            r#"{"level":"info","ts":1791468283.708788,"msg":"using config from file","file":"/Users/tester/.config/lodo/Caddyfile"}"#,
            "\n",
            r#"{"level":"error","ts":1791468283.709107,"msg":"adapting config using caddyfile: /Users/tester/.config/lodo/Caddyfile:4: unrecognized directive: revers_proxy"}"#
        );
        let plain_stderr = concat!(
            r#"{"level":"info","ts":1791468324.516309,"msg":"using config from file","file":"/Users/tester/.config/lodo/Caddyfile"}"#,
            "\n",
            r#"{"level":"warn","ts":1791468324.516472,"msg":"no handler"}"#,
            "\n",
            "Error: adapting config using caddyfile: ambiguous site definition: http://crm.test"
        );
        let info_only_stderr =
            r#"{"level":"info","ts":1791468324.645348,"msg":"using config from file"}"#;

        let cases = [
            Case {
                name: "valid",
                fail: false,
                stderr: "",
                want_err: None,
            },
            Case {
                name: "caddy 2.11 logs the error as JSON",
                fail: true,
                stderr: json_stderr,
                want_err: Some(
                    "caddy validate: adapting config using caddyfile: /Users/tester/.config/lodo/Caddyfile:4: unrecognized directive: revers_proxy",
                ),
            },
            Case {
                name: "plain Error line after JSON logs",
                fail: true,
                stderr: plain_stderr,
                want_err: Some(
                    "caddy validate: adapting config using caddyfile: ambiguous site definition: http://crm.test",
                ),
            },
            Case {
                name: "no error message in stderr",
                fail: true,
                stderr: info_only_stderr,
                want_err: None,
            },
            Case {
                name: "empty stderr",
                fail: true,
                stderr: "",
                want_err: None,
            },
        ];

        for tc in cases {
            let tmp = tempfile::tempdir().unwrap();
            let p = Paths::for_test(tmp.path());
            let line = format!(
                "{} validate --config {} --adapter caddyfile",
                p.caddy.display(),
                p.caddyfile.display()
            );
            let f = FakeRunner::new();
            if tc.fail {
                f.fail(&line, tc.stderr);
            }

            let res = validate(&f, &p.caddy, &p.caddyfile);
            assert_eq!(f.calls(), vec![line.clone()], "case {}", tc.name);
            if !tc.fail {
                assert_eq!(res, Ok(()), "case {}", tc.name);
                continue;
            }

            let want = match tc.want_err {
                Some(w) => w.to_string(),
                None => {
                    let detail = if tc.stderr.is_empty() {
                        "exit status 1"
                    } else {
                        tc.stderr
                    };
                    format!("caddy validate: {line}: {detail}")
                }
            };
            assert_eq!(res, Err(want), "case {}", tc.name);
        }
    }

    #[test]
    fn log_error_reads_tail_and_respects_since_and_file_type() {
        let start = SystemTime::now();
        let dir = tempfile::tempdir().unwrap();
        let write = |name: &str, content: &str| {
            let path = dir.path().join(name);
            fs::write(&path, content.as_bytes()).unwrap();
            path
        };

        let mut long = r#"{"level":"error","msg":"too old to read"}
"#
        .repeat(2000);
        long.push_str("{\"level\":\"error\",\"msg\":\"bind: permission denied\"}\n");
        long.push_str("{\"level\":\"info\",\"msg\":\"servers shutting down\"}\n");

        let caddy_log = write("caddy.log", &long);
        let quiet_log = write("quiet.log", "{\"level\":\"info\",\"msg\":\"serving\"}\n");
        let old_log = write("old.log", &long);
        let missing_log = dir.path().join("none.log");

        let future = SystemTime::now() + Duration::from_secs(3600);
        let cases = [
            (
                "last error",
                caddy_log.as_path(),
                start,
                "bind: permission denied",
            ),
            ("no error", quiet_log.as_path(), start, ""),
            ("written before since", old_log.as_path(), future, ""),
            ("missing", missing_log.as_path(), start, ""),
            ("a directory", dir.path(), start, ""),
        ];

        for (name, path, since, want) in cases {
            let adjusted = since - Duration::from_secs(1);
            assert_eq!(log_error(path, adjusted), want, "case {name}");
        }
    }
}
