//! Eight `lodo doctor` checks and concurrent DNS/macOS/HTTP/HTTPS probes.

pub mod probe;
#[cfg(test)]
mod tests;

use std::collections::{HashMap, HashSet};
use std::fs;
use std::io;
use std::os::unix::fs::MetadataExt;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use rustls::RootCertStore;
use serde::Serialize;

use crate::paths::Paths;
use crate::run::Runner;
use crate::store::{self, Domain};
use crate::{brew, caddy, dnsmasq, fsutil, system};

pub use probe::ProbeResult;

pub const ID_DNSMASQ: usize = 1;
pub const ID_CONFIG: usize = 2;
pub const ID_LOOPBACK: usize = 3;
pub const ID_RESOLVERS: usize = 4;
pub const ID_RESOLVER_LOCAL: usize = 5;
pub const ID_GENERATED: usize = 6;
pub const ID_NAMES: usize = 7;
pub const ID_CADDY: usize = 8;

const PREREQUISITES: usize = ID_RESOLVER_LOCAL;

const FIX_SETUP: &str = "lodo setup";
const FIX_APPLY: &str = "lodo apply";
const FIX_DNSMASQ_ON: &str = "turn dnsmasq on: lodo service dnsmasq on";
const FIX_DEV: &str = "stop the app, then start it with lodo dev in its project";

const CADDY_OFF: &str = "turned off: no http://name.lodo reaches its port until it is on";

/// Outcome of one `lodo doctor` check.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize)]
pub struct Check {
    pub id: usize,
    pub name: String,
    pub ok: bool,
    pub skipped: bool,
    pub off: bool,
    pub detail: String,
    pub fix: String,
}

/// Environment for running doctor checks and per-domain probes.
pub struct Env<'a> {
    pub paths: &'a Paths,
    pub runner: &'a dyn Runner,
    pub dns: String,
    pub http_port: u16,
    pub https_port: u16,
    pub root_cas: Option<Arc<RootCertStore>>,
    pub root_uid: u32,
    pub localhost: Vec<String>,
    pub timeout: Option<Duration>,
}

impl<'a> Env<'a> {
    pub fn new(paths: &'a Paths, runner: &'a dyn Runner) -> Self {
        Self {
            paths,
            runner,
            dns: format!("{}:{}", dnsmasq::LISTEN_ADDRESS, dnsmasq::PORT),
            http_port: store::PROXY_PORT,
            https_port: store::TLS_PORT,
            root_cas: None,
            root_uid: 0,
            localhost: vec!["127.0.0.1".into(), "::1".into()],
            timeout: None,
        }
    }

    pub fn run(&self, domains: &[Domain]) -> Vec<Check> {
        self.report(domains).0
    }

    pub fn report(&self, domains: &[Domain]) -> (Vec<Check>, Vec<ProbeResult>) {
        let results = self.probe(domains);
        let checks = self.run_n(domains, &results, ID_CADDY);
        (checks, results)
    }

    pub fn prerequisites(&self, domains: &[Domain]) -> Vec<Check> {
        self.run_n(domains, &[], PREREQUISITES)
    }

    fn run_n(&self, domains: &[Domain], results: &[ProbeResult], n: usize) -> Vec<Check> {
        let mut dnsmasq_off = false;
        let mut out = Vec::with_capacity(n);
        for id in 1..=n {
            let (name, mut c) = match id {
                ID_DNSMASQ => ("dnsmasq", self.check_dnsmasq()),
                ID_CONFIG => ("dnsmasq config", self.check_config()),
                ID_LOOPBACK => ("loopback", self.check_loopback()),
                ID_RESOLVERS => ("resolvers", self.check_resolvers(domains, dnsmasq_off)),
                ID_RESOLVER_LOCAL => ("resolver for .local", self.check_resolver_local()),
                ID_GENERATED => ("generated files", self.check_generated(domains)),
                ID_NAMES => ("names resolve", self.check_names(results, dnsmasq_off)),
                _ => ("caddy", self.check_caddy(domains)),
            };
            c.id = id;
            c.name = name.to_string();
            if id == ID_DNSMASQ {
                dnsmasq_off = c.off;
            }
            out.push(c);
        }
        out
    }

