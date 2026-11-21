//! Subprocess execution abstraction: `ExecRunner` runs real processes with
//! deadlines and `execvp`; `FakeRunner` records and stubs calls in tests.

use std::collections::HashMap;
use std::fmt;
use std::io::{self, Read, Write};
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::Mutex;
use std::thread;
use std::time::{Duration, Instant};

pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30);

/// A command that failed to start, exited non-zero, or exceeded its deadline.
#[derive(Debug)]
pub struct RunError {
    pub cmd: String,
    pub stderr: String,
    pub stdout: String,
    pub timed_out: bool,
    pub source: io::Error,
}

impl fmt::Display for RunError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if !self.stderr.is_empty() {
            write!(f, "{}: {}", self.cmd, self.stderr)
        } else if self.timed_out {
            write!(f, "{}: context deadline exceeded", self.cmd)
        } else {
            write!(f, "{}: {}", self.cmd, self.source)
        }
    }
}

impl std::error::Error for RunError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.source)
    }
}

/// Joins a binary path and its arguments for error messages and `FakeRunner` keys.
pub fn line(name: &str, args: &[&str]) -> String {
    let mut out = String::from(name);
    for arg in args {
        out.push(' ');
        out.push_str(arg);
    }
    out
}

/// External command runner.
pub trait Runner: Send + Sync {
    /// Runs `name` with `args` and returns stdout.
    fn run(&self, timeout: Option<Duration>, name: &str, args: &[&str])
    -> Result<String, RunError>;

