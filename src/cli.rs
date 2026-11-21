//! Subcommand dispatch and CLI flags.

mod dev;
mod domains;
mod flags;
mod format;
#[cfg(test)]
mod tests;

use std::fs;
use std::io::{IsTerminal, Write};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use rustls::RootCertStore;

use crate::paths::Paths;
use crate::run::{ExecRunner, Runner};
use crate::store::{self, Domain};
use crate::{check, dnsmasq, fsutil, ls, menubar, scan, system};

pub use dev::project_env;
pub use format::{
    format_checks, format_proposal, format_results, format_sites, format_vars, probe_code,
};

/// Top-level CLI help text listing all supported commands and flags.
pub const USAGE: &str = "\
lodo manages .lodo and custom local domain names on macOS through dnsmasq and Caddy.

Usage:
  lodo dev                          start the project in this folder on its names
                                    (--name NAME)
  lodo exec [--] CMD...             run CMD here with the variables lodo dev sets
  lodo ls                           list services and domains with their checks
                                    (--json, --no-color)
  lodo add <name>                   add a domain (--address IP, --port PORT,
                                    --https, --compose PATH)
  lodo set <name>                   update a domain (--name NEW, --address IP,
                                    --port PORT|none, --on|--off,
                                    --https|--no-https, --compose PATH|none)
  lodo rm <name>                    remove a domain and its subdomains
  lodo service <svc> <on|off>       start or stop dnsmasq, caddy, or menubar
  lodo menubar                      show the macOS menu bar icon (--stop hides it)
  lodo setup                        one-time system setup; asks for your password
  lodo apply                        write configs from domains.json, reload, and probe
  lodo doctor                       check every part and print what to fix
  lodo scan                         print what the project in this folder needs
                                    (--json, --name NAME)
  lodo prune                        remove listed names whose project folder is gone
                                    (--worktree also removes this git worktree)
  lodo uninstall                    remove what setup installed; keeps domains.json
  lodo version                      print the version
  lodo help                         print this help
";

/// Lookup callback resolving an executable command name (such as `pnpm` or `node`) on `PATH`.
pub type LookPathFn<'a> = Box<dyn Fn(&str) -> Result<PathBuf, String> + 'a>;

/// Injectable CLI runtime so unit and integration tests can run every subcommand hermetically.
pub struct App<'a> {
    pub paths: Paths,
    pub runner: &'a dyn Runner,
    pub dns: String,
    pub http_port: u16,
    pub https_port: u16,
    pub root_cas: Option<Arc<RootCertStore>>,
    pub root_uid: u32,
    pub localhost: Vec<String>,
    pub timeout: Option<Duration>,
    pub cwd: PathBuf,
    pub look_path: LookPathFn<'a>,
    pub color: bool,
    pub environ: Option<Vec<(String, String)>>,
}

