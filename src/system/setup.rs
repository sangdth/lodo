//! `lodo setup`: preflight, root script/sudoers/loopback installation, Homebrew
//! config rewrites, local CA trust, and passwordless resolver verification.

use std::fs::{self, DirBuilder, File, OpenOptions};
use std::io::{Read, Write};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::Ordering;
use std::thread;
use std::time::Duration;

use nix::unistd::Uid;

use crate::paths::Paths;
use crate::run::Runner;
use crate::{fsutil, menubar, store};

use super::apply::{restart_caddy, trim_log};
use super::step::{StepError, StepOutcome, Steps};
use super::{
    DNS_SYSTEM_LABELS, Deps, LOOPBACK_LABEL, MARKER, RealDeps, TRUST_WAIT_MS, file_name_str,
    loopback_plist, remove_if_exists, rewrite_system_caddyfile, rewrite_system_conf, script,
    strip_system_caddyfile, strip_system_conf, sudoers, write_files_with,
};

const TRUST_TRIES: usize = 5;

struct StagingGuard<'a>(&'a Path);

impl Drop for StagingGuard<'_> {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(self.0);
    }
}

/// Installs everything `lodo` needs, asking for the admin password once upfront.
/// Running `setup` again repairs and updates an existing setup.
pub fn setup(p: &Paths, r: &dyn Runner, out: &mut dyn Write) -> Result<(), StepError> {
    setup_with(&RealDeps, p, r, out)
}

pub(super) fn setup_with<D: Deps>(
    deps: &D,
    p: &Paths,
    r: &dyn Runner,
    out: &mut dyn Write,
) -> Result<(), StepError> {
    let mut s = Steps { out };

    s.do_step(
        "Homebrew and dnsmasq are installed",
        "brew install dnsmasq",
        || preflight(p).map(|()| StepOutcome::Done(String::new())),
    )?;

    if let Err(err) = sudoers(p) {
        return Err(StepError {
            step: "check your user name".to_string(),
            err,
            fix: "run lodo from an account with a plain short name".to_string(),
        });
    }

    let _ = writeln!(
        s.out,
        "lodo needs your password once, to install its root parts."
    );
    if let Err(err) = r.run_tty(&p.sudo.to_string_lossy(), &["-v"]) {
        return Err(StepError {
            step: "admin password".to_string(),
            err: err.to_string(),
            fix: "run lodo setup again and enter your password".to_string(),
        });
    }

    if let Err(err) = DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&p.staging)
    {
        return Err(StepError {
            step: format!("create {}", p.staging.display()),
            err: err.to_string(),
            fix: "check ~/.config is writable".to_string(),
        });
    }
    let _staging_guard = StagingGuard(&p.staging);

    s.do_step(
        &format!("lodo's files in {}", p.config_dir.display()),
        &format!("fix or remove {}", p.domains_json.display()),
        || user_files(deps, p).map(|()| StepOutcome::Done(String::new())),
    )?;

    s.do_step(
        "Homebrew's dnsmasq.conf includes lodo's",
        &format!("check {} is writable", p.system_conf.display()),
        || system_conf(p).map(StepOutcome::Done),
    )?;

    s.do_step(
        "no root dnsmasq job",
        "sudo brew services stop dnsmasq",
        || stop_system_dnsmasq(p, r).map(StepOutcome::Done),
    )?;

    s.do_step("resolver script installed", "lodo setup", || {
        install_script(p, r).map(|()| StepOutcome::Done(String::new()))
    })?;

    s.do_step("sudoers rule installed", "lodo setup", || {
        install_sudoers(p, r).map(|()| StepOutcome::Done(String::new()))
    })?;

    s.do_step(
        &format!(
            "loopback addresses {}–{} on lo0",
            store::own_address(store::OWN_FIRST),
            store::own_address(store::OWN_LAST)
        ),
        "lodo setup",
        || install_loopback(p, r).map(|()| StepOutcome::Done(String::new())),
    )?;

    s.do_step(
        &format!("no {}", p.resolver_local.display()),
        &format!("sudo rm {}", p.resolver_local.display()),
        || remove_resolver_local(p, r).map(StepOutcome::Done),
    )?;

    s.do_step(
        "dnsmasq runs as you on port 53535",
        "brew services restart dnsmasq",
        || {
            trim_log(&p.log);
            deps.brew_restart(r, p, "dnsmasq")
                .map(|()| StepOutcome::Done(String::new()))
        },
    )?;

    s.do_step(
        "Caddy serves lodo's sites on port 80",
        "brew services restart caddy",
        || setup_caddy(deps, p, r),
    )?;

    s.do_step(
        "Caddy's local CA trusted for HTTPS names",
        "lodo setup",
        || trust_caddy(p, r),
    )?;

    s.do_step(
        "sudo runs the resolver script without a password",
        "lodo setup",
        || prove_script(deps, p, r).map(|()| StepOutcome::Done(String::new())),
    )?;

    if fsutil::exists(&p.menubar_plist) {
        s.do_step("menu bar icon up to date", "lodo menubar", || {
            menubar::refresh(p, r).map(|()| StepOutcome::Done(String::new()))
        })?;
    }

    let left: Vec<&PathBuf> = p.leftovers.iter().filter(|l| fsutil::exists(l)).collect();
    if !left.is_empty() {
        let _ = writeln!(
            s.out,
            "Left alone, from LocalDNS (delete them by hand when you no longer need it):"
        );
        for l in left {
            s.note(&l.display().to_string());
        }
    }

    Ok(())
}

