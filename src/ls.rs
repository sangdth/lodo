//! Plain-text and ANSI-colored table formatter (plus `--json` output) for `lodo ls`.

use std::collections::HashMap;
use std::fmt::Write as _;
use std::path::Path;

use serde::{Deserialize, Serialize};
use unicode_width::UnicodeWidthStr;

use crate::check::{self, Check, ProbeResult};
use crate::store::{self, Domain};
use crate::{fsutil, menubar, system};

const GREEN: &str = "\x1b[32m";
const DIM: &str = "\x1b[90m";
const RED: &str = "\x1b[31m";
const RESET: &str = "\x1b[0m";

/// Complete system and per-domain snapshot returned by `lodo ls --json` and consumed by `menubar.swift`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LsReport {
    pub setup_done: bool,
    pub caddy_ready: bool,
    pub next_free: Option<String>,
    pub log_path: String,
    pub services: Vec<LsService>,
    pub domains: Vec<LsDomain>,
}

/// Status of one system component in [`LsReport`].
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LsService {
    /// `"caddy"`, `"dnsmasq"`, `"menubar"`, `"loopback"`, or `"resolvers"`.
    pub name: String,
    /// `"on"`, `"off"`, or `"error"`.
    pub state: String,
    pub detail: String,
    pub fix: String,
}

/// Configuration and live probe outcome of one listed domain in [`LsReport`].
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LsDomain {
    pub name: String,
    pub project: String,
    pub address: String,
    pub port: u16,
    pub enabled: bool,
    pub https: bool,
    pub compose: String,
    pub root: String,
    /// `"ok"`, `"down"`, `"error"`, or `"off"`.
    pub status: String,
    pub https_ok: bool,
    pub detail: String,
}

/// Runs doctor checks, probes enabled domains, and assembles an [`LsReport`].
pub fn build_report(env: &check::Env<'_>, domains: &[Domain]) -> LsReport {
    let (checks, results) = env.report(domains);
    let menubar_on = menubar::running(env.runner, env.paths);

    let setup_done = system::setup_done(env.paths);
    let caddy_ready = fsutil::exists(&env.paths.caddy) && system::caddy_set_up(env.paths);
    let next_free = store::next_free(domains).ok();
    let log_path = env.paths.log.to_string_lossy().into_owned();

    let find_check = |id: usize| -> Option<&Check> { checks.iter().find(|c| c.id == id) };
    let service_from_check = |name: &str, id: usize| -> LsService {
        match find_check(id) {
            Some(c) => {
                let state = if c.ok {
                    "on"
                } else if c.skipped {
                    "off"
                } else {
                    "error"
                };
                LsService {
                    name: name.to_string(),
                    state: state.to_string(),
                    detail: c.detail.clone(),
                    fix: c.fix.clone(),
                }
            }
            None => LsService {
                name: name.to_string(),
                state: "error".to_string(),
                detail: String::new(),
                fix: String::new(),
            },
        }
    };

    let services = vec![
        service_from_check("caddy", check::ID_CADDY),
        service_from_check("dnsmasq", check::ID_DNSMASQ),
        LsService {
            name: "menubar".to_string(),
            state: if menubar_on { "on" } else { "off" }.to_string(),
            detail: if menubar_on { "running" } else { "stopped" }.to_string(),
            fix: String::new(),
        },
        service_from_check("loopback", check::ID_LOOPBACK),
        service_from_check("resolvers", check::ID_RESOLVERS),
    ];

    let dnsmasq_off = find_check(check::ID_DNSMASQ).is_some_and(|c| c.off);
    let caddy_off = find_check(check::ID_CADDY).is_some_and(|c| c.off);
    let results_by_name: HashMap<&str, &ProbeResult> =
        results.iter().map(|r| (r.name.as_str(), r)).collect();

    let sorted = store::sort(domains);
    let mut out_domains = Vec::with_capacity(sorted.len());
    for d in sorted {
        let project = store::project(&d.name).to_string();
        let (status, https_ok, detail) = if !d.enabled {
            ("off".to_string(), false, String::new())
        } else if let Some(r) = results_by_name.get(d.name.as_str()) {
            let expected_off = dnsmasq_off || (caddy_off && r.direct && r.system);
            let st = if r.ok() {
                "ok"
            } else if r.down {
                "down"
            } else if expected_off {
                "off"
            } else {
                "error"
            };
            (st.to_string(), r.https, r.detail.clone())
        } else {
            ("off".to_string(), false, String::new())
        };

        out_domains.push(LsDomain {
            name: d.name,
            project,
            address: d.address,
            port: d.port,
            enabled: d.enabled,
            https: d.https,
            compose: d.compose,
            root: d.root,
            status,
            https_ok,
            detail,
        });
    }

    LsReport {
        setup_done,
        caddy_ready,
        next_free,
        log_path,
        services,
        domains: out_domains,
    }
}