    fn check_dnsmasq(&self) -> Check {
        let p = self.paths;
        if !fsutil::exists(&p.dnsmasq) {
            return fail("not installed", "brew install dnsmasq");
        }
        let launchctl = p.launchctl.to_string_lossy();
        for label in system::DNS_SYSTEM_LABELS {
            let sys_target = format!("system/{label}");
            if self
                .runner
                .run(None, &launchctl, &["print", &sys_target])
                .is_ok()
            {
                return fail(
                    format!("a root job, {sys_target}, runs dnsmasq and shadows lodo's"),
                    FIX_SETUP,
                );
            }
        }
        let st = brew::info(self.runner, p, "dnsmasq");
        if st.off() {
            return off(format!(
                "turned off: no {} name resolves until it is on",
                store::SUFFIX
            ));
        }
        if !st.user.is_empty() && st.user != p.user {
            return fail(format!("runs as {}", st.user), FIX_SETUP);
        }
        if !st.running {
            return fail("not running", FIX_SETUP);
        }
        pass(format!("running as {}, pid {}", p.user, st.pid))
    }

    fn check_config(&self) -> Check {
        let p = self.paths;
        let content = match read_file_str(&p.system_conf) {
            Ok(c) => c,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return fail("missing", FIX_SETUP),
            Err(e) => {
                return fail(
                    one_line(&format!("read {}: {e}", p.system_conf.display())),
                    FIX_SETUP,
                );
            }
        };
        let lodo_conf = p.dnsmasq_conf.to_string_lossy();
        let problem = conf_problem(&conf_values(&content), &lodo_conf);
        if !problem.is_empty() {
            return fail(problem, FIX_SETUP);
        }
        pass(format!("includes {}", p.dnsmasq_conf.display()))
    }

    fn check_loopback(&self) -> Check {
        let p = self.paths;
        let launchctl = p.launchctl.to_string_lossy();
        let sys_target = format!("system/{}", system::LOOPBACK_LABEL);
        let loaded = self
            .runner
            .run(None, &launchctl, &["print", &sys_target])
            .is_ok();
        let ifconfig = p.ifconfig.to_string_lossy();
        let out = self
            .runner
            .run(None, &ifconfig, &["lo0"])
            .unwrap_or_default();
        let found = own_aliases(&out);
        let total = usize::from(store::OWN_LAST - store::OWN_FIRST + 1);
        let detail = format!("{found} of {total} addresses on lo0");
        if !loaded {
            return fail(format!("job not loaded; {detail}"), FIX_SETUP);
        }
        if found < total {
            return fail(detail, FIX_SETUP);
        }
        pass(detail)
    }

    fn check_resolvers(&self, domains: &[Domain], dnsmasq_off: bool) -> Check {
        let p = self.paths;
        let problem = self.script_problem();
        if !problem.is_empty() {
            return fail(problem, FIX_SETUP);
        }
        let sudo = p.sudo.to_string_lossy();
        let script = p.script.to_string_lossy();
        if self
            .runner
            .run(None, &sudo, &["-n", "-k", "-l", &script])
            .is_err()
        {
            return fail(
                "sudo asks for a password: the sudoers rule is missing",
                FIX_SETUP,
            );
        }
        if dnsmasq_off {
            return self.resolvers_off();
        }

        let on = enabled(domains);
        let expected = system::resolver_file();
        let marker_line = format!("{}\n", system::MARKER);
        let mut missing = Vec::new();
        let mut foreign = Vec::new();
        let mut foreign_names = Vec::new();

        for d in &on {
            let path = p.resolver_dir.join(&d.name);
            match read_file_str(&path) {
                Ok(got) if got == expected => {}
                Ok(got) if got.starts_with(&marker_line) => missing.push(d.name.clone()),
                Err(e) if e.kind() == io::ErrorKind::NotFound => missing.push(d.name.clone()),
                _ => {
                    foreign.push(path.display().to_string());
                    foreign_names.push(d.name.clone());
                }
            }
        }
        if !foreign.is_empty() {
            return fail(
                format!(
                    "not written by lodo, so lodo apply won't replace: {}",
                    first_few(&foreign_names, ", ")
                ),
                format!("sudo rm {}, then lodo apply", foreign.join(" ")),
            );
        }
        if !missing.is_empty() {
            return fail(
                format!("missing for {}", first_few(&missing, ", ")),
                FIX_APPLY,
            );
        }
        pass(plural(on.len(), "file"))
    }