impl<'a> App<'a> {
    /// Creates an [`App`] targeting `paths` and `runner` with standard defaults.
    pub fn new(paths: Paths, runner: &'a dyn Runner) -> Self {
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
            cwd: std::env::current_dir().unwrap_or_default(),
            look_path: Box::new(default_look_path),
            color: false,
            environ: None,
        }
    }

    /// Builds a [`check::Env`] configured with this app's paths, runner, and probe overrides.
    pub fn check_env(&self) -> check::Env<'_> {
        let mut env = check::Env::new(&self.paths, self.runner);
        env.dns.clone_from(&self.dns);
        env.http_port = self.http_port;
        env.https_port = self.https_port;
        env.root_cas.clone_from(&self.root_cas);
        env.root_uid = self.root_uid;
        env.localhost.clone_from(&self.localhost);
        env.timeout = self.timeout;
        env
    }

    /// Parses and runs `args`, returning `0` on success, `1` on runtime failure, or `2` on usage error.
    pub fn dispatch(&self, args: &[String], stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
        let parsed = match flags::parse(args) {
            Ok(cmd) => cmd,
            Err(err) => {
                let _ = write!(stderr, "{err}\n\n{USAGE}");
                return 2;
            }
        };
        self.execute(parsed, stdout, stderr)
    }

    /// Alias for [`App::dispatch`].
    pub fn run(&self, args: &[String], stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
        self.dispatch(args, stdout, stderr)
    }

    fn execute(&self, cmd: flags::Command, stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
        match cmd {
            flags::Command::Help => {
                let _ = write!(stdout, "{USAGE}");
                0
            }
            flags::Command::Version => {
                let _ = writeln!(stdout, "{}", crate::VERSION);
                0
            }
            flags::Command::Setup => self.cmd_setup(stdout, stderr),
            flags::Command::Apply => self.cmd_apply(stdout, stderr),
            flags::Command::Doctor => self.cmd_doctor(stdout, stderr),
            flags::Command::Uninstall => self.cmd_uninstall(stdout, stderr),
            flags::Command::Menubar { stop } => self.cmd_menubar(stop, stdout, stderr),
            flags::Command::Ls { json, no_color } => self.cmd_ls(json, no_color, stdout, stderr),
            flags::Command::Scan { json, name } => self.cmd_scan(json, &name, stdout, stderr),
            flags::Command::Prune { worktree } => self.cmd_prune(worktree, stdout, stderr),
            flags::Command::Dev { name } => self.cmd_dev(&name, stdout, stderr),
            flags::Command::Exec { args } => self.cmd_exec(&args, stderr),
            flags::Command::Add(a) => self.cmd_add(a, stdout, stderr),
            flags::Command::Set(s) => self.cmd_set(s, stdout, stderr),
            flags::Command::Rm { name } => self.cmd_rm(&name, stdout, stderr),
            flags::Command::Service { service, on } => {
                self.cmd_service(&service, on, stdout, stderr)
            }
        }
    }

    fn cmd_setup(&self, stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
        if let Err(se) = system::setup(&self.paths, self.runner, stdout) {
            self.fail_step(&se, stderr);
            return 1;
        }
        if !fsutil::exists(&self.paths.menubar_plist) && menubar::can_start(&self.paths) {
            match menubar::start(&self.paths, self.runner) {
                Ok(()) => {
                    let _ = writeln!(stdout, "✓ menu bar icon started");
                }
                Err(err) => {
                    self.fail_step(
                        &system::StepError {
                            step: "menu bar icon".to_string(),
                            err,
                            fix: "lodo menubar".to_string(),
                        },
                        stderr,
                    );
                    return 1;
                }
            }
        }
        let _ = writeln!(stdout);
        self.cmd_doctor(stdout, stderr)
    }

    fn cmd_apply(&self, stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
        let mut loaded_domains: Vec<Domain> = Vec::new();
        let mut load_err: Option<String> = None;
        let mut apply_err: Option<String> = None;

        let lock_res = store::with_lock(
            &self.paths.domains_json,
            || -> Result<(), store::StoreError> {
                match store::load(&self.paths.domains_json) {
                    Ok(ds) => {
                        if let Err(e) = system::apply(&self.paths, self.runner, &ds) {
                            apply_err = Some(e);
                        }
                        loaded_domains = ds;
                    }
                    Err(e) => {
                        load_err = Some(e.to_string());
                    }
                }
                Ok(())
            },
        );

        if let Err(e) = lock_res {
            self.fail(&e.to_string(), stderr);
            return 1;
        }
        if let Some(err) = load_err {
            self.fail(&err, stderr);
            return 1;
        }
        if apply_err.as_deref() == Some(system::ERR_NOT_SET_UP) {
            self.fail(system::ERR_NOT_SET_UP, stderr);
            return 1;
        }

        let results = self.check_env().probe(&loaded_domains);
        let _ = write!(stdout, "{}", format_results(&results));
        if let Some(err) = apply_err {
            self.fail(&err, stderr);
            return 1;
        }
        probe_code(&results)
    }

    fn cmd_doctor(&self, stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
        let mut code = 0;
        let domains = match store::load(&self.paths.domains_json) {
            Ok(d) => d,
            Err(e) => {
                self.fail(&e.to_string(), stderr);
                code = 1;
                Vec::new()
            }
        };
        let checks = self.check_env().run(&domains);
        let _ = write!(stdout, "{}", format_checks(&checks));
        if !check::failed(&checks).is_empty() {
            code = 1;
        }
        code
    }

    fn cmd_uninstall(&self, stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
        if let Err(se) = system::uninstall(&self.paths, self.runner, stdout) {
            self.fail_step(&se, stderr);
            return 1;
        }
        0
    }

    fn cmd_menubar(&self, stop: bool, stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
        if stop {
            if let Err(err) = menubar::stop(&self.paths, self.runner) {
                self.fail(&err, stderr);
                return 1;
            }
            let _ = writeln!(stdout, "menubar stopped");
            return 0;
        }
        if !system::setup_done(&self.paths) {
            self.fail(system::ERR_NOT_SET_UP, stderr);
            return 1;
        }
        if let Err(err) = menubar::start(&self.paths, self.runner) {
            self.fail(&err, stderr);
            return 1;
        }
        let _ = writeln!(stdout, "menubar running");
        0
    }

    fn cmd_ls(
        &self,
        json: bool,
        no_color: bool,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> u8 {
        if json {
            let domains = match store::load(&self.paths.domains_json) {
                Ok(d) => d,
                Err(e) => {
                    self.fail(&e.to_string(), stderr);
                    return 1;
                }
            };
            let env = self.check_env();
            let report = ls::build_report(&env, &domains);
            match serde_json::to_string_pretty(&report) {
                Ok(out) => {
                    let _ = writeln!(stdout, "{out}");
                    0
                }
                Err(e) => {
                    self.fail(&format!("write ls json: {e}"), stderr);
                    1
                }
            }
        } else {
            let Some(domains) = self.ready("lodo ls runs", stderr) else {
                return 1;
            };
            let env = self.check_env();
            let report = ls::build_report(&env, &domains);
            let use_color = self.color && !no_color;
            let _ = write!(
                stdout,
                "{}",
                ls::format_table(&report, &self.paths.home, use_color)
            );
            0
        }
    }

    fn cmd_scan(
        &self,
        json: bool,
        raw_name: &str,
        stdout: &mut dyn Write,
        stderr: &mut dyn Write,
    ) -> u8 {
        let Some(name) = self.project_flag(raw_name, stderr) else {
            return 2;
        };
        let cwd = match self.resolve_cwd() {
            Ok(d) => d,
            Err(e) => {
                self.fail(&e, stderr);
                return 1;
            }
        };
        let domains = match store::load(&self.paths.domains_json) {
            Ok(d) => d,
            Err(e) => {
                self.fail(&e.to_string(), stderr);
                return 1;
            }
        };
        let Some(project) = scan::find(&cwd) else {
            self.fail(scan::ERR_NO_PROJECT, stderr);
            return 1;
        };
        let proposal = scan::propose(&project, &domains, &name, domains::is_gone);
        if json {
            match serde_json::to_string_pretty(&proposal) {
                Ok(out) => {
                    let _ = writeln!(stdout, "{out}");
                    0
                }
                Err(e) => {
                    self.fail(&format!("write the scan: {e}"), stderr);
                    1
                }
            }
        } else {
            let _ = write!(stdout, "{}", format_proposal(&proposal, &self.paths.home));
            0
        }
    }

    pub(super) fn ready(&self, what: &str, stderr: &mut dyn Write) -> Option<Vec<Domain>> {
        let domains = match store::load(&self.paths.domains_json) {
            Ok(d) => d,
            Err(e) => {
                self.fail(&e.to_string(), stderr);
                return None;
            }
        };
        let failed = check::failed(&self.check_env().prerequisites(&domains));
        if !failed.is_empty() {
            let _ = write!(stderr, "{}", format_checks(&failed));
            let _ = writeln!(
                stderr,
                "{what} once these pass; lodo doctor shows every check."
            );
            return None;
        }
        Some(domains)
    }

    pub(super) fn project_flag(&self, raw: &str, stderr: &mut dyn Write) -> Option<String> {
        if raw.is_empty() {
            return Some(String::new());
        }
        let name = store::resolve_name(raw);
        if let Err(err) = store::validate_project(&name) {
            self.fail(&format!("--name {raw}: {err}"), stderr);
            return None;
        }
        Some(name)
    }

    pub(super) fn resolve_cwd(&self) -> Result<PathBuf, String> {
        if !self.cwd.as_os_str().is_empty() {
            return Ok(self.cwd.clone());
        }
        std::env::current_dir().map_err(|e| format!("find the current folder: {e}"))
    }

    pub(super) fn optional_cwd(&self) -> Option<PathBuf> {
        if !self.cwd.as_os_str().is_empty() {
            Some(self.cwd.clone())
        } else {
            std::env::current_dir().ok()
        }
    }

    pub(super) fn base_env(&self) -> Vec<(String, String)> {
        match &self.environ {
            Some(vars) => vars.clone(),
            None => std::env::vars().collect(),
        }
    }

    pub(super) fn fail(&self, msg: &str, stderr: &mut dyn Write) {
        let _ = writeln!(stderr, "✗ {msg}");
    }

    pub(super) fn fail_step(&self, se: &system::StepError, stderr: &mut dyn Write) {
        let _ = writeln!(stderr, "✗ {}\n  {}\n  fix: {}", se.step, se.err, se.fix);
    }
}

