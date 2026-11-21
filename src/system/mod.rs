//! System setup, apply, service control, config rewriting, and uninstall for macOS.

mod apply;
mod render;
mod setup;
mod step;
#[cfg(test)]
mod tests;
mod uninstall;

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, SystemTime};

use crate::paths::Paths;
use crate::run::Runner;
use crate::store::Domain;
use crate::{brew, caddy, dnsmasq, fsutil};

use render::caddy_quote;

pub use apply::{apply, caddy_set_up, set_service, setup_done};
pub use render::{
    CADDY_GLOBAL_BLOCK, LOOPBACK_LABEL, MARKER, MAX_SCRIPT_INPUT, caddy_block, conf_block,
    loopback_plist, resolver_file, resolver_lines, script, sudoers,
};
pub use setup::setup;
pub use step::StepError;
pub use uninstall::uninstall;

/// Services `set_service` turns on and off.
pub const SERVICES: &[&str] = &["dnsmasq", "caddy", "menubar"];

/// `launchd` labels a root `dnsmasq` job started via `sudo brew services` can use.
pub const DNS_SYSTEM_LABELS: &[&str] = &["homebrew.mxcl.dnsmasq", "sh.brew.dnsmasq"];

/// Error message returned when `apply` or `set_service` runs before `lodo setup`.
pub const ERR_NOT_SET_UP: &str = "lodo is not set up: run lodo setup";

const CONF_KEYS: &[&str] = &[
    "conf-file",
    "listen-address",
    "port",
    "bind-interfaces",
    "bind-dynamic",
];

const ADMIN_OFF_MARKER: &str = "\tadmin off # lodo";

pub(crate) static CADDY_SETTLE_MS: AtomicU64 = AtomicU64::new(if cfg!(test) { 0 } else { 1000 });
pub(crate) static TRUST_WAIT_MS: AtomicU64 = AtomicU64::new(if cfg!(test) { 0 } else { 400 });

/// Overrides Caddy settle and CA root wait durations (useful for integration tests).
pub fn set_timings(caddy_settle: Duration, trust_wait: Duration) {
    CADDY_SETTLE_MS.store(caddy_settle.as_millis() as u64, Ordering::Relaxed);
    TRUST_WAIT_MS.store(trust_wait.as_millis() as u64, Ordering::Relaxed);
}

/// A file `lodo` generates from `domains.json` and its rendered content.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GeneratedFile {
    pub path: PathBuf,
    pub content: String,
}

pub(crate) trait Deps {
    fn dnsmasq_config(&self, domains: &[Domain], log_path: &Path) -> String;
    fn resolver_list(&self, domains: &[Domain]) -> String;
    fn caddy_config(&self, domains: &[Domain]) -> String;
    fn caddy_validate(&self, r: &dyn Runner, caddy_bin: &Path, path: &Path) -> Result<(), String>;
    fn caddy_log_error(&self, path: &Path, since: SystemTime) -> String;
    fn brew_info(&self, r: &dyn Runner, p: &Paths, service: &str) -> brew::Status;
    fn brew_restart(&self, r: &dyn Runner, p: &Paths, service: &str) -> Result<(), String>;
    fn brew_stop(&self, r: &dyn Runner, p: &Paths, service: &str) -> Result<(), String>;
}

pub(crate) struct RealDeps;

impl Deps for RealDeps {
    fn dnsmasq_config(&self, domains: &[Domain], log_path: &Path) -> String {
        dnsmasq::config(domains, log_path)
    }

    fn resolver_list(&self, domains: &[Domain]) -> String {
        dnsmasq::resolver_list(domains)
    }

    fn caddy_config(&self, domains: &[Domain]) -> String {
        caddy::config(domains)
    }

    fn caddy_validate(&self, r: &dyn Runner, caddy_bin: &Path, path: &Path) -> Result<(), String> {
        caddy::validate(r, caddy_bin, path)
    }

    fn caddy_log_error(&self, path: &Path, since: SystemTime) -> String {
        caddy::log_error(path, since)
    }

