//! macOS menu bar helper compilation and `io.lodo.menubar` LaunchAgent lifecycle.

use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::LazyLock;
use std::time::Duration;

use regex::Regex;

use crate::paths::Paths;
use crate::run::Runner;
use crate::{fsutil, store};

/// `launchd` label of the user LaunchAgent that keeps the menu bar icon running.
pub const LABEL: &str = "io.lodo.menubar";

/// Bounds `swiftc` compilation on a cold macOS SDK cache.
const BUILD_TIMEOUT: Duration = Duration::from_secs(120);

const SWIFT_SOURCE: &[u8] = include_bytes!("menubar.swift");

#[expect(clippy::expect_used)] // compile-time valid static regex
static PID_LINE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"pid = ([0-9]+)").expect("valid PID_LINE"));

/// Returns the embedded Swift source of the menu bar app.
pub fn source() -> &'static [u8] {
    SWIFT_SOURCE
}

/// Returns the menu bar binary path `lodo` runs: Homebrew's prebuilt helper when
/// installed, otherwise the locally compiled binary in `~/.config/lodo`.
pub fn active_binary_path(p: &Paths) -> PathBuf {
    if fsutil::exists(&p.brew_menubar_bin) {
        p.brew_menubar_bin.clone()
    } else {
        p.menubar_bin.clone()
    }
}

fn active_lodo_bin(p: &Paths) -> PathBuf {
    if fsutil::exists(&p.lodo_brew_bin) {
        p.lodo_brew_bin.clone()
    } else {
        std::env::current_exe().unwrap_or_else(|_| p.lodo_brew_bin.clone())
    }
}

/// Returns the LaunchAgent plist that starts the menu bar binary at login and
/// restarts it only on an abnormal exit so clean quits stay hidden.
pub fn plist(p: &Paths) -> String {
    let bin = xml_escape(&active_binary_path(p).to_string_lossy());
    let domains_json = xml_escape(&p.domains_json.to_string_lossy());
    let lodo_bin = xml_escape(&active_lodo_bin(p).to_string_lossy());
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{bin}</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>LODO_DOMAINS_JSON</key>
		<string>{domains_json}</string>
		<key>LODO_BIN</key>
		<string>{lodo_bin}</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
</dict>
</plist>
"#
    )
}

/// Reports whether a menu bar binary is available or can be compiled on this Mac.
pub fn can_start(p: &Paths) -> bool {
    fsutil::exists(&p.brew_menubar_bin) || compiled_up_to_date(p) || fsutil::exists(&p.swiftc)
}

/// Reports whether the menu bar LaunchAgent has a running process in the user's
/// `launchd` domain.
pub fn running(r: &dyn Runner, p: &Paths) -> bool {
    let launchctl = p.launchctl.to_string_lossy();
    let tgt = target(p);
    r.run(None, &launchctl, &["print", &tgt])
        .is_ok_and(|out| PID_LINE.is_match(&out))
}

/// Compiles the menu bar helper when needed, writes its LaunchAgent plist, and
/// starts or restarts the job in the user's GUI domain.
pub fn start(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    with_menubar_lock(p, || start_locked(p, r, None))
}

/// Ensures the menu bar LaunchAgent is installed, up to date, and running.
/// No-op when the plist and binary are up to date and the process is running.
pub fn ensure_running(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    with_menubar_lock(p, || {
        if up_to_date(p) {
            let launchctl = p.launchctl.to_string_lossy();
            let tgt = target(p);
            return match r.run(None, &launchctl, &["print", &tgt]) {
                Ok(out) if PID_LINE.is_match(&out) => Ok(()),
                Ok(_) => start_locked(p, r, Some(true)),
                Err(_) => start_locked(p, r, Some(false)),
            };
        }
        start_locked(p, r, None)
    })
}

fn with_menubar_lock<T>(p: &Paths, f: impl FnOnce() -> Result<T, String>) -> Result<T, String> {
    store::with_lock(&p.menubar_bin, || Ok::<_, store::StoreError>(f()))
        .map_err(|e| e.to_string())?
}