/// Formats `path` relative to `home` as `~/...` when inside `home`, leaving other paths unchanged.
pub fn short_path(path: &str, home: &Path) -> String {
    let home_str = home.to_string_lossy();
    if !home_str.is_empty() {
        let prefix = format!("{}/", home_str.trim_end_matches('/'));
        if let Some(rest) = path.strip_prefix(&prefix) {
            return format!("~/{rest}");
        }
    }
    path.to_string()
}

/// Formats an [`LsReport`] as an aligned human-readable table, optionally with ANSI colors.
pub fn format_table(report: &LsReport, home: &Path, color: bool) -> String {
    let mut out = String::new();
    out.push_str("services");
    for svc in &report.services {
        let _ = write!(out, "  {} {}", svc.name, service_mark(&svc.state, color));
    }
    out.push_str("\n\n");

    if report.domains.is_empty() {
        out.push_str("no domains configured (run lodo dev in a project, or lodo add <name>)\n");
    } else {
        let headers = ["NAME", "ENABLED", "ADDRESS", "PORT", "COMPOSE", "CHECK"];
        let mut widths = [
            UnicodeWidthStr::width(headers[0]),
            UnicodeWidthStr::width(headers[1]),
            UnicodeWidthStr::width(headers[2]),
            UnicodeWidthStr::width(headers[3]),
            UnicodeWidthStr::width(headers[4]),
        ];

        let mut rows: Vec<[String; 6]> = Vec::with_capacity(report.domains.len());
        for d in &report.domains {
            let indent = "  ".repeat(d.name.matches('.').count().saturating_sub(1));
            let mark = domain_mark(&d.status, color);
            let name_col = format!("{indent}{mark} {}", d.name);
            let enabled_col = if d.enabled { "✓" } else { "–" }.to_string();
            let address_col = d.address.clone();
            let port_col = if d.port > 0 {
                d.port.to_string()
            } else {
                "–".to_string()
            };
            let compose_col = if store::has_compose(&d.compose) {
                short_path(&d.compose, home)
            } else {
                "–".to_string()
            };
            let check_col = check_cell(d);

            let row = [
                name_col,
                enabled_col,
                address_col,
                port_col,
                compose_col,
                check_col,
            ];
            for (w, cell) in widths.iter_mut().zip(&row[..5]) {
                *w = (*w).max(visible_width(cell));
            }
            rows.push(row);
        }

        write_row(&mut out, &headers, &widths);
        for row in &rows {
            let refs = [
                row[0].as_str(),
                row[1].as_str(),
                row[2].as_str(),
                row[3].as_str(),
                row[4].as_str(),
                row[5].as_str(),
            ];
            write_row(&mut out, &refs, &widths);
        }
    }

    if report.services.iter().any(|s| s.state == "error") {
        out.push_str("\n! system check failed: run lodo doctor\n");
    }

    out
}

fn write_row(out: &mut String, cols: &[&str; 6], widths: &[usize; 5]) {
    for (col, &width) in cols[..5].iter().zip(widths) {
        out.push_str(col);
        let pad = width.saturating_sub(visible_width(col));
        for _ in 0..pad {
            out.push(' ');
        }
        out.push_str("  ");
    }
    out.push_str(cols[5]);
    out.push('\n');
}

fn service_mark(state: &str, color: bool) -> String {
    match (state, color) {
        ("on", true) => format!("{GREEN}●{RESET}"),
        ("on", false) => "●".to_string(),
        ("off", true) => format!("{DIM}○{RESET}"),
        ("off", false) => "○".to_string(),
        (_, true) => format!("{RED}○{RESET}"),
        (_, false) => "○".to_string(),
    }
}

fn domain_mark(status: &str, color: bool) -> String {
    match (status, color) {
        ("ok", true) => format!("{GREEN}●{RESET}"),
        ("ok", false) => "●".to_string(),
        ("off" | "down", true) => format!("{DIM}○{RESET}"),
        ("off" | "down", false) => "○".to_string(),
        (_, true) => format!("{RED}○{RESET}"),
        (_, false) => "○".to_string(),
    }
}

