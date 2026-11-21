//! `lodo apply`, `set_service`, readiness checks, Caddy settle verification,
//! and bounded query-log trimming.

use std::fs::{self, File};
use std::io::{Read, Seek, SeekFrom};
use std::path::Path;
use std::sync::atomic::Ordering;
use std::thread;
use std::time::{Duration, SystemTime};

use crate::paths::Paths;
use crate::run::Runner;
use crate::store::{self, Domain};
use crate::{dnsmasq, fsutil, menubar};

use super::render::caddy_quote;
use super::{
    CADDY_SETTLE_MS, Deps, ERR_NOT_SET_UP, RealDeps, SERVICES, split_lines, write_files_with,
};

const ERR_CADDY_NOT_SET_UP: &str =
    "a domain has a port but Caddy is not set up for lodo: brew install caddy, then lodo setup";

const MAX_LOG_SIZE: u64 = 1 << 20;

/// Makes the running system match `domains`: writes generated files, restarts
/// `dnsmasq` when its config changed or it stopped, runs the resolver script
/// through `sudo`, restarts Caddy when needed, and refreshes the menu bar icon.
pub fn apply(p: &Paths, r: &dyn Runner, domains: &[Domain]) -> Result<(), String> {
    apply_with(&RealDeps, p, r, domains)
}

pub(super) fn apply_with<D: Deps>(
    deps: &D,
    p: &Paths,
    r: &dyn Runner,
    domains: &[Domain],
) -> Result<(), String> {
    if !setup_done(p) {
        return Err(ERR_NOT_SET_UP.to_string());
    }
    let changed = write_files_with(deps, p, domains)?;

    let mut names = String::new();
    let st = deps.brew_info(r, p, "dnsmasq");
    if !st.off() {
        let trimmed = trim_log(&p.log);
        if !st.running || changed.contains(&p.dnsmasq_conf) || trimmed {
            deps.brew_restart(r, p, "dnsmasq")?;
        }
        names = deps.resolver_list(domains);
    }

    run_script(p, r, &names)?;
    apply_caddy(deps, p, r, domains, changed.contains(&p.caddyfile))?;
    menubar::refresh(p, r)
}

fn run_script(p: &Paths, r: &dyn Runner, names: &str) -> Result<(), String> {
    let sudo = p.sudo.to_string_lossy();
    let script = p.script.to_string_lossy();
    r.run_input(None, names, &sudo, &["-n", &script])
        .map(|_| ())
        .map_err(|e| format!("update {}: {e}", p.resolver_dir.display()))
}

fn apply_caddy<D: Deps>(
    deps: &D,
    p: &Paths,
    r: &dyn Runner,
    domains: &[Domain],
    changed: bool,
) -> Result<(), String> {
    let needed = domains.iter().any(Domain::has_site);
    if !caddy_set_up(p) {
        if needed {
            return Err(ERR_CADDY_NOT_SET_UP.to_string());
        }
        return Ok(());
    }
    let st = deps.brew_info(r, p, "caddy");
    if st.off() {
        return Ok(());
    }
    if !changed && (!needed || st.running) {
        return Ok(());
    }
    restart_caddy(deps, p, r)
}

pub(super) fn restart_caddy<D: Deps>(deps: &D, p: &Paths, r: &dyn Runner) -> Result<(), String> {
    deps.caddy_validate(r, &p.caddy, &p.system_caddyfile)?;
    let started = SystemTime::now();
    deps.brew_restart(r, p, "caddy")?;

    let settle_ms = CADDY_SETTLE_MS.load(Ordering::Relaxed);
    if settle_ms > 0 {
        thread::sleep(Duration::from_millis(settle_ms));
    }

    if deps.brew_info(r, p, "caddy").running {
        return Ok(());
    }
    let msg = deps.caddy_log_error(&p.caddy_log, started);
    if !msg.is_empty() {
        return Err(format!("caddy stopped right after it started: {msg}"));
    }
    Err(format!(
        "caddy stopped right after it started; see {}",
        p.caddy_log.display()
    ))
}