fn start_locked(p: &Paths, r: &dyn Runner, loaded: Option<bool>) -> Result<(), String> {
    ensure_binary(p, r)?;
    if let Some(dir) = p.menubar_plist.parent() {
        fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    }
    let plist_changed = fsutil::write_file(&p.menubar_plist, plist(p).as_bytes(), 0o644)
        .map_err(|e| e.to_string())?;

    let launchctl = p.launchctl.to_string_lossy();
    let tgt = target(p);
    let is_loaded = loaded.unwrap_or_else(|| r.run(None, &launchctl, &["print", &tgt]).is_ok());
    if is_loaded && !plist_changed {
        r.run(None, &launchctl, &["kickstart", "-k", &tgt])
            .map_err(|e| format!("restart menubar: {e}"))?;
        return Ok(());
    }
    if is_loaded {
        let _ = r.run(None, &launchctl, &["bootout", &tgt]);
    }
    let dom = domain(p);
    let plist_path = p.menubar_plist.to_string_lossy();
    r.run(None, &launchctl, &["bootstrap", &dom, &plist_path])
        .map_err(|e| format!("start menubar: {e}"))?;
    Ok(())
}

/// Rebuilds and restarts the menu bar helper when enabled and missing or stale.
/// No-op when disabled or already up to date.
pub fn refresh(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    if !fsutil::exists(&p.menubar_plist) || up_to_date(p) {
        return Ok(());
    }
    start(p, r)
}

/// Unloads the menu bar LaunchAgent if loaded and removes its plist so it stays
/// hidden across logins. Succeeds when already stopped.
pub fn stop(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    let launchctl = p.launchctl.to_string_lossy();
    let tgt = target(p);
    if r.run(None, &launchctl, &["print", &tgt]).is_ok() {
        r.run(None, &launchctl, &["bootout", &tgt])
            .map_err(|e| format!("stop menubar: {e}"))?;
    }
    match fs::remove_file(&p.menubar_plist) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(format!("remove {}: {e}", p.menubar_plist.display())),
    }
    Ok(())
}

fn up_to_date(p: &Paths) -> bool {
    if !fs::read_to_string(&p.menubar_plist).is_ok_and(|s| s == plist(p)) {
        return false;
    }
    fsutil::exists(&p.brew_menubar_bin) || compiled_up_to_date(p)
}

fn compiled_up_to_date(p: &Paths) -> bool {
    if !fsutil::exists(&p.menubar_bin) {
        return false;
    }
    let stamp = path_with_suffix(&p.menubar_bin, ".swift");
    fs::read(&stamp).is_ok_and(|got| got == SWIFT_SOURCE)
}

struct TempCleanup<'a>(&'a Path, &'a Path);

impl Drop for TempCleanup<'_> {
    fn drop(&mut self) {
        let _ = fs::remove_file(self.0);
        let _ = fs::remove_file(self.1);
    }
}

fn ensure_binary(p: &Paths, r: &dyn Runner) -> Result<(), String> {
    if fsutil::exists(&p.brew_menubar_bin) || compiled_up_to_date(p) {
        return Ok(());
    }
    if !fsutil::exists(&p.swiftc) {
        return Err("swiftc is not installed: xcode-select --install".to_string());
    }
    fs::create_dir_all(&p.config_dir)
        .map_err(|e| format!("create {}: {e}", p.config_dir.display()))?;

    let tmp_src = path_with_suffix(&p.menubar_bin, ".tmp.swift");
    let tmp_bin = path_with_suffix(&p.menubar_bin, ".tmp");
    let _cleanup = TempCleanup(&tmp_src, &tmp_bin);

    write_mode(&tmp_src, SWIFT_SOURCE, 0o600)
        .map_err(|e| format!("write {}: {e}", tmp_src.display()))?;
    // Pre-create tmp_bin so atomic rename also succeeds under FakeRunner in tests;
    // real swiftc replaces it with an executable Mach-O binary.
    write_mode(&tmp_bin, &[], 0o700).map_err(|e| format!("create {}: {e}", tmp_bin.display()))?;

    let swiftc = p.swiftc.to_string_lossy();
    let tmp_src_s = tmp_src.to_string_lossy();
    let tmp_bin_s = tmp_bin.to_string_lossy();
    r.run(
        Some(BUILD_TIMEOUT),
        &swiftc,
        &["-O", &tmp_src_s, "-o", &tmp_bin_s],
    )
    .map_err(|e| format!("build menubar: {e}"))?;

    fs::rename(&tmp_bin, &p.menubar_bin).map_err(|e| format!("install menubar: {e}"))?;

    let stamp = path_with_suffix(&p.menubar_bin, ".swift");
    fsutil::write_file(&stamp, SWIFT_SOURCE, 0o644)
        .map_err(|e| format!("record menubar source: {e}"))?;
    Ok(())
}

