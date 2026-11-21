//! Domain and service management commands: `add`, `set`, `rm`, `service`, and `prune`.

use std::collections::HashSet;
use std::fs;
use std::io::Write;
use std::path::Path;

use crate::paths::Paths;
use crate::store::{self, Domain};
use crate::{check, clean, compose, fsutil, ls, scan, system};

use super::flags::{AddArgs, SetArgs};
use super::{App, CliError, with_domains_lock};

pub(super) fn is_gone(path: &Path) -> bool {
    matches!(fs::metadata(path), Err(e) if e.kind() == std::io::ErrorKind::NotFound)
}

pub(super) fn write_clean_outcome(
    outcome: &clean::Outcome,
    stdout: &mut dyn Write,
    stderr: &mut dyn Write,
) {
    for line in outcome.summary_lines() {
        let _ = writeln!(stdout, "{line}");
    }
    for note in &outcome.notes {
        let _ = writeln!(stderr, "note  {note}");
    }
}

pub(super) fn ports_ready(p: &Paths) -> Result<(), String> {
    if !fsutil::exists(&p.caddy) {
        return Err("caddy is not installed: brew install caddy, then lodo setup".to_string());
    }
    if !system::caddy_set_up(p) {
        return Err("caddy is not set up for lodo: run lodo setup".to_string());
    }
    Ok(())
}

fn expand_compose(raw: &str, home: &Path, cwd: Option<&Path>) -> Result<String, String> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return Ok(String::new());
    }
    if trimmed.eq_ignore_ascii_case(store::NO_COMPOSE) {
        return Ok(store::NO_COMPOSE.to_string());
    }
    let resolved = if let Some(rest) = trimmed.strip_prefix("~/")
        && !home.as_os_str().is_empty()
    {
        compose::clean_path(&home.join(rest))
    } else if !Path::new(trimmed).is_absolute() {
        let Some(dir) = cwd.filter(|d| !d.as_os_str().is_empty()) else {
            return Err("find the current folder: current directory is unavailable".to_string());
        };
        compose::clean_path(&dir.join(trimmed))
    } else {
        compose::clean_path(Path::new(trimmed))
    };
    Ok(resolved.to_string_lossy().into_owned())
}

fn check_compose(path: &str, home: &Path) -> Result<(), String> {
    store::validate_compose(path).map_err(|e| e.to_string())?;
    if !store::has_compose(path) {
        return Ok(());
    }
    if let Err(err) = fsutil::read_regular(Path::new(path)) {
        if err.is_not_found() {
            return Err(format!("no file at {}", ls::short_path(path, home)));
        }
        return Err(format!(
            "can't read it: {}",
            check::one_line(&err.to_string())
        ));
    }
    Ok(())
}