/// Runs one `lodo` CLI command against the real macOS environment, returning exit code `0`, `1`, or `2`.
pub fn dispatch(args: &[String], stdout: &mut dyn Write, stderr: &mut dyn Write) -> u8 {
    dispatch_with_euid(args, nix::unistd::geteuid().is_root(), stdout, stderr)
}

pub(crate) fn dispatch_with_euid(
    args: &[String],
    is_root: bool,
    stdout: &mut dyn Write,
    stderr: &mut dyn Write,
) -> u8 {
    let parsed = match flags::parse(args) {
        Ok(cmd) => cmd,
        Err(err) => {
            let _ = write!(stderr, "{err}\n\n{USAGE}");
            return 2;
        }
    };

    match parsed {
        flags::Command::Help => {
            let _ = write!(stdout, "{USAGE}");
            return 0;
        }
        flags::Command::Version => {
            let _ = writeln!(stdout, "{}", crate::VERSION);
            return 0;
        }
        _ => {}
    }

    if is_root {
        let _ = writeln!(
            stderr,
            "lodo: run lodo as your user, not with sudo; it asks for your password when it needs it"
        );
        return 2;
    }

    let paths = match Paths::default_paths() {
        Ok(p) => p,
        Err(e) => {
            let _ = writeln!(stderr, "lodo: {e}");
            return 1;
        }
    };

    let runner = ExecRunner::default();
    let mut app = App::new(paths, &runner);
    app.color = should_use_color();
    app.execute(parsed, stdout, stderr)
}