fn write_mode(path: &Path, data: &[u8], mode: u32) -> std::io::Result<()> {
    let mut f = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(mode)
        .open(path)?;
    f.write_all(data)
}

fn path_with_suffix(base: &Path, suffix: &str) -> PathBuf {
    let mut os = base.as_os_str().to_os_string();
    os.push(suffix);
    PathBuf::from(os)
}

fn xml_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for ch in s.chars() {
        match ch {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&apos;"),
            _ => out.push(ch),
        }
    }
    out
}

fn domain(p: &Paths) -> String {
    format!("gui/{}", p.uid)
}

fn target(p: &Paths) -> String {
    format!("{}/{LABEL}", domain(p))
}

#[cfg(test)]
mod tests {
    use std::process::Command;

    use super::*;
    use crate::run::{FakeRunner, line};

    fn write_file(path: &Path, content: &[u8]) {
        if let Some(dir) = path.parent() {
            fs::create_dir_all(dir).unwrap();
        }
        fs::write(path, content).unwrap();
    }

    #[test]
    fn running_checks_launchctl_pid() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        let r = FakeRunner::new();
        let print_line = line(
            &p.launchctl.to_string_lossy(),
            &["print", "gui/501/io.lodo.menubar"],
        );
        r.fail(&print_line, "Could not find service");

        assert!(!running(&r, &p));

        r.set(&print_line, "\tstate = not running\n");
        assert!(!running(&r, &p));