fn check_cell(d: &LsDomain) -> String {
    if !d.enabled {
        return "off".to_string();
    }
    let https_only_failure =
        d.https && !d.https_ok && d.status == "error" && d.detail.starts_with("https: ");
    if d.status == "ok" || https_only_failure {
        if d.https {
            let h = if d.https_ok { "✓" } else { "✗" };
            return format!("dns ✓  https {h}");
        }
        return "dns ✓".to_string();
    }
    if d.status == "down" {
        return "app down".to_string();
    }
    if d.detail.is_empty() {
        return d.status.clone();
    }
    d.detail.clone()
}

/// Computes terminal display width while ignoring CSI ANSI escape sequences (`\x1b[...m`).
fn visible_width(s: &str) -> usize {
    let mut total = 0;
    let mut rest = s;
    while let Some(esc) = rest.find("\x1b[") {
        total += UnicodeWidthStr::width(&rest[..esc]);
        let after_esc = &rest[esc + 2..];
        if let Some(end) = after_esc.find('m') {
            rest = &after_esc[end + 1..];
        } else {
            return total;
        }
    }
    total + UnicodeWidthStr::width(rest)
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::fs::PermissionsExt;

    use nix::unistd::Uid;

    use super::*;
    use crate::paths::Paths;
    use crate::run::{self, FakeRunner};

    fn sample_services(has_error: bool) -> Vec<LsService> {
        vec![
            LsService {
                name: "caddy".into(),
                state: "on".into(),
                detail: "running, serves 2 sites".into(),
                fix: String::new(),
            },
            LsService {
                name: "dnsmasq".into(),
                state: if has_error { "error" } else { "on" }.into(),
                detail: if has_error {
                    "not running".into()
                } else {
                    "running as tester, pid 42".into()
                },
                fix: if has_error {
                    "lodo setup".into()
                } else {
                    String::new()
                },
            },
            LsService {
                name: "menubar".into(),
                state: "off".into(),
                detail: "stopped".into(),
                fix: String::new(),
            },
            LsService {
                name: "loopback".into(),
                state: "on".into(),
                detail: "50 of 50 addresses on lo0".into(),
                fix: String::new(),
            },
            LsService {
                name: "resolvers".into(),
                state: "on".into(),
                detail: "3 files".into(),
                fix: String::new(),
            },
        ]
    }

    #[test]
    fn format_table_empty_and_error_footer() {
        let home = Path::new("/Users/tester");
        let report_ok = LsReport {
            setup_done: true,
            caddy_ready: true,
            next_free: Some("127.0.1.1".into()),
            log_path: "/Users/tester/.config/lodo/dnsmasq.log".into(),
            services: sample_services(false),
            domains: Vec::new(),
        };
        let plain = format_table(&report_ok, home, false);
        assert_eq!(
            plain,
            concat!(
                "services  caddy ●  dnsmasq ●  menubar ○  loopback ●  resolvers ●\n",
                "\n",
                "no domains configured (run lodo dev in a project, or lodo add <name>)\n"
            )
        );

        let colored = format_table(&report_ok, home, true);
        assert!(colored.starts_with(
            "services  caddy \x1b[32m●\x1b[0m  dnsmasq \x1b[32m●\x1b[0m  menubar \x1b[90m○\x1b[0m  loopback \x1b[32m●\x1b[0m  resolvers \x1b[32m●\x1b[0m\n"
        ));

        let report_err = LsReport {
            services: sample_services(true),
            ..report_ok
        };
        let err_plain = format_table(&report_err, home, false);
        assert!(err_plain.contains("dnsmasq ○"));
        assert!(err_plain.ends_with("\n! system check failed: run lodo doctor\n"));

        let err_color = format_table(&report_err, home, true);
        assert!(err_color.contains("dnsmasq \x1b[31m○\x1b[0m"));
        assert!(err_color.ends_with("\n! system check failed: run lodo doctor\n"));
    }

    #[test]
    fn format_table_aligns_columns_indents_subdomains_and_handles_ansi() {
        let home = Path::new("/Users/tester");
        let report = LsReport {
            setup_done: true,
            caddy_ready: true,
            next_free: Some("127.0.1.2".into()),
            log_path: "/Users/tester/.config/lodo/dnsmasq.log".into(),
            services: sample_services(false),
            domains: vec![
                LsDomain {
                    name: "crm.lodo".into(),
                    project: "crm.lodo".into(),
                    address: "127.0.1.1".into(),
                    port: 0,
                    enabled: true,
                    https: false,
                    compose: "/Users/tester/Projects/crm/docker/compose.dev.yaml".into(),
                    root: "/Users/tester/Projects/crm".into(),
                    status: "ok".into(),
                    https_ok: false,
                    detail: String::new(),
                },
                LsDomain {
                    name: "api.crm.lodo".into(),
                    project: "crm.lodo".into(),
                    address: "127.0.1.1".into(),
                    port: 4000,
                    enabled: true,
                    https: false,
                    compose: String::new(),
                    root: String::new(),
                    status: "down".into(),
                    https_ok: false,
                    detail: "app down: nothing answers on 127.0.1.1:4000".into(),
                },
                LsDomain {
                    name: "v1.api.crm.lodo".into(),
                    project: "crm.lodo".into(),
                    address: "127.0.1.1".into(),
                    port: 4001,
                    enabled: true,
                    https: true,
                    compose: "none".into(),
                    root: String::new(),
                    status: "ok".into(),
                    https_ok: true,
                    detail: String::new(),
                },
                LsDomain {
                    name: "web.crm.lodo".into(),
                    project: "crm.lodo".into(),
                    address: "127.0.1.1".into(),
                    port: 3000,
                    enabled: true,
                    https: true,
                    compose: String::new(),
                    root: String::new(),
                    status: "error".into(),
                    https_ok: false,
                    detail: "https: Caddy's certificate isn't trusted: run lodo setup".into(),
                },
                LsDomain {
                    name: "broken.lodo".into(),
                    project: "broken.lodo".into(),
                    address: "127.0.1.3".into(),
                    port: 0,
                    enabled: true,
                    https: false,
                    compose: String::new(),
                    root: String::new(),
                    status: "error".into(),
                    https_ok: false,
                    detail: "macOS: no address".into(),
                },
                LsDomain {
                    name: "old.lodo".into(),
                    project: "old.lodo".into(),
                    address: "127.0.0.1".into(),
                    port: 5000,
                    enabled: false,
                    https: false,
                    compose: String::new(),
                    root: String::new(),
                    status: "off".into(),
                    https_ok: false,
                    detail: String::new(),
                },
            ],
        };

        let plain = format_table(&report, home, false);
        let want_plain = concat!(
            "services  caddy ●  dnsmasq ●  menubar ○  loopback ●  resolvers ●\n",
            "\n",
            "NAME                   ENABLED  ADDRESS    PORT  COMPOSE                                 CHECK\n",
            "● crm.lodo             ✓        127.0.1.1  –     ~/Projects/crm/docker/compose.dev.yaml  dns ✓\n",
            "  ○ api.crm.lodo       ✓        127.0.1.1  4000  –                                       app down\n",
            "    ● v1.api.crm.lodo  ✓        127.0.1.1  4001  –                                       dns ✓  https ✓\n",
            "  ○ web.crm.lodo       ✓        127.0.1.1  3000  –                                       dns ✓  https ✗\n",
            "○ broken.lodo          ✓        127.0.1.3  –     –                                       macOS: no address\n",
            "○ old.lodo             –        127.0.0.1  5000  –                                       off\n",
        );
        assert_eq!(plain, want_plain);

        let col_of = |line: &str, needle: &str| -> usize {
            let byte_idx = line.find(needle).unwrap();
            UnicodeWidthStr::width(&line[..byte_idx])
        };
        let lines: Vec<&str> = plain.lines().collect();
        assert_eq!(col_of(lines[2], "ENABLED"), col_of(lines[3], "✓"));
        assert_eq!(col_of(lines[2], "ENABLED"), col_of(lines[5], "✓"));
        assert_eq!(col_of(lines[2], "CHECK"), col_of(lines[3], "dns ✓"));
        assert_eq!(col_of(lines[2], "CHECK"), col_of(lines[4], "app down"));
        assert_eq!(
            col_of(lines[2], "CHECK"),
            col_of(lines[5], "dns ✓  https ✓")
        );
        assert_eq!(
            col_of(lines[2], "CHECK"),
            col_of(lines[6], "dns ✓  https ✗")
        );
        assert_eq!(
            col_of(lines[2], "CHECK"),
            col_of(lines[7], "macOS: no address")
        );
        assert_eq!(col_of(lines[2], "CHECK"), col_of(lines[8], "off"));

        // Colored output strips down to the exact same visible text as plain output.
        let colored = format_table(&report, home, true);
        assert!(colored.contains("\x1b[32m●\x1b[0m crm.lodo"));
        assert!(colored.contains("  \x1b[90m○\x1b[0m api.crm.lodo"));
        assert!(colored.contains("  \x1b[31m○\x1b[0m web.crm.lodo"));
        let stripped = colored
            .replace("\x1b[32m", "")
            .replace("\x1b[90m", "")
            .replace("\x1b[31m", "")
            .replace("\x1b[0m", "");
        assert_eq!(stripped, plain);
    }

    #[test]
    fn build_report_roundtrips_json_and_handles_expected_off() {
        let tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(tmp.path());
        let r = FakeRunner::new();

        // Set up fake Mac where dnsmasq is intentionally turned off (no plist, launchctl print fails).
        for (path, content) in [
            (&p.dnsmasq, String::new()),
            (&p.caddy, String::new()),
            (&p.system_conf, system::rewrite_system_conf("", &p)),
            (
                &p.system_caddyfile,
                system::rewrite_system_caddyfile("", &p),
            ),
            (&p.script, system::script(&p)),
        ] {
            if let Some(dir) = path.parent() {
                fs::create_dir_all(dir).unwrap();
            }
            fs::write(path, content).unwrap();
        }
        fs::set_permissions(&p.script, fs::Permissions::from_mode(0o755)).unwrap();

        let launchctl = p.launchctl.to_string_lossy().into_owned();
        for label in system::DNS_SYSTEM_LABELS {
            r.fail(
                run::line(&launchctl, &["print", &format!("system/{label}")]),
                "not found",
            );
        }
        r.fail(
            run::line(&launchctl, &["print", "gui/501/sh.brew.dnsmasq"]),
            "not found",
        );
        r.fail(
            run::line(&launchctl, &["print", "gui/501/sh.brew.caddy"]),
            "not found",
        );
        r.fail(
            run::line(&launchctl, &["print", "gui/501/io.lodo.menubar"]),
            "not found",
        );
        let mut lo0 = String::new();
        for i in store::OWN_FIRST..=store::OWN_LAST {
            let _ = writeln!(lo0, "\tinet {} netmask 0xff000000", store::own_address(i));
        }
        r.set(run::line(&p.ifconfig.to_string_lossy(), &["lo0"]), lo0);

        let domains = vec![
            Domain {
                name: "shop.lodo".into(),
                address: "127.0.1.1".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "old.lodo".into(),
                address: "127.0.1.2".into(),
                enabled: false,
                ..Domain::default()
            },
        ];

        let mut env = check::Env::new(&p, &r);
        env.root_uid = Uid::current().as_raw();
        env.dns = "127.0.0.1:1".into();
        env.timeout = Some(std::time::Duration::from_millis(20));

        let report = build_report(&env, &domains);
        assert!(report.setup_done);
        assert!(report.caddy_ready);
        assert_eq!(report.next_free.as_deref(), Some("127.0.1.3"));
        assert_eq!(
            report
                .services
                .iter()
                .map(|s| s.name.as_str())
                .collect::<Vec<_>>(),
            vec!["caddy", "dnsmasq", "menubar", "loopback", "resolvers"]
        );
        assert_eq!(report.services[1].state, "off"); // dnsmasq turned off -> skipped -> "off"
        assert_eq!(report.services[2].state, "off"); // menubar off
        assert_eq!(report.services[3].state, "on"); // loopback ok
        assert_eq!(report.services[4].state, "on"); // resolvers ok while dnsmasq off

        // Sorted order: old.lodo, shop.lodo; both have status "off" (old disabled, shop expected-off).
        assert_eq!(report.domains.len(), 2);
        assert_eq!(report.domains[0].name, "old.lodo");
        assert_eq!(report.domains[0].status, "off");
        assert_eq!(report.domains[1].name, "shop.lodo");
        assert_eq!(report.domains[1].status, "off");

        let json = serde_json::to_string_pretty(&report).unwrap();
        let decoded: LsReport = serde_json::from_str(&json).unwrap();
        assert_eq!(decoded, report);
    }
}