    fn resolvers_off(&self) -> Check {
        let dir = &self.paths.resolver_dir;
        let entries = match fs::read_dir(dir) {
            Ok(it) => it,
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                return pass("none while dnsmasq is turned off");
            }
            Err(e) => return off(format!("dnsmasq turned off; {}", one_line(&e.to_string()))),
        };
        let marker_line = format!("{}\n", system::MARKER);
        let mut left = Vec::new();
        for entry in entries.flatten() {
            let Ok(ft) = entry.file_type() else {
                continue;
            };
            if !ft.is_file() {
                continue;
            }
            if let Ok(got) = read_file_str(&entry.path())
                && got.starts_with(&marker_line)
            {
                left.push(entry.file_name().to_string_lossy().into_owned());
            }
        }
        left.sort();
        if !left.is_empty() {
            return off(format!(
                "dnsmasq turned off, but lodo apply should remove these: {}",
                first_few(&left, ", ")
            ));
        }
        pass("none while dnsmasq is turned off")
    }

    fn script_problem(&self) -> String {
        let p = self.paths;
        let meta = match fs::symlink_metadata(&p.script) {
            Ok(m) => m,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return "script missing".to_string(),
            Err(e) => return one_line(&format!("lstat {}: {e}", p.script.display())),
        };
        if meta.file_type().is_symlink() {
            return "script is a symlink".to_string();
        }
        if !meta.file_type().is_file() {
            return "script is not a regular file".to_string();
        }
        if meta.uid() != self.root_uid {
            return "script not owned by root".to_string();
        }
        let perm = meta.mode() & 0o7777;
        if perm != 0o755 {
            return format!("script mode {perm:04o}, want 0755");
        }
        match read_file_str(&p.script) {
            Ok(content) if content == system::script(p) => String::new(),
            Ok(_) => "script out of date".to_string(),
            Err(e) => one_line(&e.to_string()),
        }
    }

    fn check_resolver_local(&self) -> Check {
        let local = &self.paths.resolver_local;
        if fs::symlink_metadata(local).is_err() {
            return pass("absent");
        }
        let disp = local.display();
        fail(
            format!("{disp} sends every .local name to one server, away from Bonjour"),
            format!("sudo rm {disp}"),
        )
    }

    fn check_generated(&self, domains: &[Domain]) -> Check {
        let mut stale = Vec::new();
        for f in system::generated(self.paths, domains) {
            let matches = read_file_str(&f.path).is_ok_and(|got| got == f.content);
            if !matches {
                let name = f
                    .path
                    .file_name()
                    .map(|n| n.to_string_lossy().into_owned())
                    .unwrap_or_else(|| f.path.display().to_string());
                stale.push(name);
            }
        }
        if !stale.is_empty() {
            return fail(format!("out of date: {}", stale.join(", ")), FIX_APPLY);
        }
        pass("match domains.json")
    }

    fn check_names(&self, results: &[ProbeResult], dnsmasq_off: bool) -> Check {
        if results.is_empty() {
            return pass("no enabled domains");
        }
        let mut down = Vec::new();
        let mut details = Vec::new();
        let mut names_by_detail: HashMap<&str, Vec<String>> = HashMap::new();
        let mut apply = false;
        let mut host = true;

        for r in results {
            if r.down {
                down.push(r.name.clone());
            } else if !r.ok() {
                if !names_by_detail.contains_key(r.detail.as_str()) {
                    details.push(r.detail.as_str());
                }
                names_by_detail
                    .entry(r.detail.as_str())
                    .or_default()
                    .push(r.name.clone());
                apply = apply || probe::needs_apply(r);
                host = host && r.detail.ends_with(probe::RUN_DEV);
            }
        }

        let not_running = if down.is_empty() {
            String::new()
        } else {
            format!("; not running: {}", first_few(&down, ", "))
        };

        if !details.is_empty() {
            let failed_groups: Vec<String> = details
                .iter()
                .map(|&detail| {
                    let group = names_by_detail
                        .get(detail)
                        .map(Vec::as_slice)
                        .unwrap_or(&[]);
                    format!("{}: {detail}", first_few(group, ", "))
                })
                .collect();
            let fix = if dnsmasq_off {
                FIX_DNSMASQ_ON
            } else if apply {
                FIX_APPLY
            } else if host {
                FIX_DEV
            } else {
                ""
            };
            return fail(
                format!("{}{not_running}", first_few(&failed_groups, "; ")),
                fix,
            );
        }

        pass(format!(
            "{} of {} resolve{not_running}",
            results.len(),
            results.len()
        ))
    }

    fn check_caddy(&self, domains: &[Domain]) -> Check {
        let sites = domains.iter().filter(|d| d.has_site()).count();
        let p = self.paths;
        if sites == 0 {
            if fsutil::exists(&p.caddy) && system::caddy_set_up(p) {
                let st = brew::info(self.runner, p, "caddy");
                if st.off() {
                    return off(CADDY_OFF);
                }
                if st.running {
                    return pass("running, no enabled domain has a port");
                }
            }
            return skip("no enabled domain has a port");
        }
        if !fsutil::exists(&p.caddy) {
            return fail("not installed", "brew install caddy, then lodo setup");
        }
        if !system::caddy_set_up(p) {
            return fail("Homebrew's Caddyfile does not import lodo's", FIX_SETUP);
        }
        if !system::caddy_admin_disabled(p) {
            return fail("Caddy's admin API is not turned off", FIX_SETUP);
        }
        let st = brew::info(self.runner, p, "caddy");
        if st.off() {
            return off(CADDY_OFF);
        }
        if !st.user.is_empty() && st.user != p.user {
            return fail(format!("runs as {}", st.user), FIX_SETUP);
        }
        if !st.running {
            if let Some(blocker) = self.port_blocker() {
                return blocker;
            }
            return fail("not running", FIX_SETUP);
        }
        if let Err(err) = caddy::validate(self.runner, &p.caddy, &p.system_caddyfile) {
            return fail(one_line(&err), FIX_APPLY);
        }
        self.port_owner(sites)
    }

    fn port_blocker(&self) -> Option<Check> {
        let lsof = self.paths.lsof.to_string_lossy();
        let port_arg = format!("-iTCP:{}", self.http_port);
        let out = self
            .runner
            .run(None, &lsof, &["-nP", &port_arg, "-sTCP:LISTEN"])
            .unwrap_or_default();
        let cmds = listeners(&out);
        if cmds.is_empty() || cmds.iter().any(|c| c.starts_with("caddy")) {
            return None;
        }
        Some(fail(
            format!("port {} is taken by {}", self.http_port, cmds[0]),
            format!("stop {}, then lodo apply", cmds[0]),
        ))
    }

    fn port_owner(&self, sites: usize) -> Check {
        let lsof = self.paths.lsof.to_string_lossy();
        let port_arg = format!("-iTCP:{}", self.http_port);
        let out = self
            .runner
            .run(None, &lsof, &["-nP", &port_arg, "-sTCP:LISTEN"])
            .unwrap_or_default();
        let cmds = listeners(&out);
        if cmds.iter().any(|c| c.starts_with("caddy")) {
            return pass(format!("running, serves {}", plural(sites, "site")));
        }
        if !cmds.is_empty() {
            return fail(
                format!("port {} is taken by {}", self.http_port, cmds[0]),
                format!("stop {}, then lodo apply", cmds[0]),
            );
        }
        fail(
            format!("nothing listens on port {}", self.http_port),
            FIX_SETUP,
        )
    }
}