/// Turns a service (`dnsmasq`, `caddy`, or `menubar`) on or off.
pub fn set_service(p: &Paths, r: &dyn Runner, service: &str, on: bool) -> Result<(), String> {
    set_service_with(&RealDeps, p, r, service, on)
}

pub(super) fn set_service_with<D: Deps>(
    deps: &D,
    p: &Paths,
    r: &dyn Runner,
    service: &str,
    on: bool,
) -> Result<(), String> {
    if !setup_done(p) {
        return Err(ERR_NOT_SET_UP.to_string());
    }
    if !SERVICES.contains(&service) {
        return Err(format!("unknown service {service:?}"));
    }
    match service {
        "dnsmasq" => set_dnsmasq(deps, p, r, on),
        "menubar" => {
            if on {
                menubar::start(p, r)
            } else {
                menubar::stop(p, r)
            }
        }
        _ => {
            if !on {
                return deps.brew_stop(r, p, service);
            }
            if !caddy_set_up(p) {
                return Err(
                    "caddy is not set up for lodo: brew install caddy, then lodo setup".to_string(),
                );
            }
            restart_caddy(deps, p, r)
        }
    }
}

fn set_dnsmasq<D: Deps>(deps: &D, p: &Paths, r: &dyn Runner, on: bool) -> Result<(), String> {
    if !on {
        run_script(p, r, "")?;
        return deps.brew_stop(r, p, "dnsmasq");
    }
    let domains = store::load(&p.domains_json).map_err(|e| e.to_string())?;
    deps.brew_restart(r, p, "dnsmasq")?;
    run_script(p, r, &deps.resolver_list(&domains))
}

/// Reports whether `lodo setup` has run: the resolver script is installed and
/// Homebrew's `dnsmasq.conf` includes `lodo`'s config.
pub fn setup_done(p: &Paths) -> bool {
    fsutil::exists(&p.script)
        && has_line(
            &p.system_conf,
            &format!("conf-file={}", p.dnsmasq_conf.display()),
        )
}

/// Reports whether Homebrew's `Caddyfile` imports `lodo`'s `Caddyfile`.
pub fn caddy_set_up(p: &Paths) -> bool {
    let caddyfile = p.caddyfile.to_string_lossy();
    has_line(&p.system_caddyfile, &format!("import {caddyfile}"))
        || has_line(
            &p.system_caddyfile,
            &format!("import {}", caddy_quote(&caddyfile)),
        )
}

/// Caps `dnsmasq`'s query log at `dnsmasq::MAX_TAIL` bytes when it exceeds
/// `MAX_LOG_SIZE`, reporting whether it rewrote the file.
pub(super) fn trim_log(path: &Path) -> bool {
    let Ok(info) = fs::metadata(path) else {
        return false;
    };
    if !info.file_type().is_file() || info.len() <= MAX_LOG_SIZE {
        return false;
    }
    let Ok(mut f) = File::open(path) else {
        return false;
    };
    let start = info.len().saturating_sub(dnsmasq::MAX_TAIL + 1);
    if f.seek(SeekFrom::Start(start)).is_err() {
        return false;
    }
    let mut buf = Vec::new();
    if f.take(dnsmasq::MAX_TAIL + 1).read_to_end(&mut buf).is_err() {
        return false;
    }
    let max_tail = dnsmasq::MAX_TAIL as usize;
    let tail = if let Some(pos) = buf.iter().position(|&b| b == b'\n') {
        &buf[pos + 1..]
    } else if buf.len() > max_tail {
        &buf[buf.len() - max_tail..]
    } else {
        &buf[..]
    };
    fsutil::write_file(path, tail, 0o644).unwrap_or(false)
}

fn has_line(path: &Path, want: &str) -> bool {
    let Ok(data) = fs::read_to_string(path) else {
        return false;
    };
    split_lines(&data).iter().any(|line| line.trim() == want)
}