fn preflight(p: &Paths) -> Result<(), String> {
    for (path, what) in [
        (&p.brew, "Homebrew at /opt/homebrew"),
        (&p.dnsmasq, "dnsmasq"),
        (&p.system_conf, "Homebrew's dnsmasq.conf"),
    ] {
        if !fsutil::exists(path) {
            return Err(format!("{what} not found at {}", path.display()));
        }
    }
    Ok(())
}

fn user_files<D: Deps>(deps: &D, p: &Paths) -> Result<(), String> {
    fs::create_dir_all(&p.config_dir)
        .map_err(|e| format!("create {}: {e}", p.config_dir.display()))?;
    if !fsutil::exists(&p.domains_json) {
        store::save(&p.domains_json, &[]).map_err(|e| e.to_string())?;
    }
    let domains = store::load(&p.domains_json).map_err(|e| e.to_string())?;
    write_files_with(deps, p, &domains).map(|_| ())
}

fn system_conf(p: &Paths) -> Result<String, String> {
    let old =
        fs::read(&p.system_conf).map_err(|e| format!("read {}: {e}", p.system_conf.display()))?;
    let base = file_name_str(&p.system_conf_backup);
    let note = if fsutil::exists(&p.system_conf_backup) {
        format!("backup kept in {base}")
    } else {
        format!("backup in {base}")
    };
    back_up(&p.system_conf_backup, &old, strip_system_conf, false)?;
    let old_str = String::from_utf8_lossy(&old);
    fsutil::write_file(
        &p.system_conf,
        rewrite_system_conf(&old_str, p).as_bytes(),
        0o644,
    )
    .map_err(|e| e.to_string())?;
    Ok(note)
}

fn stop_system_dnsmasq(p: &Paths, r: &dyn Runner) -> Result<String, String> {
    let launchctl = p.launchctl.to_string_lossy();
    let sudo = p.sudo.to_string_lossy();
    let rm = p.rm.to_string_lossy();
    let mut done = Vec::new();

    for label in DNS_SYSTEM_LABELS {
        let sys_label = format!("system/{label}");
        if r.run(None, &launchctl, &["print", &sys_label]).is_ok() {
            r.run(None, &sudo, &["-n", &launchctl, "bootout", &sys_label])
                .map_err(|e| e.to_string())?;
            done.push(format!("stopped {sys_label}"));
        }
        let plist = p.launch_daemons.join(format!("{label}.plist"));
        if fsutil::exists(&plist) {
            let plist_s = plist.to_string_lossy();
            r.run(None, &sudo, &["-n", &rm, "-f", &plist_s])
                .map_err(|e| e.to_string())?;
            done.push(format!("removed {}", plist.display()));
        }
    }
    Ok(done.join(", "))
}

fn install_script(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    let sudo = p.sudo.to_string_lossy();
    let install = p.install.to_string_lossy();
    let script_dir = p
        .script
        .parent()
        .unwrap_or_else(|| Path::new("/"))
        .to_string_lossy();
    r.run(
        None,
        &sudo,
        &[
            "-n",
            &install,
            "-d",
            "-o",
            "root",
            "-g",
            "wheel",
            "-m",
            "755",
            &script_dir,
        ],
    )
    .map_err(|e| e.to_string())?;
    install_file(p, r, &script(p), &p.script, "755")
}

