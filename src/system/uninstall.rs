//! `lodo uninstall`: restores Homebrew configs, untrusts the local CA, stops
//! services and the menu bar icon, removes loopback aliases, and deletes root
//! files while keeping `~/.config/lodo`.

use std::collections::HashSet;
use std::fs;
use std::io::Write;
use std::path::Path;

use crate::paths::Paths;
use crate::run::Runner;
use crate::{fsutil, menubar, store};

use super::apply::caddy_set_up;
use super::step::{StepError, StepOutcome, Steps};
use super::{
    Deps, LOOPBACK_LABEL, RealDeps, file_name_str, remove_if_exists, split_lines,
    strip_system_caddyfile, strip_system_conf,
};

/// Removes everything `setup` installed and restores Homebrew's configs while
/// preserving `~/.config/lodo` (`domains.json`).
pub fn uninstall(p: &Paths, r: &dyn Runner, out: &mut dyn Write) -> Result<(), StepError> {
    uninstall_with(&RealDeps, p, r, out)
}

pub(super) fn uninstall_with<D: Deps>(
    deps: &D,
    p: &Paths,
    r: &dyn Runner,
    out: &mut dyn Write,
) -> Result<(), StepError> {
    let mut s = Steps { out };
    let _ = writeln!(s.out, "lodo needs your password to remove its root parts.");
    if let Err(err) = r.run_tty(&p.sudo.to_string_lossy(), &["-v"]) {
        return Err(StepError {
            step: "admin password".to_string(),
            err: err.to_string(),
            fix: "run lodo uninstall again and enter your password".to_string(),
        });
    }

    s.do_step(
        "lodo's /etc/resolver files removed",
        "lodo uninstall",
        || remove_resolver_files(p, r).map(|()| StepOutcome::Done(String::new())),
    )?;

    s.do_step("dnsmasq stopped", "brew services stop dnsmasq", || {
        deps.brew_stop(r, p, "dnsmasq")
            .map(|()| StepOutcome::Done(String::new()))
    })?;

    s.do_step(
        "Homebrew's dnsmasq.conf restored",
        &format!("edit {} by hand", p.system_conf.display()),
        || restore_system_conf(p).map(StepOutcome::Done),
    )?;

    s.do_step(
        "Caddy's local CA no longer trusted",
        &format!(
            "sudo security remove-trusted-cert -d {}",
            p.trusted_ca.display()
        ),
        || untrust_caddy(p, r),
    )?;

    s.do_step(
        "Caddy no longer serves lodo's sites",
        &format!("edit {} by hand", p.system_caddyfile.display()),
        || restore_caddy(deps, p, r).map(StepOutcome::Done),
    )?;

    s.do_step("menu bar icon stopped", "lodo menubar --stop", || {
        stop_menubar(p, r)
    })?;

    s.do_step(
        "loopback addresses removed",
        &format!("sudo launchctl bootout system/{LOOPBACK_LABEL}"),
        || remove_loopback(p, r).map(|()| StepOutcome::Done(String::new())),
    )?;

    s.do_step(
        "sudoers rule and resolver script removed",
        &format!("sudo rm {} '{}'", p.sudoers.display(), p.script.display()),
        || remove_root_files(p, r).map(|()| StepOutcome::Done(String::new())),
    )?;

    let _ = writeln!(
        s.out,
        "Kept {}. dnsmasq is stopped; to run it as root on port 53 again: sudo brew services start dnsmasq",
        p.config_dir.display()
    );
    Ok(())
}

fn remove_resolver_files(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    if !fsutil::exists(&p.script) {
        return Ok(());
    }
    let sudo = p.sudo.to_string_lossy();
    let script = p.script.to_string_lossy();
    r.run_input(None, "", &sudo, &["-n", &script])
        .map(|_| ())
        .map_err(|e| e.to_string())
}

fn restore_system_conf(p: &Paths) -> Result<String, String> {
    if fsutil::exists(&p.system_conf_backup) {
        fs::rename(&p.system_conf_backup, &p.system_conf)
            .map_err(|e| format!("restore {}: {e}", p.system_conf.display()))?;
        return Ok(format!("from {}", file_name_str(&p.system_conf_backup)));
    }
    let old = match fs::read_to_string(&p.system_conf) {
        Ok(s) => s,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(String::new()),
        Err(e) => return Err(format!("read {}: {e}", p.system_conf.display())),
    };
    fsutil::write_file(&p.system_conf, strip_system_conf(&old).as_bytes(), 0o644)
        .map_err(|e| e.to_string())?;
    Ok("no backup: removed lodo's block".to_string())
}