    fn brew_info(&self, r: &dyn Runner, p: &Paths, service: &str) -> brew::Status {
        brew::info(r, p, service)
    }

    fn brew_restart(&self, r: &dyn Runner, p: &Paths, service: &str) -> Result<(), String> {
        brew::restart(r, p, service)
    }

    fn brew_stop(&self, r: &dyn Runner, p: &Paths, service: &str) -> Result<(), String> {
        brew::stop(r, p, service)
    }
}

/// Returns Homebrew's `dnsmasq.conf` with every line setting one of `lodo`'s
/// options removed and `lodo`'s block appended. Idempotent.
pub fn rewrite_system_conf(old: &str, p: &Paths) -> String {
    append_block(&strip_system_conf(old), &conf_block(p))
}

/// Returns Homebrew's `dnsmasq.conf` without `lodo`'s marker and without any
/// line setting one of `lodo`'s options.
pub fn strip_system_conf(old: &str) -> String {
    strip(old, |line| {
        let key = line.split('=').next().unwrap_or("").trim();
        CONF_KEYS.contains(&key)
    })
}

/// Returns Homebrew's `Caddyfile` with `lodo`'s global block at the top (or
/// `admin off` ensured in the user's global block) and `lodo`'s import block at
/// the end. Idempotent.
pub fn rewrite_system_caddyfile(old: &str, p: &Paths) -> String {
    let rest = strip_system_caddyfile(old, p);
    if has_global_block(&rest) {
        return append_block(&ensure_admin_off(&rest), &caddy_block(p));
    }
    format!(
        "{CADDY_GLOBAL_BLOCK}\n{}",
        append_block(&rest, &caddy_block(p))
    )
}

/// Returns Homebrew's `Caddyfile` without `lodo`'s marker, global block,
/// injected `admin off` line, and import line.
pub fn strip_system_caddyfile(old: &str, p: &Paths) -> String {
    let caddyfile = p.caddyfile.to_string_lossy();
    let import_plain = format!("import {caddyfile}");
    let import_quoted = format!("import {}", caddy_quote(&caddyfile));
    let admin_line = ADMIN_OFF_MARKER.trim();
    strip(old, |line| {
        line == import_plain || line == import_quoted || line == admin_line
    })
}

/// Reports whether Homebrew's `Caddyfile` has a global options block at the top
/// that sets `admin off`.
pub fn caddy_admin_disabled(p: &Paths) -> bool {
    let Ok(data) = fs::read_to_string(&p.system_caddyfile) else {
        return false;
    };
    let (lines, Some((start, end))) = global_block_bounds(&data) else {
        return false;
    };
    lines[start + 1..end].iter().copied().any(is_admin_off_line)
}

/// Returns `lodo`'s three generated files for `domains`: `dnsmasq.conf`, the
/// resolver list, and the `Caddyfile`.
pub fn generated(p: &Paths, domains: &[Domain]) -> Vec<GeneratedFile> {
    generated_with(&RealDeps, p, domains)
}

pub(crate) fn generated_with<D: Deps>(
    deps: &D,
    p: &Paths,
    domains: &[Domain],
) -> Vec<GeneratedFile> {
    vec![
        GeneratedFile {
            path: p.dnsmasq_conf.clone(),
            content: deps.dnsmasq_config(domains, &p.log),
        },
        GeneratedFile {
            path: p.resolvers.clone(),
            content: deps.resolver_list(domains),
        },
        GeneratedFile {
            path: p.caddyfile.clone(),
            content: deps.caddy_config(domains),
        },
    ]
}

/// Writes `lodo`'s generated files for `domains` and returns the paths of the
/// ones whose content changed.
pub fn write_files(p: &Paths, domains: &[Domain]) -> Result<Vec<PathBuf>, String> {
    write_files_with(&RealDeps, p, domains)
}

