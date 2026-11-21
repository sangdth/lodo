//! Homebrew `launchd` service inspection, kickstart, and stop.

use std::sync::LazyLock;

use regex::Regex;

use crate::fsutil;
use crate::paths::Paths;
use crate::run::Runner;

/// State of a Homebrew service as `brew services info` reports it.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Status {
    /// Whether the `launchd` job currently has a running process.
    pub running: bool,
    /// User the job runs as; empty when no plist registers it.
    pub user: String,
    /// Process ID when running, or `0` when not running.
    pub pid: u32,
    /// Whether a plist registers the job to start at login.
    pub registered: bool,
}

impl Status {
    /// Reports whether the service was turned off (stopped and unregistered).
    pub fn off(&self) -> bool {
        !self.running && !self.registered
    }
}

#[expect(clippy::expect_used)] // compile-time valid static regex
static PID_LINE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"pid = ([0-9]+)").expect("valid PID_LINE"));

fn labels(service: &str) -> [String; 2] {
    [
        format!("sh.brew.{service}"),
        format!("homebrew.mxcl.{service}"),
    ]
}

fn target(p: &Paths, label: &str) -> String {
    format!("gui/{}/{label}", p.uid)
}

struct Job {
    label: String,
    status: Status,
    loaded: bool,
}

fn find(r: &dyn Runner, p: &Paths, service: &str) -> Job {
    let candidates = labels(service);
    let mut job = Job {
        label: candidates[0].clone(),
        status: Status::default(),
        loaded: false,
    };

    for label in &candidates {
        if fsutil::exists(&p.launch_agents.join(format!("{label}.plist"))) {
            job.label.clone_from(label);
            job.status.registered = true;
            job.status.user.clone_from(&p.user);
            break;
        }
    }
    if !job.status.registered {
        for label in &candidates {
            if fsutil::exists(&p.launch_daemons.join(format!("{label}.plist"))) {
                job.label.clone_from(label);
                job.status.registered = true;
                job.status.user = String::from("root");
                break;
            }
        }
    }

    let domain_target = if job.status.user == "root" {
        format!("system/{}", job.label)
    } else {
        target(p, &job.label)
    };
    let launchctl = p.launchctl.to_string_lossy();
    let Ok(out) = r.run(None, &launchctl, &["print", &domain_target]) else {
        return job;
    };

    job.loaded = true;
    if let Some(caps) = PID_LINE.captures(&out)
        && let Some(m) = caps.get(1)
    {
        job.status.pid = m.as_str().parse::<u32>().unwrap_or(0);
        job.status.running = true;
    }
    job
}

/// Returns the current `launchd` and registration state of `service`.
pub fn info(r: &dyn Runner, p: &Paths, service: &str) -> Status {
    find(r, p, service).status
}

/// Stops `service` if running, starts it, and registers it to start at login.
///
/// Uses `launchctl kickstart -k` when the job is already loaded in the current user's
/// `launchd` domain, and falls back to `brew services restart` otherwise.
pub fn restart(r: &dyn Runner, p: &Paths, service: &str) -> Result<(), String> {
    let job = find(r, p, service);
    if job.loaded && job.status.user == p.user {
        let launchctl = p.launchctl.to_string_lossy();
        let tgt = target(p, &job.label);
        r.run(None, &launchctl, &["kickstart", "-k", &tgt])
            .map_err(|e| format!("restart {service}: {e}"))?;
        return Ok(());
    }
    let brew = p.brew.to_string_lossy();
    r.run(None, &brew, &["services", "restart", service])
        .map_err(|e| format!("restart {service}: {e}"))?;
    Ok(())
}

