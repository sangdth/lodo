//! `lodo dev` and `lodo exec`: auto-registration, hook wiring, `NODE_OPTIONS`, and `.env` overrides.

use std::collections::HashSet;
use std::fs;
use std::io::Write;
use std::path::Path;

use crate::store::{self, Domain};
use crate::{clean, compose, dev, ls, menubar, scan, system};

use super::domains::{is_gone, ports_ready, write_clean_outcome};
use super::format::{compose_vars, format_sites, format_vars};
use super::{App, CliError, with_domains_lock};

/// Returns `env` with `proposal`'s compose and `.env` variables replacing any existing keys
/// without mutating the input slice.
pub fn project_env(env: &[(String, String)], proposal: &scan::Proposal) -> Vec<(String, String)> {
    let mut vars = compose_vars(proposal);
    for v in &proposal.env {
        let pair = v.env_pair();
        if let Some((k, raw_val)) = pair.split_once('=') {
            vars.push((k.to_string(), raw_val.to_string()));
        }
    }

    let keys: HashSet<&str> = vars.iter().map(|(k, _)| k.as_str()).collect();
    let mut out: Vec<(String, String)> = env
        .iter()
        .filter(|(k, _)| !keys.contains(k.as_str()))
        .cloned()
        .collect();
    out.extend(vars);
    out
}

fn node_config(pm: &str) -> Option<(&'static str, &'static str)> {
    match pm {
        "npm" => Some(("--prefix", "npm_config_node_options")),
        "pnpm" => Some(("--dir", "pnpm_config_node_options")),
        _ => None,
    }
}