pub(crate) fn write_files_with<D: Deps>(
    deps: &D,
    p: &Paths,
    domains: &[Domain],
) -> Result<Vec<PathBuf>, String> {
    fs::create_dir_all(&p.config_dir)
        .map_err(|e| format!("create {}: {e}", p.config_dir.display()))?;
    let mut changed = Vec::new();
    for f in generated_with(deps, p, domains) {
        let wrote =
            fsutil::write_file(&f.path, f.content.as_bytes(), 0o644).map_err(|e| e.to_string())?;
        if wrote {
            changed.push(f.path);
        }
    }
    Ok(changed)
}

fn global_block_bounds(content: &str) -> (Vec<&str>, Option<(usize, usize)>) {
    let lines = split_lines(content);
    let mut start = None;
    for (i, line) in lines.iter().enumerate() {
        let t = line.trim();
        if t.is_empty() || t.starts_with('#') {
            continue;
        }
        if t == "{" {
            start = Some(i);
        }
        break;
    }
    let Some(s) = start else {
        return (lines, None);
    };
    let Some(e) = block_end(&lines, s) else {
        return (lines, None);
    };
    (lines, Some((s, e)))
}

fn is_admin_off_line(line: &str) -> bool {
    let f: Vec<&str> = line.split_whitespace().collect();
    f.len() >= 2 && f[0] == "admin" && f[1] == "off" && (f.len() == 2 || f[2].starts_with('#'))
}

fn ensure_admin_off(content: &str) -> String {
    let (mut lines, Some((start, end))) = global_block_bounds(content) else {
        return content.to_string();
    };
    for i in start + 1..end {
        if lines[i].split_whitespace().next() != Some("admin") {
            continue;
        }
        if is_admin_off_line(lines[i]) {
            return content.to_string();
        }
        lines[i] = ADMIN_OFF_MARKER;
        return format!("{}\n", lines.join("\n"));
    }
    lines.insert(start + 1, ADMIN_OFF_MARKER);
    format!("{}\n", lines.join("\n"))
}

fn has_global_block(content: &str) -> bool {
    for line in split_lines(content) {
        let trimmed = line.trim();
        if trimmed.is_empty() || trimmed.starts_with('#') {
            continue;
        }
        return trimmed.starts_with('{');
    }
    false
}

fn strip(old: &str, mut drop_line: impl FnMut(&str) -> bool) -> String {
    let lines = split_lines(old);
    let mut kept = Vec::new();
    let mut i = 0;
    while i < lines.len() {
        let trimmed = lines[i].trim();
        if trimmed == MARKER {
            if let Some(end) = block_end(&lines, i + 1) {
                i = end + 1;
            } else {
                i += 1;
            }
            continue;
        }
        if drop_line(trimmed) {
            i += 1;
            continue;
        }
        kept.push(lines[i]);
        i += 1;
    }
    while kept.first().is_some_and(|l| l.trim().is_empty()) {
        kept.remove(0);
    }
    while kept.last().is_some_and(|l| l.trim().is_empty()) {
        kept.pop();
    }
    if kept.is_empty() {
        return String::new();
    }
    format!("{}\n", kept.join("\n"))
}

fn block_end(lines: &[&str], start: usize) -> Option<usize> {
    if start >= lines.len() || lines[start].trim() != "{" {
        return None;
    }
    (start + 1..lines.len()).find(|&i| lines[i].trim() == "}")
}

fn append_block(content: &str, block: &str) -> String {
    if content.is_empty() {
        block.to_string()
    } else {
        format!("{content}\n{block}")
    }
}

pub(crate) fn split_lines(s: &str) -> Vec<&str> {
    if s.is_empty() {
        return Vec::new();
    }
    s.split_inclusive('\n')
        .map(|line| line.trim_end_matches(['\r', '\n']))
        .collect()
}

pub(crate) fn file_name_str(path: &Path) -> String {
    path.file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| path.display().to_string())
}

pub(crate) fn remove_if_exists(path: &Path) -> Result<(), String> {
    match fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(format!("remove {}: {e}", path.display())),
    }
}