fn should_use_color() -> bool {
    std::io::stdout().is_terminal()
        && std::env::var_os("NO_COLOR").is_none()
        && std::env::var("TERM").as_deref() != Ok("dumb")
}

#[derive(Debug)]
pub(super) struct CliError(pub(super) String);

impl std::fmt::Display for CliError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl From<store::StoreError> for CliError {
    fn from(err: store::StoreError) -> Self {
        Self(err.to_string())
    }
}

impl From<store::FieldError> for CliError {
    fn from(err: store::FieldError) -> Self {
        Self(err.to_string())
    }
}

impl From<String> for CliError {
    fn from(msg: String) -> Self {
        Self(msg)
    }
}

impl From<&str> for CliError {
    fn from(msg: &str) -> Self {
        Self(msg.to_string())
    }
}

pub(super) fn with_domains_lock<T>(
    path: &Path,
    f: impl FnOnce() -> Result<T, CliError>,
) -> Result<T, String> {
    store::with_lock(path, f).map_err(|e| e.0)
}

fn default_look_path(file: &str) -> Result<PathBuf, String> {
    let is_executable = |p: &Path| -> bool {
        fs::metadata(p).is_ok_and(|m| m.is_file() && (m.permissions().mode() & 0o111) != 0)
    };
    if file.contains('/') {
        let p = Path::new(file);
        if is_executable(p) {
            return Ok(p.to_path_buf());
        }
        return Err(format!("{file}: executable file not found"));
    }
    if let Some(path_var) = std::env::var_os("PATH") {
        for dir in std::env::split_paths(&path_var) {
            let candidate = dir.join(file);
            if is_executable(&candidate) {
                return Ok(candidate);
            }
        }
    }
    Err(format!("executable file {file:?} not found in $PATH"))
}