impl App<'_> {
    pub(super) fn cmd_dev(
        &self,
        raw_name: &str,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> u8 {
        let Some(chosen_name) = self.project_flag(raw_name, stderr) else {
            return 2;
        };
        let cwd = match self.resolve_cwd() {
            Ok(d) => d,
            Err(e) => {
                self.fail(&e, stderr);
                return 1;
            }
        };
        if compose::project_root(&cwd).is_none() {
            self.fail(scan::ERR_NO_PROJECT, stderr);
            return 1;
        }
        if !system::setup_done(&self.paths) {
            if let Err(se) = system::setup(&self.paths, self.runner, stdout) {
                self.fail_step(&se, stderr);
                return 1;
            }
            let _ = writeln!(stdout);
        }
        let Some(domains) = self.ready("lodo dev starts", stderr) else {
            return 1;
        };
        if let Err(msg) = self.start_dev(&cwd, domains, &chosen_name, stdout, stderr) {
            self.fail(&msg, stderr);
            return 1;
        }
        0
    }

    pub(super) fn start_dev(
        &self,
        cwd: &Path,
        mut domains: Vec<Domain>,
        chosen_name: &str,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> Result<(), String> {
        let resolved_name = store::resolve_name(chosen_name);
        let project = scan::find(cwd).ok_or_else(|| scan::ERR_NO_PROJECT.to_string())?;
        let mut proposal = scan::propose(&project, &domains, &resolved_name, is_gone);

        let listed = domains.iter().any(|d| {
            scan::same_folder(&d.root, &proposal.root)
                && (resolved_name.is_empty() || d.name == resolved_name)
        });

        if !listed {
            domains = self.auto_register_project(cwd, &resolved_name, stdout, stderr)?;
            if let Some(fresh_proj) = scan::find(cwd) {
                proposal = scan::propose(&fresh_proj, &domains, "", is_gone);
            }
        }

        for d in &proposal.add {
            if let Some(app) = proposal.dirs.get(&d.name)
                && !app.is_empty()
            {
                let _ = writeln!(
                    stderr,
                    "warn  {app} has no name: lodo add {} adds it",
                    d.name
                );
            }
        }

        let ports = dev::ports(&domains, &proposal.name)?;
        let root_path = Path::new(&proposal.root);
        let pm = dev::package_manager(root_path)?;
        let pm_path = (self.look_path)(&pm).map_err(|e| format!("find {pm}: {e}"))?;
        let hook = dev::write_hooks(&self.paths, &proposal.name, &ports)?;
        let env = self.dev_env(&pm, &pm_path, &proposal.root, &hook)?;

        if menubar::can_start(&self.paths)
            && let Err(err) = menubar::ensure_running(&self.paths, self.runner)
        {
            let _ = writeln!(stderr, "warn  menubar: {err}");
        }

        let _ = write!(stdout, "{}", format_sites(&domains, &proposal.name));
        let _ = write!(stdout, "{}", format_vars(&proposal));
        let _ = writeln!(stdout, "starting: {pm} run dev");

        let pm_str = pm_path.to_string_lossy();
        let final_env = project_env(&env, &proposal);
        self.runner
            .replace(
                root_path,
                &pm_str,
                &["run".to_string(), "dev".to_string()],
                &final_env,
            )
            .map_err(|e| e.to_string())
    }

    fn auto_register_project(
        &self,
        cwd: &Path,
        chosen_name: &str,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> Result<Vec<Domain>, String> {
        let (domains, cleaned) = with_domains_lock(
            &self.paths.domains_json,
            || -> Result<(Vec<Domain>, Option<clean::Outcome>), CliError> {
                let fresh = store::load(&self.paths.domains_json)?;
                let fresh_proj = scan::find(cwd).ok_or(scan::ERR_NO_PROJECT)?;

                if !chosen_name.is_empty()
                    && let Some(existing) = store::find(&fresh, chosen_name)
                    && scan::taken(existing, &fresh_proj.root, is_gone)
                {
                    return Err(scan::belongs_to(
                        chosen_name,
                        &ls::short_path(&existing.root, &self.paths.home),
                    )
                    .into());
                }

                let mut working = fresh.clone();
                let mut removed_projects: Vec<String> = Vec::new();
                if !chosen_name.is_empty() {
                    let old_owners: Vec<Domain> = fresh
                        .iter()
                        .filter(|d| {
                            store::project(&d.name) == d.name
                                && d.name != chosen_name
                                && scan::same_folder(&d.root, &fresh_proj.root)
                        })
                        .cloned()
                        .collect();

                    if !old_owners.is_empty() {
                        let claiming_existing = store::find(&fresh, chosen_name).is_some();
                        for old_owner in &old_owners {
                            if let Ok(after) = store::remove(&working, &old_owner.name) {
                                working = after;
                            }
                            removed_projects.push(old_owner.name.clone());
                        }
                        if !claiming_existing && let Some(primary_old) = old_owners.first() {
                            let mut migrated_owner = primary_old.clone();
                            migrated_owner.name = chosen_name.to_string();
                            migrated_owner.root.clone_from(&fresh_proj.root);
                            migrated_owner.worktree = fresh_proj.is_worktree();
                            working = store::add(&working, migrated_owner)?;

                            let old_suffix = format!(".{}", primary_old.name);
                            for mut sub in store::under(&fresh, &primary_old.name) {
                                if let Some(prefix) = sub.name.strip_suffix(&old_suffix) {
                                    sub.name = format!("{prefix}.{chosen_name}");
                                    working = store::add(&working, sub)?;
                                }
                            }
                        }
                    }
                }

                let prop = scan::propose(&fresh_proj, &working, chosen_name, is_gone);
                if prop.name.is_empty() {
                    if let Some(note) = prop.notes.first() {
                        return Err(note.clone().into());
                    }
                    return Err(format!(
                        "its folder's name makes no {} name: pass --name <name>",
                        store::SUFFIX
                    )
                    .into());
                }
                if let Some(existing) = store::find(&working, &prop.name)
                    && scan::taken(existing, &fresh_proj.root, is_gone)
                {
                    return Err(scan::belongs_to(
                        &prop.name,
                        &ls::short_path(&existing.root, &self.paths.home),
                    )
                    .into());
                }

                let stale_owners: Vec<String> = working
                    .iter()
                    .filter(|d| {
                        store::project(&d.name) == d.name
                            && d.name != prop.name
                            && scan::same_folder(&d.root, &fresh_proj.root)
                    })
                    .map(|d| d.name.clone())
                    .collect();
                for stale in stale_owners {
                    if let Ok(after) = store::remove(&working, &stale) {
                        working = after;
                    }
                    removed_projects.push(stale);
                }

                if !prop.can_accept() && !prop.listed && working == fresh && prop.address.is_empty()
                {
                    if let Some(note) = prop.notes.first() {
                        return Err(note.clone().into());
                    }
                    return Ok((fresh, None));
                }

                let reclaimed_owner = store::find(&working, &prop.name)
                    .filter(|old| {
                        !old.root.is_empty()
                            && !scan::same_folder(&old.root, &prop.root)
                            && is_gone(Path::new(&old.root))
                    })
                    .cloned();

                let mut next = working;
                for mut d in prop.add {
                    if d.name == prop.name {
                        d.root.clone_from(&prop.root);
                        d.worktree = prop.worktree;
                        if !prop.compose.is_empty() {
                            d.compose.clone_from(&prop.compose);
                        }
                    }
                    next = store::add(&next, d)?;
                }

                if let Some(existing_owner) = store::find(&next, &prop.name).cloned() {
                    let mut updated = existing_owner.clone();
                    updated.root.clone_from(&prop.root);
                    updated.worktree = prop.worktree;
                    if !prop.compose.is_empty() {
                        updated.compose.clone_from(&prop.compose);
                    }
                    if updated != existing_owner {
                        next = store::update(&next, &prop.name, updated)?;
                    }
                }

                if next == fresh {
                    return Ok((fresh, None));
                }

                let needs_caddy = next.iter().any(|d| {
                    d.port > 0 && store::find(&fresh, &d.name).is_none_or(|old| old.port != d.port)
                });
                if needs_caddy {
                    ports_ready(&self.paths)?;
                }

                store::save(&self.paths.domains_json, &next)?;
                for old_proj in removed_projects {
                    if !next.iter().any(|x| store::project(&x.name) == old_proj) {
                        let _ = fs::remove_file(self.paths.dev_dir.join(format!("{old_proj}.cjs")));
                    }
                }
                let cleaned = if prop.worktree
                    && let Some(old) = reclaimed_owner
                {
                    let target = clean::Target {
                        name: old.name,
                        address: old.address,
                        compose: old.compose,
                        worktree: old.worktree || chosen_name.is_empty(),
                    };
                    Some(clean::project(&self.paths, self.runner, &target, &next))
                } else {
                    None
                };
                system::apply(&self.paths, self.runner, &next)?;
                Ok((next, cleaned))
            },
        )?;

        if let Some(outcome) = cleaned {
            write_clean_outcome(&outcome, stdout, stderr);
        }
        Ok(domains)
    }

    fn dev_env(
        &self,
        pm: &str,
        pm_path: &Path,
        root: &str,
        hook: &Path,
    ) -> Result<Vec<(String, String)>, String> {
        let base = self.base_env();
        let mut options = base
            .iter()
            .rfind(|(k, _)| k == "NODE_OPTIONS")
            .map(|(_, v)| v.clone())
            .unwrap_or_default();

        let conf = node_config(pm);
        if let Some((dir_flag, _)) = conf {
            let pm_str = pm_path.to_string_lossy();
            let out = self
                .runner
                .run(
                    None,
                    &pm_str,
                    &[dir_flag, root, "config", "get", "node-options"],
                )
                .map_err(|e| format!("read {pm}'s node-options: {e}"))?;
            let v = out.trim();
            if !v.is_empty() && v != "null" && v != "undefined" {
                options = if options.is_empty() {
                    v.to_string()
                } else {
                    format!("{v} {options}").trim().to_string()
                };
            }
        }

        options = dev::node_options(&options, hook);
        let mut env: Vec<(String, String)> = base
            .into_iter()
            .filter(|(k, _)| k != "NODE_OPTIONS" && conf.is_none_or(|(_, env_var)| k != env_var))
            .collect();
        env.push(("NODE_OPTIONS".to_string(), options.clone()));
        if let Some((_, env_var)) = conf {
            env.push((env_var.to_string(), options));
        }
        Ok(env)
    }

    pub(super) fn cmd_exec(&self, args: &[String], stderr: &mut dyn Write) -> u8 {
        let cwd = match self.resolve_cwd() {
            Ok(d) => d,
            Err(e) => {
                self.fail(&e, stderr);
                return 1;
            }
        };
        let Some(domains) = self.ready("lodo exec runs", stderr) else {
            return 1;
        };
        if let Err(msg) = self.start_exec(&cwd, &domains, args, stderr) {
            self.fail(&msg, stderr);
            return 1;
        }
        0
    }

    pub(super) fn start_exec(
        &self,
        cwd: &Path,
        domains: &[Domain],
        args: &[String],
        stderr: &mut dyn Write,
    ) -> Result<(), String> {
        let project = scan::find(cwd).ok_or_else(|| scan::ERR_NO_PROJECT.to_string())?;
        let proposal = scan::propose(&project, domains, "", is_gone);
        if !proposal.listed {
            return Err(format!(
                "{} has no names yet: run lodo dev there first",
                proposal.root
            ));
        }
        let Some(first) = args.first() else {
            return Err("exec needs a command to run".to_string());
        };
        let cmd_path = (self.look_path)(first).map_err(|e| format!("find {first}: {e}"))?;
        let _ = write!(stderr, "{}", format_vars(&proposal));

        let cmd_str = cmd_path.to_string_lossy();
        let final_env = project_env(&self.base_env(), &proposal);
        self.runner
            .replace(cwd, &cmd_str, &args[1..], &final_env)
            .map_err(|e| e.to_string())
    }
}