fn install_sudoers(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    let rule = sudoers(p)?;
    let staged = stage(p, &p.sudoers, &rule)?;
    let staged_s = staged.to_string_lossy();
    let visudo = p.visudo.to_string_lossy();
    r.run(None, &visudo, &["-c", "-f", &staged_s])
        .map_err(|e| format!("the rendered rule failed visudo, nothing installed: {e}"))?;

    let sudo = p.sudo.to_string_lossy();
    let install = p.install.to_string_lossy();
    let target_s = p.sudoers.to_string_lossy();
    r.run(
        None,
        &sudo,
        &[
            "-n", &install, "-o", "root", "-g", "wheel", "-m", "440", &staged_s, &target_s,
        ],
    )
    .map(|_| ())
    .map_err(|e| e.to_string())
}

fn install_loopback(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    install_file(p, r, &loopback_plist(), &p.loopback_plist, "644")?;
    let sudo = p.sudo.to_string_lossy();
    let launchctl = p.launchctl.to_string_lossy();
    let sys_label = format!("system/{LOOPBACK_LABEL}");
    // bootout fails when the job isn't loaded yet; bootstrap is what counts.
    let _ = r.run(None, &sudo, &["-n", &launchctl, "bootout", &sys_label]);
    let plist_s = p.loopback_plist.to_string_lossy();
    r.run(
        None,
        &sudo,
        &["-n", &launchctl, "bootstrap", "system", &plist_s],
    )
    .map(|_| ())
    .map_err(|e| e.to_string())
}

fn prove_script<D: Deps>(deps: &D, p: &Paths, r: &dyn Runner) -> Result<(), String> {
    let domains = store::load(&p.domains_json).map_err(|e| e.to_string())?;
    let sudo = p.sudo.to_string_lossy();
    let script_s = p.script.to_string_lossy();
    r.run_input(
        None,
        &deps.resolver_list(&domains),
        &sudo,
        &["-n", "-k", &script_s],
    )
    .map(|_| ())
    .map_err(|e| e.to_string())
}

fn remove_resolver_local(p: &Paths, r: &dyn Runner) -> Result<String, String> {
    if fs::symlink_metadata(&p.resolver_local).is_err() {
        return Ok(String::new());
    }
    let sudo = p.sudo.to_string_lossy();
    let rm = p.rm.to_string_lossy();
    let local_s = p.resolver_local.to_string_lossy();
    r.run(None, &sudo, &["-n", &rm, "-f", &local_s])
        .map_err(|e| e.to_string())?;
    Ok("removed it: it sent every .local name away from Bonjour".to_string())
}

fn setup_caddy<D: Deps>(deps: &D, p: &Paths, r: &dyn Runner) -> Result<StepOutcome, String> {
    if !fsutil::exists(&p.caddy) {
        return Ok(StepOutcome::Skipped(
            "Caddy is not installed; domains with a port need it: brew install caddy, then lodo setup"
                .to_string(),
        ));
    }
    let (old, had_old) = match fs::read(&p.system_caddyfile) {
        Ok(b) => (b, true),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => (Vec::new(), false),
        Err(e) => return Err(format!("read {}: {e}", p.system_caddyfile.display())),
    };
    if had_old || fsutil::exists(&p.system_caddyfile_backup) {
        back_up(
            &p.system_caddyfile_backup,
            &old,
            |s| strip_system_caddyfile(s, p),
            true,
        )?;
    }
    let old_str = String::from_utf8_lossy(&old);
    fsutil::write_file(
        &p.system_caddyfile,
        rewrite_system_caddyfile(&old_str, p).as_bytes(),
        0o644,
    )
    .map_err(|e| e.to_string())?;
    restart_caddy(deps, p, r)?;
    Ok(StepOutcome::Done(String::new()))
}

fn trust_caddy(p: &Paths, r: &dyn Runner) -> Result<StepOutcome, String> {
    let domains = store::load(&p.domains_json).map_err(|e| e.to_string())?;
    if !domains.iter().any(|d| d.enabled && d.https) {
        return Ok(StepOutcome::Skipped(
            "no enabled name has HTTPS on".to_string(),
        ));
    }
    if !fsutil::exists(&p.caddy) {
        return Ok(StepOutcome::Skipped(
            "Caddy is not installed; names with HTTPS on need it: brew install caddy, then lodo setup"
                .to_string(),
        ));
    }
    let root = read_caddy_root(p)?;
    let old_ca = fs::read(&p.trusted_ca).ok();
    let had = old_ca.is_some();

    let sudo = p.sudo.to_string_lossy();
    let security = p.security.to_string_lossy();
    let trusted_s = p.trusted_ca.to_string_lossy();

    if old_ca.as_deref() == Some(root.as_slice()) {
        if r.run(None, &security, &["verify-cert", "-c", &trusted_s])
            .is_ok()
        {
            return Ok(StepOutcome::Done(String::new()));
        }
    } else if had {
        let _ = r.run_tty(&sudo, &[&security, "remove-trusted-cert", "-d", &trusted_s]);
    }

    fsutil::write_file(&p.trusted_ca, &root, 0o644).map_err(|e| e.to_string())?;

    let keychain_s = p.system_keychain.to_string_lossy();
    if let Err(err) = r.run_tty(
        &sudo,
        &[
            &security,
            "add-trusted-cert",
            "-d",
            "-r",
            "trustRoot",
            "-k",
            &keychain_s,
            &trusted_s,
        ],
    ) {
        if !had {
            let _ = fs::remove_file(&p.trusted_ca);
        }
        return Err(err.to_string());
    }
    Ok(StepOutcome::Done(String::new()))
}