    /// Runs `name` with `stdin` piped to standard input and returns stdout.
    fn run_input(
        &self,
        timeout: Option<Duration>,
        stdin: &str,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError>;

    /// Runs `name` attached directly to the terminal (no timeout, for `sudo` prompts).
    fn run_tty(&self, name: &str, args: &[&str]) -> Result<(), RunError>;

    /// Replaces the current process in `dir` with `name`, `args`, and `env` (`execvp`).
    fn replace(
        &self,
        dir: &Path,
        name: &str,
        args: &[String],
        env: &[(String, String)],
    ) -> Result<(), RunError>;
}

/// Runs real OS commands.
#[derive(Debug, Clone, Copy)]
pub struct ExecRunner {
    pub timeout: Duration,
}

impl Default for ExecRunner {
    fn default() -> Self {
        Self {
            timeout: DEFAULT_TIMEOUT,
        }
    }
}

impl ExecRunner {
    fn execute(
        &self,
        timeout: Option<Duration>,
        stdin_data: Option<&str>,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError> {
        let cmd_line = line(name, args);
        let limit = match timeout {
            Some(d) if !d.is_zero() => d,
            _ if !self.timeout.is_zero() => self.timeout,
            _ => DEFAULT_TIMEOUT,
        };

        let mut command = Command::new(name);
        command
            .args(args)
            .stdin(if stdin_data.is_some() {
                Stdio::piped()
            } else {
                Stdio::null()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());

        let mut child = command.spawn().map_err(|e| RunError {
            cmd: cmd_line.clone(),
            stderr: String::new(),
            stdout: String::new(),
            timed_out: false,
            source: e,
        })?;

        let child_stdin = child.stdin.take();
        let child_stdout = child.stdout.take();
        let child_stderr = child.stderr.take();

        thread::scope(|s| {
            if let (Some(mut pipe), Some(input)) = (child_stdin, stdin_data) {
                let bytes = input.as_bytes();
                s.spawn(move || {
                    let _ = pipe.write_all(bytes);
                });
            }

            let out_handle = s.spawn(move || {
                let mut buf = Vec::new();
                if let Some(mut pipe) = child_stdout {
                    let _ = pipe.read_to_end(&mut buf);
                }
                String::from_utf8_lossy(&buf).into_owned()
            });

            let err_handle = s.spawn(move || {
                let mut buf = Vec::new();
                if let Some(mut pipe) = child_stderr {
                    let _ = pipe.read_to_end(&mut buf);
                }
                String::from_utf8_lossy(&buf).trim().to_string()
            });

            let start = Instant::now();
            let mut timed_out = false;
            let status = loop {
                match child.try_wait() {
                    Ok(Some(st)) => break Ok(st),
                    Ok(None) => {
                        if start.elapsed() >= limit {
                            timed_out = true;
                            let _ = child.kill();
                            break child.wait();
                        }
                        thread::sleep(Duration::from_millis(5));
                    }
                    Err(e) => break Err(e),
                }
            };

            let stdout = out_handle.join().unwrap_or_default();
            let stderr = err_handle.join().unwrap_or_default();

            match status {
                Ok(st) if st.success() && !timed_out => Ok(stdout),
                Ok(st) => {
                    let source = if timed_out {
                        io::Error::new(io::ErrorKind::TimedOut, "context deadline exceeded")
                    } else {
                        io::Error::other(format!("exit status {}", st.code().unwrap_or(1)))
                    };
                    Err(RunError {
                        cmd: cmd_line,
                        stderr: if timed_out { String::new() } else { stderr },
                        stdout,
                        timed_out,
                        source,
                    })
                }
                Err(e) => Err(RunError {
                    cmd: cmd_line,
                    stderr,
                    stdout,
                    timed_out,
                    source: e,
                }),
            }
        })
    }
}

impl Runner for ExecRunner {
    fn run(
        &self,
        timeout: Option<Duration>,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError> {
        self.execute(timeout, None, name, args)
    }

    fn run_input(
        &self,
        timeout: Option<Duration>,
        stdin: &str,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError> {
        self.execute(timeout, Some(stdin), name, args)
    }

    fn run_tty(&self, name: &str, args: &[&str]) -> Result<(), RunError> {
        let cmd_line = line(name, args);
        let status = Command::new(name)
            .args(args)
            .stdin(Stdio::inherit())
            .stdout(Stdio::inherit())
            .stderr(Stdio::inherit())
            .status()
            .map_err(|e| RunError {
                cmd: cmd_line.clone(),
                stderr: String::new(),
                stdout: String::new(),
                timed_out: false,
                source: e,
            })?;

        if status.success() {
            Ok(())
        } else {
            Err(RunError {
                cmd: cmd_line,
                stderr: String::new(),
                stdout: String::new(),
                timed_out: false,
                source: io::Error::other(format!("exit status {}", status.code().unwrap_or(1))),
            })
        }
    }

    fn replace(
        &self,
        dir: &Path,
        name: &str,
        args: &[String],
        env: &[(String, String)],
    ) -> Result<(), RunError> {
        let arg_refs: Vec<&str> = args.iter().map(String::as_str).collect();
        let cmd_line = line(name, &arg_refs);
        std::env::set_current_dir(dir).map_err(|e| RunError {
            cmd: cmd_line.clone(),
            stderr: String::new(),
            stdout: String::new(),
            timed_out: false,
            source: e,
        })?;
        let err = Command::new(name)
            .args(args)
            .env_clear()
            .envs(env.iter().cloned())
            .exec();
        Err(RunError {
            cmd: cmd_line,
            stderr: String::new(),
            stdout: String::new(),
            timed_out: false,
            source: err,
        })
    }
}

#[derive(Debug, Clone)]
enum FakeAnswer {
    Ok(String),
    Err(String),
}

#[derive(Debug, Default)]
struct FakeInner {
    answers: HashMap<String, FakeAnswer>,
    calls: Vec<String>,
    inputs: HashMap<String, Vec<String>>,
    replaced_dir: Option<PathBuf>,
    replaced_env: Vec<(String, String)>,
}

/// Thread-safe fake runner for unit and hermetic integration tests.
#[derive(Debug, Default)]
pub struct FakeRunner {
    inner: Mutex<FakeInner>,
}

impl FakeRunner {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn set(&self, cmd_line: impl Into<String>, out: impl Into<String>) {
        if let Ok(mut g) = self.inner.lock() {
            g.answers
                .insert(cmd_line.into(), FakeAnswer::Ok(out.into()));
        }
    }

    pub fn fail(&self, cmd_line: impl Into<String>, stderr: impl Into<String>) {
        if let Ok(mut g) = self.inner.lock() {
            g.answers
                .insert(cmd_line.into(), FakeAnswer::Err(stderr.into()));
        }
    }

    pub fn calls(&self) -> Vec<String> {
        self.inner
            .lock()
            .map(|g| g.calls.clone())
            .unwrap_or_default()
    }

    pub fn inputs(&self, cmd_line: &str) -> Vec<String> {
        self.inner
            .lock()
            .ok()
            .and_then(|g| g.inputs.get(cmd_line).cloned())
            .unwrap_or_default()
    }

    pub fn replaced(&self) -> (Option<PathBuf>, Vec<(String, String)>) {
        self.inner
            .lock()
            .map(|g| (g.replaced_dir.clone(), g.replaced_env.clone()))
            .unwrap_or_default()
    }

    fn record_and_answer(&self, cmd_line: String) -> Result<String, RunError> {
        let Ok(mut g) = self.inner.lock() else {
            return Ok(String::new());
        };
        g.calls.push(cmd_line.clone());
        match g.answers.get(&cmd_line).cloned() {
            Some(FakeAnswer::Ok(out)) => Ok(out),
            Some(FakeAnswer::Err(stderr)) => Err(RunError {
                cmd: cmd_line,
                stderr,
                stdout: String::new(),
                timed_out: false,
                source: io::Error::other("exit status 1"),
            }),
            None => Ok(String::new()),
        }
    }
}

impl Runner for FakeRunner {
    fn run(
        &self,
        _timeout: Option<Duration>,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError> {
        self.record_and_answer(line(name, args))
    }

    fn run_input(
        &self,
        _timeout: Option<Duration>,
        stdin: &str,
        name: &str,
        args: &[&str],
    ) -> Result<String, RunError> {
        let cmd_line = line(name, args);
        if let Ok(mut g) = self.inner.lock() {
            g.inputs
                .entry(cmd_line.clone())
                .or_default()
                .push(stdin.to_string());
        }
        self.record_and_answer(cmd_line)
    }

    fn run_tty(&self, name: &str, args: &[&str]) -> Result<(), RunError> {
        self.record_and_answer(line(name, args)).map(|_| ())
    }

    fn replace(
        &self,
        dir: &Path,
        name: &str,
        args: &[String],
        env: &[(String, String)],
    ) -> Result<(), RunError> {
        if let Ok(mut g) = self.inner.lock() {
            g.replaced_dir = Some(dir.to_path_buf());
            g.replaced_env = env.to_vec();
        }
        let arg_refs: Vec<&str> = args.iter().map(String::as_str).collect();
        self.record_and_answer(line(name, &arg_refs)).map(|_| ())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exec_runner_captures_stdout_stderr_and_timeouts() {
        let r = ExecRunner::default();
        let out = r
            .run(None, "/bin/sh", &["-c", "echo out; echo noise >&2"])
            .unwrap();
        assert_eq!(out, "out\n");

        let err = r
            .run(
                None,
                "/bin/sh",
                &["-c", "echo partial; echo boom >&2; exit 3"],
            )
            .unwrap_err();
        assert_eq!(err.stdout, "partial\n");
        assert_eq!(err.stderr, "boom");
        assert_eq!(
            err.to_string(),
            "/bin/sh -c echo partial; echo boom >&2; exit 3: boom"
        );

        let missing = r.run(None, "/nonexistent/tool", &[]).unwrap_err();
        assert!(missing.to_string().starts_with("/nonexistent/tool:"));

        let timeout_err = ExecRunner {
            timeout: Duration::from_millis(40),
        }
        .run(None, "/bin/sleep", &["5"])
        .unwrap_err();
        assert!(timeout_err.timed_out);
        assert_eq!(
            timeout_err.to_string(),
            "/bin/sleep 5: context deadline exceeded"
        );
    }

    #[test]
    fn exec_runner_pipes_stdin() {
        let out = ExecRunner::default()
            .run_input(None, "a\nb\n", "/usr/bin/wc", &["-l"])
            .unwrap();
        assert_eq!(out.trim(), "2");
    }

    #[test]
    fn fake_runner_records_calls_inputs_and_replace() {
        let f = FakeRunner::new();
        f.set("brew services info dnsmasq --json", r#"[{"running":true}]"#);
        f.fail("sudo -n script", "sudo: a password is required");

        assert_eq!(
            f.run(None, "brew", &["services", "info", "dnsmasq", "--json"])
                .unwrap(),
            r#"[{"running":true}]"#
        );
        let err = f.run(None, "sudo", &["-n", "script"]).unwrap_err();
        assert_eq!(err.stderr, "sudo: a password is required");
        assert_eq!(f.run(None, "unknown", &[]).unwrap(), "");
        f.run_tty("sudo", &["-v"]).unwrap();
        assert!(
            f.run_input(None, "a.lodo\n", "sudo", &["-n", "script"])
                .is_err()
        );
        let _ = f.run_input(None, "", "sudo", &["-n", "script"]);

        f.replace(
            Path::new("/p/flowy"),
            "/bin/pnpm",
            &["run".into(), "dev".into()],
            &[("NODE_OPTIONS".into(), "x".into())],
        )
        .unwrap();
        let (dir, env) = f.replaced();
        assert_eq!(dir.as_deref(), Some(Path::new("/p/flowy")));
        assert_eq!(env, vec![("NODE_OPTIONS".into(), "x".into())]);
        assert_eq!(
            f.inputs("sudo -n script"),
            vec!["a.lodo\n".to_string(), String::new()]
        );
        assert_eq!(f.calls().len(), 7);
    }
}