/// Stops `service` and unregisters it so it does not start at login.
pub fn stop(r: &dyn Runner, p: &Paths, service: &str) -> Result<(), String> {
    let brew = p.brew.to_string_lossy();
    r.run(None, &brew, &["services", "stop", service])
        .map_err(|e| format!("stop {service}: {e}"))?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::path::Path;

    use super::*;
    use crate::run::{self, FakeRunner};

    const FAILURE: &str = "Error: Failure while executing; `/bin/launchctl bootstrap gui/501 /Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist` exited with 5.";
    const NOT_LOADED: &str =
        "Could not find service \"sh.brew.dnsmasq\" in domain for user gui: 501";

    const PRINT_RUNNING: &str = include_str!("../tests/testdata/brew/print-running.txt");
    const PRINT_STOPPED: &str = include_str!("../tests/testdata/brew/print-stopped.txt");

    fn touch(path: &Path) {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(path, b"").unwrap();
    }

    #[test]
    fn status_off_distinguishes_stopped_from_crashed() {
        let cases = [
            (
                "running",
                Status {
                    running: true,
                    registered: true,
                    ..Status::default()
                },
                false,
            ),
            (
                "crashed or unloaded by hand: still registered",
                Status {
                    registered: true,
                    ..Status::default()
                },
                false,
            ),
            ("stopped by brew services stop", Status::default(), true),
        ];
        for (name, st, want) in cases {
            assert_eq!(st.off(), want, "case {name}");
        }
    }

    #[test]
    fn info_inspects_plists_and_launchctl_print() {
        struct Case<'a> {
            name: &'a str,
            plist: &'a str,
            print_out: Option<&'a str>,
            label: &'a str,
            want: Status,
        }

        let cases = [
            Case {
                name: "running, captured",
                plist: "Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist",
                print_out: Some(PRINT_RUNNING),
                label: "sh.brew.dnsmasq",
                want: Status {
                    running: true,
                    user: "tester".into(),
                    pid: 63372,
                    registered: true,
                },
            },
            Case {
                name: "loaded but not running",
                plist: "Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist",
                print_out: Some(PRINT_STOPPED),
                label: "sh.brew.dnsmasq",
                want: Status {
                    user: "tester".into(),
                    registered: true,
                    ..Status::default()
                },
            },
            Case {
                name: "registered under the old label",
                plist: "Users/tester/Library/LaunchAgents/homebrew.mxcl.dnsmasq.plist",
                print_out: Some(PRINT_RUNNING),
                label: "homebrew.mxcl.dnsmasq",
                want: Status {
                    running: true,
                    user: "tester".into(),
                    pid: 63372,
                    registered: true,
                },
            },
            Case {
                name: "registered, not loaded",
                plist: "Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist",
                print_out: None,
                label: "sh.brew.dnsmasq",
                want: Status {
                    user: "tester".into(),
                    registered: true,
                    ..Status::default()
                },
            },
            Case {
                name: "a root plist",
                plist: "Library/LaunchDaemons/sh.brew.dnsmasq.plist",
                print_out: None,
                label: "sh.brew.dnsmasq",
                want: Status {
                    user: "root".into(),
                    registered: true,
                    ..Status::default()
                },
            },
            Case {
                name: "a running root plist",
                plist: "Library/LaunchDaemons/sh.brew.dnsmasq.plist",
                print_out: Some(PRINT_RUNNING),
                label: "sh.brew.dnsmasq",
                want: Status {
                    running: true,
                    user: "root".into(),
                    pid: 63372,
                    registered: true,
                },
            },
            Case {
                name: "turned off",
                plist: "",
                print_out: None,
                label: "sh.brew.dnsmasq",
                want: Status::default(),
            },
        ];

        for tc in cases {
            let root = tempfile::tempdir().unwrap();
            let p = Paths::for_test(root.path());
            if !tc.plist.is_empty() {
                touch(&root.path().join(tc.plist));
            }
            let domain = if tc.plist.starts_with("Library/LaunchDaemons/") {
                "system/"
            } else {
                "gui/501/"
            };
            let target_arg = format!("{domain}{}", tc.label);
            let line = run::line(&p.launchctl.to_string_lossy(), &["print", &target_arg]);
            let f = FakeRunner::new();
            match tc.print_out {
                Some(out) => f.set(&line, out),
                None => f.fail(&line, NOT_LOADED),
            }

            let got = info(&f, &p, "dnsmasq");
            assert_eq!(f.calls(), vec![line], "case {}", tc.name);
            assert_eq!(got, tc.want, "case {}", tc.name);
        }
    }

    #[test]
    fn restart_uses_kickstart_when_loaded_as_user_else_brew() {
        struct Case<'a> {
            name: &'a str,
            plist: Option<&'a str>,
            loaded: bool,
            stderr: &'a str,
            want_kickstart: bool,
        }

        let cases = [
            Case {
                name: "loaded: kickstart",
                plist: Some("Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist"),
                loaded: true,
                stderr: "",
                want_kickstart: true,
            },
            Case {
                name: "kickstart fails",
                plist: Some("Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist"),
                loaded: true,
                stderr: "Boot-out failed",
                want_kickstart: true,
            },
            Case {
                name: "registered, not loaded: brew",
                plist: Some("Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist"),
                loaded: false,
                stderr: "",
                want_kickstart: false,
            },
            Case {
                name: "root plist loaded: brew restarts as user",
                plist: Some("Library/LaunchDaemons/sh.brew.dnsmasq.plist"),
                loaded: true,
                stderr: "",
                want_kickstart: false,
            },
            Case {
                name: "turned off: brew registers it",
                plist: None,
                loaded: false,
                stderr: "",
                want_kickstart: false,
            },
            Case {
                name: "brew fails",
                plist: None,
                loaded: false,
                stderr: FAILURE,
                want_kickstart: false,
            },
        ];

        for tc in cases {
            let root = tempfile::tempdir().unwrap();
            let p = Paths::for_test(root.path());
            if let Some(rel) = tc.plist {
                touch(&root.path().join(rel));
            }
            let print_target = if tc
                .plist
                .is_some_and(|rel| rel.starts_with("Library/LaunchDaemons/"))
            {
                "system/sh.brew.dnsmasq"
            } else {
                "gui/501/sh.brew.dnsmasq"
            };
            let print_line = run::line(&p.launchctl.to_string_lossy(), &["print", print_target]);
            let restart_line = if tc.want_kickstart {
                run::line(
                    &p.launchctl.to_string_lossy(),
                    &["kickstart", "-k", "gui/501/sh.brew.dnsmasq"],
                )
            } else {
                run::line(
                    &p.brew.to_string_lossy(),
                    &["services", "restart", "dnsmasq"],
                )
            };

            let f = FakeRunner::new();
            if !tc.loaded {
                f.fail(&print_line, NOT_LOADED);
            }
            if !tc.stderr.is_empty() {
                f.fail(&restart_line, tc.stderr);
            }

            let res = restart(&f, &p, "dnsmasq");
            assert_eq!(
                f.calls(),
                vec![print_line, restart_line.clone()],
                "case {}",
                tc.name
            );
            if tc.stderr.is_empty() {
                assert_eq!(res, Ok(()), "case {}", tc.name);
            } else {
                assert_eq!(
                    res,
                    Err(format!("restart dnsmasq: {restart_line}: {}", tc.stderr)),
                    "case {}",
                    tc.name
                );
            }
        }
    }

    #[test]
    fn stop_calls_brew_services_stop() {
        for stderr in ["", FAILURE] {
            let root = tempfile::tempdir().unwrap();
            let p = Paths::for_test(root.path());
            let line = run::line(&p.brew.to_string_lossy(), &["services", "stop", "caddy"]);
            let f = FakeRunner::new();
            if !stderr.is_empty() {
                f.fail(&line, stderr);
            }

            let res = stop(&f, &p, "caddy");
            assert_eq!(f.calls(), vec![line.clone()]);
            if stderr.is_empty() {
                assert_eq!(res, Ok(()));
            } else {
                assert_eq!(res, Err(format!("stop caddy: {line}: {stderr}")));
            }
        }
    }
}