fn read_caddy_root(p: &Paths) -> Result<Vec<u8>, String> {
    for attempt in 1..=TRUST_TRIES {
        match fs::symlink_metadata(&p.caddy_root) {
            Ok(info) => {
                owned_regular(&info)
                    .map_err(|e| format!("caddy's root {}: {e}", p.caddy_root.display()))?;
                let mut file =
                    File::open(&p.caddy_root).map_err(|e| format!("read caddy's root: {e}"))?;
                let opened = file
                    .metadata()
                    .map_err(|e| format!("read caddy's root: {e}"))?;
                owned_regular(&opened)
                    .map_err(|e| format!("caddy's root {}: {e}", p.caddy_root.display()))?;
                if opened.dev() != info.dev() || opened.ino() != info.ino() {
                    return Err(format!(
                        "caddy's root {}: changed while reading",
                        p.caddy_root.display()
                    ));
                }
                let mut data = Vec::new();
                file.read_to_end(&mut data)
                    .map_err(|e| format!("read caddy's root: {e}"))?;
                return Ok(data);
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                if attempt == TRUST_TRIES {
                    return Err(format!(
                        "caddy has not made its root {}; check caddy runs: brew services info caddy",
                        p.caddy_root.display()
                    ));
                }
                let wait_ms = TRUST_WAIT_MS.load(Ordering::Relaxed);
                if wait_ms > 0 {
                    thread::sleep(Duration::from_millis(wait_ms));
                }
            }
            Err(e) => return Err(format!("check caddy's root: {e}")),
        }
    }
    Err(format!(
        "caddy has not made its root {}; check caddy runs: brew services info caddy",
        p.caddy_root.display()
    ))
}

fn owned_regular(info: &fs::Metadata) -> Result<(), String> {
    if !info.file_type().is_file() {
        return Err("not a regular file".to_string());
    }
    let file_uid = info.uid();
    let my_uid = Uid::current().as_raw();
    if file_uid != my_uid {
        return Err(format!("owned by uid {file_uid}, not you"));
    }
    Ok(())
}

fn back_up(
    backup: &Path,
    current: &[u8],
    strip: impl FnOnce(&str) -> String,
    strip_unmarked: bool,
) -> Result<(), String> {
    let content = match fs::read(backup) {
        Ok(b) => b,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => current.to_vec(),
        Err(e) => return Err(format!("read {}: {e}", backup.display())),
    };
    let content_str = String::from_utf8_lossy(&content);
    let marked = content_str.split('\n').any(|l| l.trim() == MARKER);
    if !marked && !strip_unmarked {
        fsutil::write_file(backup, &content, 0o644).map_err(|e| e.to_string())?;
        return Ok(());
    }
    let rest = strip(&content_str);
    if rest.trim().is_empty() {
        return remove_if_exists(backup);
    }
    fsutil::write_file(backup, rest.as_bytes(), 0o644).map_err(|e| e.to_string())?;
    Ok(())
}

fn install_file(
    p: &Paths,
    r: &dyn Runner,
    content: &str,
    target: &Path,
    mode: &str,
) -> Result<(), String> {
    let staged = stage(p, target, content)?;
    let sudo = p.sudo.to_string_lossy();
    let install = p.install.to_string_lossy();
    let staged_s = staged.to_string_lossy();
    let target_s = target.to_string_lossy();
    r.run(
        None,
        &sudo,
        &[
            "-n", &install, "-o", "root", "-g", "wheel", "-m", mode, &staged_s, &target_s,
        ],
    )
    .map(|_| ())
    .map_err(|e| e.to_string())
}

fn stage(p: &Paths, target: &Path, content: &str) -> Result<PathBuf, String> {
    let base = file_name_str(target);
    let staged = p.staging.join(&base);
    let mut f = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(&staged)
        .map_err(|e| format!("stage {base}: {e}"))?;
    f.write_all(content.as_bytes())
        .map_err(|e| format!("stage {base}: {e}"))?;
    Ok(staged)
}