fn untrust_caddy(p: &Paths, r: &dyn Runner) -> Result<StepOutcome, String> {
    if !fsutil::exists(&p.trusted_ca) {
        return Ok(StepOutcome::Skipped("lodo never trusted a CA".to_string()));
    }
    let sudo = p.sudo.to_string_lossy();
    let security = p.security.to_string_lossy();
    let trusted_s = p.trusted_ca.to_string_lossy();
    if let Err(err) = r.run_tty(&sudo, &[&security, "remove-trusted-cert", "-d", &trusted_s]) {
        return Ok(StepOutcome::Warning(format!(
            "{err}; remove it by hand: sudo security remove-trusted-cert -d {}",
            p.trusted_ca.display()
        )));
    }
    if let Err(err) = fs::remove_file(&p.trusted_ca) {
        return Ok(StepOutcome::Warning(format!(
            "untrusted, but {err}; delete it by hand"
        )));
    }
    Ok(StepOutcome::Done(String::new()))
}

fn restore_caddy<D: Deps>(deps: &D, p: &Paths, r: &dyn Runner) -> Result<String, String> {
    if !caddy_set_up(p) {
        return Ok(String::new());
    }
    deps.brew_stop(r, p, "caddy")?;

    let (src, from_backup) = if fsutil::exists(&p.system_caddyfile_backup) {
        (&p.system_caddyfile_backup, true)
    } else {
        (&p.system_caddyfile, false)
    };
    let old = fs::read(src).map_err(|e| format!("read {}: {e}", src.display()))?;
    let rest = strip_system_caddyfile(&String::from_utf8_lossy(&old), p);

    let msg = if rest.trim().is_empty() {
        remove_if_exists(&p.system_caddyfile)?;
        format!(
            "stopped Caddy, removed lodo's {}",
            file_name_str(&p.system_caddyfile)
        )
    } else {
        fsutil::write_file(&p.system_caddyfile, rest.as_bytes(), 0o644)
            .map_err(|e| e.to_string())?;
        if from_backup {
            format!(
                "stopped Caddy, restored {}",
                file_name_str(&p.system_caddyfile)
            )
        } else {
            "stopped Caddy, removed lodo's import".to_string()
        }
    };

    if from_backup {
        remove_if_exists(&p.system_caddyfile_backup)?;
    }
    Ok(msg)
}

fn remove_loopback(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    let sudo = p.sudo.to_string_lossy();
    let launchctl = p.launchctl.to_string_lossy();
    let rm = p.rm.to_string_lossy();
    let ifconfig = p.ifconfig.to_string_lossy();

    let sys_label = format!("system/{LOOPBACK_LABEL}");
    let _ = r.run(None, &sudo, &["-n", &launchctl, "bootout", &sys_label]);

    let plist_s = p.loopback_plist.to_string_lossy();
    r.run(None, &sudo, &["-n", &rm, "-f", &plist_s])
        .map_err(|e| e.to_string())?;

    let if_res = r.run(None, &ifconfig, &["lo0"]);
    let mut on = HashSet::new();
    if let Ok(ref out) = if_res {
        for line in split_lines(out) {
            let mut fields = line.split_whitespace();
            if fields.next() == Some("inet")
                && let Some(addr) = fields.next()
            {
                on.insert(addr.to_string());
            }
        }
    }

    for i in store::OWN_FIRST..=store::OWN_LAST {
        let addr = store::own_address(i);
        if if_res.is_ok() && !on.contains(addr.as_str()) {
            continue;
        }
        let _ = r.run(None, &sudo, &["-n", &ifconfig, "lo0", "-alias", &addr]);
    }
    Ok(())
}

fn stop_menubar(p: &Paths, r: &dyn Runner) -> Result<StepOutcome, String> {
    if !fsutil::exists(&p.menubar_plist) && !menubar::running(r, p) {
        return Ok(StepOutcome::Skipped("not enabled".to_string()));
    }
    menubar::stop(p, r).map(|()| StepOutcome::Done(String::new()))
}

fn remove_root_files(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    let sudo = p.sudo.to_string_lossy();
    let rm = p.rm.to_string_lossy();
    let rmdir = p.rmdir.to_string_lossy();
    let sudoers_s = p.sudoers.to_string_lossy();
    let script_s = p.script.to_string_lossy();

    r.run(None, &sudo, &["-n", &rm, "-f", &sudoers_s, &script_s])
        .map_err(|e| e.to_string())?;
    let script_dir = p
        .script
        .parent()
        .unwrap_or_else(|| Path::new("/"))
        .to_string_lossy();
    // rmdir refuses a non-empty directory, which is the safe outcome.
    let _ = r.run(None, &sudo, &["-n", &rmdir, &script_dir]);
    Ok(())
}