        r.set(&print_line, "\tstate = running\n\tpid = 999\n");
        assert!(running(&r, &p));
    }

    #[test]
    fn start_and_stop_lifecycle() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        write_file(&p.swiftc, b"");
        let r = FakeRunner::new();
        let launchctl = p.launchctl.to_string_lossy().into_owned();
        let swiftc = p.swiftc.to_string_lossy().into_owned();
        let print_line = line(&launchctl, &["print", "gui/501/io.lodo.menubar"]);
        r.fail(&print_line, "Could not find service");

        // First start compiles via temp files and bootstraps the LaunchAgent.
        start(&p, &r).unwrap();
        let tmp_src = path_with_suffix(&p.menubar_bin, ".tmp.swift");
        let tmp_bin = path_with_suffix(&p.menubar_bin, ".tmp");
        let want_first = vec![
            line(
                &swiftc,
                &[
                    "-O",
                    &tmp_src.to_string_lossy(),
                    "-o",
                    &tmp_bin.to_string_lossy(),
                ],
            ),
            print_line.clone(),
            line(
                &launchctl,
                &["bootstrap", "gui/501", &p.menubar_plist.to_string_lossy()],
            ),
        ];
        assert_eq!(r.calls(), want_first);
        assert!(!tmp_src.exists());
        assert!(!tmp_bin.exists());

        // Second start when already loaded skips swiftc and kickstarts the job.
        let r2 = FakeRunner::new();
        r2.set(&print_line, "\tstate = running\n\tpid = 999\n");
        start(&p, &r2).unwrap();
        let want_second = vec![
            print_line.clone(),
            line(&launchctl, &["kickstart", "-k", "gui/501/io.lodo.menubar"]),
        ];
        assert_eq!(r2.calls(), want_second);

        // Stop unloads the job first, then deletes the plist.
        let r3 = FakeRunner::new();
        r3.set(&print_line, "\tstate = running\n\tpid = 999\n");
        stop(&p, &r3).unwrap();
        let want_stop = vec![
            print_line.clone(),
            line(&launchctl, &["bootout", "gui/501/io.lodo.menubar"]),
        ];
        assert_eq!(r3.calls(), want_stop);
        assert!(!p.menubar_plist.exists());

        // Stop still boots out a loaded job even if the plist was already removed.
        let r4 = FakeRunner::new();
        r4.set(&print_line, "\tstate = running\n\tpid = 999\n");
        stop(&p, &r4).unwrap();
        assert_eq!(r4.calls(), want_stop);
    }

    #[test]
    fn start_uses_prebuilt_brew_binary_without_swiftc() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        // Note: p.swiftc does NOT exist; brew_menubar_bin and lodo_brew_bin exist.
        write_file(&p.brew_menubar_bin, b"mach-o");
        write_file(&p.lodo_brew_bin, b"lodo");
        assert_eq!(active_binary_path(&p), p.brew_menubar_bin);

        let launchctl = p.launchctl.to_string_lossy().into_owned();
        let print_line = line(&launchctl, &["print", "gui/501/io.lodo.menubar"]);
        let r = FakeRunner::new();
        r.fail(&print_line, "Could not find service");

        start(&p, &r).unwrap();
        assert_eq!(
            r.calls(),
            vec![
                print_line,
                line(
                    &launchctl,
                    &["bootstrap", "gui/501", &p.menubar_plist.to_string_lossy()],
                ),
            ]
        );
        let written_plist = fs::read_to_string(&p.menubar_plist).unwrap();
        assert!(written_plist.contains(&p.brew_menubar_bin.display().to_string()));
        assert!(written_plist.contains(&p.lodo_brew_bin.display().to_string()));

        // Refresh is a no-op once plist matches brew_menubar_bin.
        let r_refresh = FakeRunner::new();
        refresh(&p, &r_refresh).unwrap();
        assert!(r_refresh.calls().is_empty());
    }

    #[test]
    fn start_failed_build_rebuilds_next_time() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        write_file(&p.swiftc, b"");
        write_file(&p.menubar_bin, b"old-binary");
        write_file(
            &path_with_suffix(&p.menubar_bin, ".swift"),
            b"// old source\n",
        );

        let tmp_src = path_with_suffix(&p.menubar_bin, ".tmp.swift");
        let tmp_bin = path_with_suffix(&p.menubar_bin, ".tmp");
        let compile_line = line(
            &p.swiftc.to_string_lossy(),
            &[
                "-O",
                &tmp_src.to_string_lossy(),
                "-o",
                &tmp_bin.to_string_lossy(),
            ],
        );

        let r = FakeRunner::new();
        r.fail(&compile_line, "xcode license not accepted");
        let err = start(&p, &r).unwrap_err();
        assert!(err.contains("build menubar"), "err = {err}");
        assert_eq!(fs::read(&p.menubar_bin).unwrap(), b"old-binary");

        // Next Start retries compilation instead of thinking old binary is up to date.
        let r2 = FakeRunner::new();
        let print_line = line(
            &p.launchctl.to_string_lossy(),
            &["print", "gui/501/io.lodo.menubar"],
        );
        r2.fail(&print_line, "Could not find service");
        start(&p, &r2).unwrap();
        assert!(r2.calls().contains(&compile_line));
    }

    #[test]
    fn refresh_skips_disabled_and_up_to_date_and_rebuilds_stale() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        write_file(&p.swiftc, b"");
        let r = FakeRunner::new();

        // Disabled (no plist): no-op.
        refresh(&p, &r).unwrap();
        assert!(r.calls().is_empty());

        // Enabled and up to date: no-op (no flicker).
        write_file(&p.menubar_plist, plist(&p).as_bytes());
        write_file(&p.menubar_bin, b"bin");
        write_file(&path_with_suffix(&p.menubar_bin, ".swift"), source());
        refresh(&p, &r).unwrap();
        assert!(r.calls().is_empty());

        // Enabled with stale source: rebuilds and kickstarts.
        write_file(&path_with_suffix(&p.menubar_bin, ".swift"), b"// stale\n");
        let launchctl = p.launchctl.to_string_lossy().into_owned();
        let print_line = line(&launchctl, &["print", "gui/501/io.lodo.menubar"]);
        r.set(&print_line, "\tstate = running\n\tpid = 999\n");
        refresh(&p, &r).unwrap();

        let tmp_src = path_with_suffix(&p.menubar_bin, ".tmp.swift");
        let tmp_bin = path_with_suffix(&p.menubar_bin, ".tmp");
        let want = vec![
            line(
                &p.swiftc.to_string_lossy(),
                &[
                    "-O",
                    &tmp_src.to_string_lossy(),
                    "-o",
                    &tmp_bin.to_string_lossy(),
                ],
            ),
            print_line,
            line(&launchctl, &["kickstart", "-k", "gui/501/io.lodo.menubar"]),
        ];
        assert_eq!(r.calls(), want);
    }

    #[test]
    fn can_start_checks_prebuilt_compiled_and_swiftc() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        assert!(!can_start(&p));

        write_file(&p.swiftc, b"");
        assert!(can_start(&p));
        fs::remove_file(&p.swiftc).unwrap();
        assert!(!can_start(&p));

        write_file(&p.menubar_bin, b"bin");
        write_file(&path_with_suffix(&p.menubar_bin, ".swift"), source());
        assert!(can_start(&p));
        fs::remove_file(&p.menubar_bin).unwrap();
        assert!(!can_start(&p));

        write_file(&p.brew_menubar_bin, b"mach-o");
        assert!(can_start(&p));
    }

    #[test]
    fn ensure_running_starts_stopped_skips_running_and_rebuilds_stale() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        write_file(&p.swiftc, b"");
        let launchctl = p.launchctl.to_string_lossy().into_owned();
        let print_line = line(&launchctl, &["print", "gui/501/io.lodo.menubar"]);

        // 1. Not installed yet: compiles and bootstraps.
        let r1 = FakeRunner::new();
        r1.fail(&print_line, "Could not find service");
        ensure_running(&p, &r1).unwrap();
        assert!(p.menubar_plist.exists());
        assert!(r1.calls().contains(&line(
            &launchctl,
            &["bootstrap", "gui/501", &p.menubar_plist.to_string_lossy()],
        )));

        // 2. Installed, up to date, and running: only checks launchctl print (no kickstart).
        let r2 = FakeRunner::new();
        r2.set(&print_line, "\tstate = running\n\tpid = 999\n");
        ensure_running(&p, &r2).unwrap();
        assert_eq!(r2.calls(), vec![print_line.clone()]);

        // 3. Installed and up to date, but not running: kickstarts without recompiling.
        let r3 = FakeRunner::new();
        r3.set(&print_line, "\tstate = not running\n");
        ensure_running(&p, &r3).unwrap();
        assert_eq!(
            r3.calls(),
            vec![
                print_line.clone(),
                line(&launchctl, &["kickstart", "-k", "gui/501/io.lodo.menubar"]),
            ]
        );

        // 4. Stale source while running: recompiles and kickstarts.
        write_file(&path_with_suffix(&p.menubar_bin, ".swift"), b"// stale\n");
        let r4 = FakeRunner::new();
        r4.set(&print_line, "\tstate = running\n\tpid = 999\n");
        ensure_running(&p, &r4).unwrap();
        assert!(r4.calls().iter().any(|c| c.contains("swiftc")));
        assert!(r4.calls().contains(&line(
            &launchctl,
            &["kickstart", "-k", "gui/501/io.lodo.menubar"],
        )));

        // 5. Changed plist while already loaded: boots out and bootstraps so launchd reloads it.
        write_file(&p.brew_menubar_bin, b"mach-o");
        let r5 = FakeRunner::new();
        r5.set(&print_line, "\tstate = running\n\tpid = 999\n");
        ensure_running(&p, &r5).unwrap();
        assert_eq!(
            r5.calls(),
            vec![
                print_line,
                line(&launchctl, &["bootout", "gui/501/io.lodo.menubar"]),
                line(
                    &launchctl,
                    &["bootstrap", "gui/501", &p.menubar_plist.to_string_lossy()],
                ),
            ]
        );
    }

    #[test]
    fn ensure_running_serializes_concurrent_startups() {
        use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
        use std::thread;

        use crate::run::RunError;

        struct ConcurrentRunner {
            paths: Paths,
            running: AtomicBool,
            compiles: AtomicUsize,
            bootstraps: AtomicUsize,
        }

        impl Runner for ConcurrentRunner {
            fn run(
                &self,
                _timeout: Option<Duration>,
                name: &str,
                args: &[&str],
            ) -> Result<String, RunError> {
                if Path::new(name) == self.paths.swiftc {
                    self.compiles.fetch_add(1, Ordering::SeqCst);
                    thread::sleep(Duration::from_millis(25));
                    return Ok(String::new());
                }
                if Path::new(name) == self.paths.launchctl {
                    match args.first().copied() {
                        Some("print") => {
                            if self.running.load(Ordering::SeqCst) {
                                return Ok("\tstate = running\n\tpid = 999\n".to_string());
                            }
                            return Err(RunError {
                                cmd: line(name, args),
                                stderr: "Could not find service".into(),
                                stdout: String::new(),
                                timed_out: false,
                                source: std::io::Error::other("not loaded"),
                            });
                        }
                        Some("bootstrap") => {
                            self.bootstraps.fetch_add(1, Ordering::SeqCst);
                            self.running.store(true, Ordering::SeqCst);
                            return Ok(String::new());
                        }
                        _ => {}
                    }
                }
                Ok(String::new())
            }

            fn run_input(
                &self,
                _timeout: Option<Duration>,
                _stdin: &str,
                _name: &str,
                _args: &[&str],
            ) -> Result<String, RunError> {
                Ok(String::new())
            }

            fn run_tty(&self, _name: &str, _args: &[&str]) -> Result<(), RunError> {
                Ok(())
            }

            fn replace(
                &self,
                _dir: &Path,
                _name: &str,
                _args: &[String],
                _env: &[(String, String)],
            ) -> Result<(), RunError> {
                Ok(())
            }
        }

        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        write_file(&p.swiftc, b"");
        let runner = ConcurrentRunner {
            paths: p.clone(),
            running: AtomicBool::new(false),
            compiles: AtomicUsize::new(0),
            bootstraps: AtomicUsize::new(0),
        };

        thread::scope(|s| {
            for _ in 0..3 {
                s.spawn(|| ensure_running(&p, &runner).unwrap());
            }
        });

        assert_eq!(runner.compiles.load(Ordering::SeqCst), 1);
        assert_eq!(runner.bootstraps.load(Ordering::SeqCst), 1);
        assert!(p.menubar_plist.exists());
    }

    #[test]
    fn start_without_swiftc_returns_helpful_error() {
        let dir = tempfile::tempdir().unwrap();
        let p = Paths::for_test(dir.path());
        let r = FakeRunner::new();
        let err = start(&p, &r).unwrap_err();
        assert!(err.contains("swiftc is not installed"), "err = {err}");
    }

    #[test]
    fn plist_escapes_xml_and_passes_plutil() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("a & b <c> \"d\" 'e'");
        let p = Paths::for_test(&root);
        let content = plist(&p);
        assert!(content.contains("<key>SuccessfulExit</key>\n\t\t<false/>"));
        assert!(content.contains("<key>LODO_DOMAINS_JSON</key>"));
        assert!(content.contains("<key>LODO_BIN</key>"));

        if !Path::new("/usr/bin/plutil").exists() {
            return;
        }
        let plist_file = dir.path().join("io.lodo.menubar.plist");
        write_file(&plist_file, content.as_bytes());
        let out = Command::new("/usr/bin/plutil")
            .args(["-lint", &plist_file.to_string_lossy()])
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "plutil -lint failed: {}",
            String::from_utf8_lossy(&out.stdout)
        );
    }

    #[test]
    fn swift_self_tests() {
        let swiftc = Path::new("/usr/bin/swiftc");
        if !swiftc.exists() {
            return;
        }
        let dir = tempfile::tempdir().unwrap();
        let src = dir.path().join("menubar.swift");
        let bin = dir.path().join("lodo-menubar");
        write_file(&src, source());

        let compile = Command::new(swiftc)
            .args([
                src.to_string_lossy().as_ref(),
                "-o",
                bin.to_string_lossy().as_ref(),
            ])
            .output()
            .unwrap();
        assert!(
            compile.status.success(),
            "swiftc compile failed: {}",
            String::from_utf8_lossy(&compile.stderr)
        );

        let test_out = Command::new(&bin).arg("--test").output().unwrap();
        assert!(
            test_out.status.success(),
            "lodo-menubar --test failed: {}",
            String::from_utf8_lossy(&test_out.stderr)
        );

        let dump_out = Command::new(&bin)
            .arg("--dump")
            .env("LODO_DOMAINS_JSON", dir.path().join("missing.json"))
            .output()
            .unwrap();
        assert!(dump_out.status.success());
        assert_eq!(dump_out.stdout, b"No running services\n");
    }
}