impl App<'_> {
    pub(super) fn cmd_add(
        &self,
        args: AddArgs,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> u8 {
        let res = (|| -> Result<Domain, String> {
            let name = store::resolve_name(&args.name);
            let port = match &args.port {
                Some(s) => store::parse_port(s).map_err(|e| e.to_string())?,
                None => 0,
            };
            let compose_path = match &args.compose {
                Some(raw) => {
                    let cwd = self.optional_cwd();
                    let expanded = expand_compose(raw, &self.paths.home, cwd.as_deref())?;
                    check_compose(&expanded, &self.paths.home)?;
                    expanded
                }
                None => String::new(),
            };

            with_domains_lock(&self.paths.domains_json, || -> Result<Domain, CliError> {
                let domains = store::load(&self.paths.domains_json)?;
                let address = match &args.address {
                    Some(ip) => ip.trim().to_string(),
                    None => match store::parent(&domains, &name) {
                        Some(p) => p.address.clone(),
                        None => store::next_free(&domains)?,
                    },
                };
                let d = Domain {
                    name,
                    address,
                    port,
                    enabled: true,
                    compose: compose_path,
                    https: args.https,
                    root: String::new(),
                    worktree: false,
                };
                let next = store::add(&domains, d.clone())?;
                if d.port > 0 {
                    ports_ready(&self.paths)?;
                }
                if !system::setup_done(&self.paths) {
                    return Err(system::ERR_NOT_SET_UP.into());
                }
                store::save(&self.paths.domains_json, &next)?;
                system::apply(&self.paths, self.runner, &next)?;
                Ok(d)
            })
        })();

        match res {
            Ok(d) => {
                if d.port > 0 {
                    let _ = writeln!(stdout, "added {} -> {}:{}", d.name, d.address, d.port);
                } else {
                    let _ = writeln!(stdout, "added {} -> {}", d.name, d.address);
                }
                0
            }
            Err(msg) => {
                self.fail(&msg, stderr);
                1
            }
        }
    }

    pub(super) fn cmd_set(
        &self,
        args: SetArgs,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> u8 {
        let res = (|| -> Result<Domain, String> {
            let expanded_compose = match &args.compose {
                Some(raw) => {
                    let cwd = self.optional_cwd();
                    Some(expand_compose(raw, &self.paths.home, cwd.as_deref())?)
                }
                None => None,
            };

            with_domains_lock(&self.paths.domains_json, || -> Result<Domain, CliError> {
                let domains = store::load(&self.paths.domains_json)?;
                let trimmed = args.target.trim();
                let target_name = if store::find(&domains, trimmed).is_some() {
                    trimmed.to_string()
                } else {
                    store::resolve_name(trimmed)
                };
                let Some(old) = store::find(&domains, &target_name).cloned() else {
                    return Err(store::StoreError::NotListed(target_name).into());
                };

                let mut d = old.clone();
                if let Some(new_name) = &args.name {
                    d.name = store::resolve_name(new_name);
                }
                if let Some(addr) = &args.address {
                    d.address = addr.trim().to_string();
                }
                if let Some(port_raw) = &args.port {
                    let p_trim = port_raw.trim();
                    if p_trim == "0" || p_trim.is_empty() || p_trim.eq_ignore_ascii_case("none") {
                        d.port = 0;
                        d.https = false;
                    } else {
                        d.port = store::parse_port(p_trim)?;
                    }
                }
                if let Some(en) = args.enabled {
                    d.enabled = en;
                }
                if let Some(h) = args.https {
                    d.https = h;
                }
                if let Some(comp) = expanded_compose {
                    d.compose = comp;
                }

                if d.port > 0 && d.port != old.port {
                    ports_ready(&self.paths)?;
                }
                if d.compose != old.compose {
                    check_compose(&d.compose, &self.paths.home)?;
                }

                let next = store::update(&domains, &old.name, d.clone())?;
                let routing_changed = d.name != old.name
                    || d.address != old.address
                    || d.port != old.port
                    || d.enabled != old.enabled
                    || d.https != old.https;

                if routing_changed && !system::setup_done(&self.paths) {
                    return Err(system::ERR_NOT_SET_UP.into());
                }

                store::save(&self.paths.domains_json, &next)?;

                if d.name != old.name {
                    let old_proj = store::project(&old.name);
                    if !next.iter().any(|x| store::project(&x.name) == old_proj) {
                        let _ = fs::remove_file(self.paths.dev_dir.join(format!("{old_proj}.cjs")));
                    }
                }

                if routing_changed {
                    system::apply(&self.paths, self.runner, &next)?;
                }
                Ok(d)
            })
        })();

        match res {
            Ok(d) => {
                let _ = writeln!(stdout, "updated {}", d.name);
                0
            }
            Err(msg) => {
                self.fail(&msg, stderr);
                1
            }
        }
    }

    pub(super) fn cmd_rm(
        &self,
        raw_name: &str,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> u8 {
        let res = with_domains_lock(
            &self.paths.domains_json,
            || -> Result<Vec<Domain>, CliError> {
                if !system::setup_done(&self.paths) {
                    return Err(system::ERR_NOT_SET_UP.into());
                }
                let domains = store::load(&self.paths.domains_json)?;
                let trimmed = raw_name.trim();
                let name = if store::find(&domains, trimmed).is_some() {
                    trimmed.to_string()
                } else {
                    store::resolve_name(trimmed)
                };
                let next = store::remove(&domains, &name)?;
                store::save(&self.paths.domains_json, &next)?;

                let mut removed = Vec::new();
                for d in &domains {
                    if store::find(&next, &d.name).is_none() {
                        let proj = store::project(&d.name);
                        if !next.iter().any(|x| store::project(&x.name) == proj) {
                            let _ = fs::remove_file(self.paths.dev_dir.join(format!("{proj}.cjs")));
                        }
                        removed.push(d.clone());
                    }
                }
                system::apply(&self.paths, self.runner, &next)?;
                Ok(removed)
            },
        );

        match res {
            Ok(removed) => {
                for d in removed {
                    let _ = writeln!(stdout, "removed {}", d.name);
                }
                0
            }
            Err(msg) => {
                self.fail(&msg, stderr);
                1
            }
        }
    }

    pub(super) fn cmd_service(
        &self,
        service: &str,
        on: bool,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> u8 {
        if let Err(msg) = system::set_service(&self.paths, self.runner, service, on) {
            self.fail(&msg, stderr);
            return 1;
        }
        let state = if on { "on" } else { "off" };
        let _ = writeln!(stdout, "{service} {state}");
        0
    }

    pub(super) fn cmd_prune(
        &self,
        worktree: bool,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> u8 {
        let cwd = match self.resolve_cwd() {
            Ok(d) => d,
            Err(e) => {
                self.fail(&e, stderr);
                return 1;
            }
        };
        if is_gone(&self.paths.domains_json) {
            let _ = writeln!(stdout, "nothing to prune");
            return 0;
        }
        let wt = if worktree {
            scan::worktree_root(&cwd)
        } else {
            None
        };

        let res = with_domains_lock(
            &self.paths.domains_json,
            || -> Result<(Vec<Domain>, Vec<clean::Outcome>), CliError> {
                let domains = store::load(&self.paths.domains_json)?;
                let mut next = domains.clone();
                for d in &domains {
                    if d.root.is_empty() {
                        continue;
                    }
                    let root_gone = is_gone(Path::new(&d.root));
                    let matches_wt = wt.as_ref().is_some_and(|w| scan::same_folder(&d.root, w));
                    if !root_gone && !matches_wt {
                        continue;
                    }
                    if let Ok(after) = store::remove(&next, &d.name) {
                        next = after;
                    }
                }
                if next.len() == domains.len() {
                    return Ok((Vec::new(), Vec::new()));
                }
                if !system::setup_done(&self.paths) {
                    return Err(system::ERR_NOT_SET_UP.into());
                }
                store::save(&self.paths.domains_json, &next)?;

                let mut removed = Vec::new();
                for d in &domains {
                    if store::find(&next, &d.name).is_none() {
                        let proj = store::project(&d.name);
                        if !next.iter().any(|x| store::project(&x.name) == proj) {
                            let _ = fs::remove_file(self.paths.dev_dir.join(format!("{proj}.cjs")));
                        }
                        removed.push(d.clone());
                    }
                }

                let mut cleaned = Vec::new();
                let mut seen_projects = HashSet::new();
                for d in &removed {
                    let proj = store::project(&d.name);
                    if next.iter().any(|x| store::project(&x.name) == proj)
                        || !seen_projects.insert(proj.to_string())
                    {
                        continue;
                    }
                    let owner = removed.iter().find(|x| x.name == proj).unwrap_or(d);
                    let compose = if !owner.compose.is_empty() {
                        owner.compose.clone()
                    } else {
                        removed
                            .iter()
                            .find(|x| store::project(&x.name) == proj && !x.compose.is_empty())
                            .map(|x| x.compose.clone())
                            .unwrap_or_default()
                    };
                    let is_wt = owner.worktree
                        || (!owner.root.is_empty()
                            && scan::worktree_root(Path::new(&owner.root)).is_some());
                    let target = clean::Target {
                        name: proj.to_string(),
                        address: owner.address.clone(),
                        compose,
                        worktree: is_wt,
                    };
                    cleaned.push(clean::project(&self.paths, self.runner, &target, &next));
                }

                system::apply(&self.paths, self.runner, &next)?;
                Ok((removed, cleaned))
            },
        );

        let (removed, cleaned) = match res {
            Ok(r) => r,
            Err(msg) => {
                self.fail(&msg, stderr);
                return 1;
            }
        };

        if removed.is_empty() {
            let _ = writeln!(stdout, "nothing to prune");
            return 0;
        }
        for d in removed {
            if !d.root.is_empty() {
                let _ = writeln!(
                    stdout,
                    "removed {} ({})",
                    d.name,
                    ls::short_path(&d.root, &self.paths.home)
                );
            } else {
                let _ = writeln!(stdout, "removed {}", d.name);
            }
        }
        for outcome in &cleaned {
            write_clean_outcome(outcome, stdout, stderr);
        }
        0
    }
}