/// Returns checks that are neither `ok` nor `skipped`, preserving order.
pub fn failed(checks: &[Check]) -> Vec<Check> {
    checks
        .iter()
        .filter(|c| !c.ok && !c.skipped)
        .cloned()
        .collect()
}

fn pass(detail: impl Into<String>) -> Check {
    Check {
        ok: true,
        detail: detail.into(),
        ..Check::default()
    }
}

fn fail(detail: impl Into<String>, fix: impl Into<String>) -> Check {
    Check {
        detail: detail.into(),
        fix: fix.into(),
        ..Check::default()
    }
}

fn skip(detail: impl Into<String>) -> Check {
    Check {
        skipped: true,
        detail: detail.into(),
        ..Check::default()
    }
}

fn off(detail: impl Into<String>) -> Check {
    Check {
        skipped: true,
        off: true,
        detail: detail.into(),
        ..Check::default()
    }
}

pub(crate) fn enabled(domains: &[Domain]) -> Vec<Domain> {
    store::sort(domains)
        .into_iter()
        .filter(|d| d.enabled)
        .collect()
}

pub(crate) fn read_file_str(path: &Path) -> io::Result<String> {
    fs::read_to_string(path)
}

pub(crate) fn join_host_port(host: &str, port: u16) -> String {
    if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

fn conf_values(content: &str) -> HashMap<String, Vec<String>> {
    let mut values: HashMap<String, Vec<String>> = HashMap::new();
    for raw in content.lines() {
        let line = raw.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let (key, val) = match line.split_once('=') {
            Some((k, v)) => (k.trim(), v.trim()),
            None => (line, ""),
        };
        values
            .entry(key.to_string())
            .or_default()
            .push(val.to_string());
    }
    values
}

fn conf_problem(values: &HashMap<String, Vec<String>>, lodo_conf: &str) -> String {
    let p = line_count(values, "conf-file");
    if !p.is_empty() {
        return p;
    }
    if let Some(got) = values.get("conf-file").and_then(|v| v.first())
        && got != lodo_conf
    {
        return format!("conf-file points at {got}");
    }
    for (key, want) in [
        ("port", dnsmasq::PORT.to_string()),
        ("listen-address", dnsmasq::LISTEN_ADDRESS.to_string()),
    ] {
        let p = line_count(values, key);
        if !p.is_empty() {
            return p;
        }
        if let Some(got) = values.get(key).and_then(|v| v.first())
            && got != &want
        {
            return format!("{key} is {got}, want {want}");
        }
    }
    if !values.contains_key("bind-interfaces") {
        return "no bind-interfaces line".to_string();
    }
    String::new()
}

fn line_count(values: &HashMap<String, Vec<String>>, key: &str) -> String {
    match values.get(key).map_or(0, Vec::len) {
        0 => format!("no {key} line"),
        1 => String::new(),
        n => format!("{n} {key} lines"),
    }
}

fn own_aliases(out: &str) -> usize {
    let mut on = HashSet::new();
    for line in out.lines() {
        let mut fields = line.split_whitespace();
        if fields.next() == Some("inet")
            && let Some(ip) = fields.next()
        {
            on.insert(ip);
        }
    }
    (store::OWN_FIRST..=store::OWN_LAST)
        .filter(|&i| on.contains(store::own_address(i).as_str()))
        .count()
}

fn listeners(out: &str) -> Vec<String> {
    out.trim()
        .lines()
        .skip(1)
        .filter_map(|line| line.split_whitespace().next())
        .map(str::to_string)
        .collect()
}

fn first_few(items: &[String], sep: &str) -> String {
    const SHOWN: usize = 3;
    if items.len() <= SHOWN {
        return items.join(sep);
    }
    format!(
        "{}{sep}and {} more",
        items[..SHOWN].join(sep),
        items.len() - SHOWN
    )
}

fn plural(n: usize, noun: &str) -> String {
    if n == 1 {
        format!("1 {noun}")
    } else {
        format!("{n} {noun}s")
    }
}

pub(crate) fn one_line(s: &str) -> String {
    s.lines()
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .collect::<Vec<_>>()
        .join("; ")
}
