//! Best-effort teardown of a removed worktree's Docker Compose resources and freed own-IP listeners.

use std::collections::HashSet;
use std::sync::atomic::{AtomicU64, Ordering};
use std::thread;
use std::time::{Duration, Instant};

use crate::paths::Paths;
use crate::run::{RunError, Runner};
use crate::store::{self, Domain};
use crate::{check, fsutil};

const DOCKER_LIST_TIMEOUT: Duration = Duration::from_secs(5);
const DOCKER_RM_TIMEOUT: Duration = Duration::from_secs(20);
const LSOF_TIMEOUT: Duration = Duration::from_secs(5);
const KILL_TIMEOUT: Duration = Duration::from_secs(5);

pub(crate) static LISTENER_SETTLE_MS: AtomicU64 = AtomicU64::new(if cfg!(test) { 0 } else { 500 });

/// Project metadata needed to tear down its containers, networks, volumes, and own-IP listeners.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Target {
    pub name: String,
    pub address: String,
    pub compose: String,
    pub worktree: bool,
}

/// Counts of resources cleaned up and any non-fatal warnings to surface on `stderr`.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Outcome {
    pub name: String,
    pub address: String,
    pub containers: usize,
    pub networks: usize,
    pub volumes: usize,
    pub listeners: usize,
    pub notes: Vec<String>,
}

impl Outcome {
    pub fn is_empty(&self) -> bool {
        self.containers == 0 && self.networks == 0 && self.volumes == 0 && self.listeners == 0
    }

    pub fn summary_lines(&self) -> Vec<String> {
        let mut lines = Vec::new();
        let mut parts = Vec::new();
        if self.containers > 0 {
            parts.push(plural(self.containers, "container"));
        }
        if self.networks > 0 {
            parts.push(plural(self.networks, "network"));
        }
        if self.volumes > 0 {
            parts.push(plural(self.volumes, "volume"));
        }
        if !parts.is_empty() {
            let (label, _) = store::cut_project(&self.name);
            lines.push(format!("stopped {label} compose ({})", parts.join(", ")));
        }
        if self.listeners > 0 {
            lines.push(format!(
                "stopped {} on {}",
                plural(self.listeners, "listener"),
                self.address
            ));
        }
        lines
    }
}

/// Tears down Docker Compose resources (worktrees only) and TCP listeners on `target`'s own IP.
///
/// Entries in `remaining` belonging to `target`'s own project are ignored when checking whether
/// another listed project still uses `target`'s compose label or IP address.
pub fn project(p: &Paths, r: &dyn Runner, target: &Target, remaining: &[Domain]) -> Outcome {
    let mut outcome = Outcome {
        name: target.name.clone(),
        address: target.address.clone(),
        ..Outcome::default()
    };
    let target_project = store::project(&target.name);
    let others = || {
        remaining
            .iter()
            .filter(|d| store::project(&d.name) != target_project)
    };

    if target.worktree && target.compose != store::NO_COMPOSE {
        let (label, _) = store::cut_project(&target.name);
        let label_in_use = others().any(|d| store::cut_project(&d.name).0 == label);
        if !label.is_empty() && !label_in_use {
            clean_docker(p, r, &label, &target.compose, &mut outcome);
        }
    }

    let addr_in_use = others().any(|d| d.address == target.address);
    if store::is_own(&target.address) && !addr_in_use {
        clean_listeners(p, r, &target.address, &mut outcome);
    }

    outcome
}

fn clean_docker(p: &Paths, r: &dyn Runner, label: &str, compose: &str, outcome: &mut Outcome) {
    let Some(docker_path) = p.resolve_docker() else {
        return;
    };
    let docker = docker_path.to_string_lossy();
    let filter = format!("label=com.docker.compose.project={label}");

    let ps_out = match r.run(
        Some(DOCKER_LIST_TIMEOUT),
        &docker,
        &["ps", "-aq", "--filter", &filter],
    ) {
        Ok(out) => out,
        Err(err) => {
            if store::has_compose(compose) {
                outcome.notes.push(format!(
                    "{label}: skipped docker cleanup: {}",
                    short_err(&err)
                ));
            }
            return;
        }
    };

    let containers = non_empty_lines(&ps_out);
    if !containers.is_empty() {
        let mut args = Vec::with_capacity(containers.len() + 2);
        args.extend(["rm", "-f"]);
        args.extend(containers.iter().copied());
        match r.run(Some(DOCKER_RM_TIMEOUT), &docker, &args) {
            Ok(_) => outcome.containers = containers.len(),
            Err(err) => outcome
                .notes
                .push(format!("{label}: remove containers: {}", short_err(&err))),
        }
    }

    match r.run(
        Some(DOCKER_LIST_TIMEOUT),
        &docker,
        &["network", "ls", "-q", "--filter", &filter],
    ) {
        Ok(out) => {
            let networks = non_empty_lines(&out);
            if !networks.is_empty() {
                let mut args = Vec::with_capacity(networks.len() + 2);
                args.extend(["network", "rm"]);
                args.extend(networks.iter().copied());
                match r.run(Some(DOCKER_RM_TIMEOUT), &docker, &args) {
                    Ok(_) => outcome.networks = networks.len(),
                    Err(err) => outcome
                        .notes
                        .push(format!("{label}: remove networks: {}", short_err(&err))),
                }
            }
        }
        Err(err) => outcome
            .notes
            .push(format!("{label}: list networks: {}", short_err(&err))),
    }

    match r.run(
        Some(DOCKER_LIST_TIMEOUT),
        &docker,
        &["volume", "ls", "-q", "--filter", &filter],
    ) {
        Ok(out) => {
            let volumes = non_empty_lines(&out);
            if !volumes.is_empty() {
                let mut args = Vec::with_capacity(volumes.len() + 2);
                args.extend(["volume", "rm"]);
                args.extend(volumes.iter().copied());
                match r.run(Some(DOCKER_RM_TIMEOUT), &docker, &args) {
                    Ok(_) => outcome.volumes = volumes.len(),
                    Err(err) => outcome
                        .notes
                        .push(format!("{label}: remove volumes: {}", short_err(&err))),
                }
            }
        }
        Err(err) => outcome
            .notes
            .push(format!("{label}: list volumes: {}", short_err(&err))),
    }
}

fn clean_listeners(p: &Paths, r: &dyn Runner, address: &str, outcome: &mut Outcome) {
    if !fsutil::exists(&p.lsof) || !fsutil::exists(&p.kill) {
        return;
    }
    let lsof = p.lsof.to_string_lossy();
    let self_pid = std::process::id();
    let out = query_listeners(r, &lsof, address);
    let pids = parse_listeners(&out, address, self_pid);
    if pids.is_empty() {
        return;
    }

    let kill = p.kill.to_string_lossy();
    let pid_strs: Vec<String> = pids.iter().map(u32::to_string).collect();
    let mut args = Vec::with_capacity(pid_strs.len() + 1);
    args.push("-TERM");
    args.extend(pid_strs.iter().map(String::as_str));

    match r.run(Some(KILL_TIMEOUT), &kill, &args) {
        Ok(_) => outcome.listeners = pids.len(),
        Err(err) => {
            if !err.stderr.contains("No such process") {
                outcome
                    .notes
                    .push(format!("{address}: stop listeners: {}", short_err(&err)));
            }
            return;
        }
    }

    let settle_ms = LISTENER_SETTLE_MS.load(Ordering::Relaxed);
    if settle_ms > 0 {
        let deadline = Instant::now() + Duration::from_millis(settle_ms);
        while Instant::now() < deadline {
            thread::sleep(Duration::from_millis(50));
            let after = query_listeners(r, &lsof, address);
            if parse_listeners(&after, address, self_pid).is_empty() {
                break;
            }
        }
    }
}

fn query_listeners(r: &dyn Runner, lsof: &str, address: &str) -> String {
    let addr_arg = format!("-iTCP@{address}");
    match r.run(
        Some(LSOF_TIMEOUT),
        lsof,
        &["-nP", "-Fpcn", &addr_arg, "-sTCP:LISTEN"],
    ) {
        Ok(out) => out,
        Err(err) => err.stdout,
    }
}

fn parse_listeners(out: &str, address: &str, self_pid: u32) -> Vec<u32> {
    let prefix = format!("{address}:");
    let mut pids = Vec::new();
    let mut seen = HashSet::new();
    let mut cur_pid: Option<u32> = None;
    let mut cur_cmd: Option<&str> = None;
    let mut matched = false;

    let flush = |pid: Option<u32>,
                 cmd: Option<&str>,
                 matched: bool,
                 seen: &mut HashSet<u32>,
                 pids: &mut Vec<u32>| {
        let (Some(p), Some(c)) = (pid, cmd) else {
            return;
        };
        if matched && p > 1 && p != self_pid && !is_protected_command(c) && seen.insert(p) {
            pids.push(p);
        }
    };

    for line in out.lines() {
        let Some(&tag) = line.as_bytes().first() else {
            continue;
        };
        let val = &line[1..];
        match tag {
            b'p' => {
                flush(cur_pid, cur_cmd, matched, &mut seen, &mut pids);
                cur_pid = val.trim().parse::<u32>().ok();
                cur_cmd = None;
                matched = false;
            }
            b'c' => {
                cur_cmd = Some(val.trim());
            }
            b'n' if val.starts_with(&prefix) && !val.contains("->") => {
                matched = true;
            }
            _ => {}
        }
    }
    flush(cur_pid, cur_cmd, matched, &mut seen, &mut pids);
    pids
}

fn is_protected_command(cmd: &str) -> bool {
    let lower = cmd.trim().to_ascii_lowercase();
    if lower.is_empty() {
        return true;
    }
    if matches!(
        lower.as_str(),
        "caddy" | "dnsmasq" | "lodo" | "lodo-menubar" | "limactl" | "gvproxy" | "colima"
    ) {
        return true;
    }
    lower.starts_with("com.docker.")
        || lower.starts_with("com.apple.")
        || lower.starts_with("vpnkit")
        || lower.starts_with("orbstack")
        || lower.starts_with("docker")
        || lower.starts_with("qemu-system-")
}

fn non_empty_lines(out: &str) -> Vec<&str> {
    out.lines()
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .collect()
}

fn short_err(err: &RunError) -> String {
    if !err.stderr.is_empty() {
        return check::one_line(&err.stderr);
    }
    if err.timed_out {
        return "timed out".to_string();
    }
    check::one_line(&err.source.to_string())
}

fn plural(n: usize, noun: &str) -> String {
    if n == 1 {
        format!("1 {noun}")
    } else {
        format!("{n} {noun}s")
    }
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::fs::PermissionsExt;
    use std::path::Path;

    use super::*;
    use crate::run::{FakeRunner, line};

    fn make_exec(path: &Path) {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(path, b"").unwrap();
        fs::set_permissions(path, fs::Permissions::from_mode(0o755)).unwrap();
    }

    fn setup_tools(p: &Paths) -> (String, String, String) {
        let docker = p.docker_candidates[0].clone();
        make_exec(&docker);
        make_exec(&p.lsof);
        make_exec(&p.kill);
        (
            docker.to_string_lossy().into_owned(),
            p.lsof.to_string_lossy().into_owned(),
            p.kill.to_string_lossy().into_owned(),
        )
    }

    #[test]
    fn worktree_cleans_docker_in_order_and_stops_listeners() {
        let tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(tmp.path());
        let (docker, lsof, kill) = setup_tools(&p);
        let r = FakeRunner::new();

        r.set(
            line(
                &docker,
                &[
                    "ps",
                    "-aq",
                    "--filter",
                    "label=com.docker.compose.project=shop2",
                ],
            ),
            "c1\nc2\n",
        );
        r.set(
            line(
                &docker,
                &[
                    "network",
                    "ls",
                    "-q",
                    "--filter",
                    "label=com.docker.compose.project=shop2",
                ],
            ),
            "net1\n",
        );
        r.set(
            line(
                &docker,
                &[
                    "volume",
                    "ls",
                    "-q",
                    "--filter",
                    "label=com.docker.compose.project=shop2",
                ],
            ),
            "shop2_pgdata\n",
        );
        r.set(
            line(&lsof, &["-nP", "-Fpcn", "-iTCP@127.0.1.2", "-sTCP:LISTEN"]),
            "p4100\ncnode\nf21\nn127.0.1.2:3000\nf22\nn127.0.1.2:3001\np4200\ncnext-server\nf9\nn127.0.1.2:3002\n",
        );

        let target = Target {
            name: "shop2.lodo".into(),
            address: "127.0.1.2".into(),
            compose: "/p/wt/compose.dev.yml".into(),
            worktree: true,
        };
        let out = project(&p, &r, &target, &[]);
        assert_eq!(
            out,
            Outcome {
                name: "shop2.lodo".into(),
                address: "127.0.1.2".into(),
                containers: 2,
                networks: 1,
                volumes: 1,
                listeners: 2,
                notes: vec![],
            }
        );
        assert_eq!(
            out.summary_lines(),
            vec![
                "stopped shop2 compose (2 containers, 1 network, 1 volume)",
                "stopped 2 listeners on 127.0.1.2",
            ]
        );
        assert_eq!(
            r.calls(),
            vec![
                line(
                    &docker,
                    &[
                        "ps",
                        "-aq",
                        "--filter",
                        "label=com.docker.compose.project=shop2"
                    ],
                ),
                line(&docker, &["rm", "-f", "c1", "c2"]),
                line(
                    &docker,
                    &[
                        "network",
                        "ls",
                        "-q",
                        "--filter",
                        "label=com.docker.compose.project=shop2",
                    ],
                ),
                line(&docker, &["network", "rm", "net1"]),
                line(
                    &docker,
                    &[
                        "volume",
                        "ls",
                        "-q",
                        "--filter",
                        "label=com.docker.compose.project=shop2",
                    ],
                ),
                line(&docker, &["volume", "rm", "shop2_pgdata"]),
                line(&lsof, &["-nP", "-Fpcn", "-iTCP@127.0.1.2", "-sTCP:LISTEN"]),
                line(&kill, &["-TERM", "4100", "4200"]),
            ]
        );
    }

    #[test]
    fn parse_listeners_skips_protected_daemons_wrong_ip_and_self() {
        let out = concat!(
            "p1\ncnode\nf3\nn127.0.1.2:3000\n",
            "p999\ncnode\nf3\nn127.0.1.2:3000\n",
            "p1001\nccom.docker.backend\nf10\nn127.0.1.2:5432\n",
            "p1002\ncOrbStack Helper\nf11\nn127.0.1.2:5432\n",
            "p1003\ncvpnkit-bridge\nf12\nn127.0.1.2:5432\n",
            "p1004\nccaddy\nf13\nn127.0.1.2:80\n",
            "p1005\ncdnsmasq\nf14\nn127.0.1.2:53535\n",
            "p1006\ncdocker-proxy\nf15\nn127.0.1.2:6379\n",
            "p1007\nclimactl\nf16\nn127.0.1.2:5432\n",
            "p1008\ncgvproxy\nf17\nn127.0.1.2:5432\n",
            "p1009\nclodo\nf18\nn127.0.1.2:3000\n",
            "p1010\ncnode\nf19\nn127.0.1.20:3000\n",
            "p1011\ncnode\nf20\nn*:3000\n",
            "p1012\ncnode\nf21\nn127.0.1.2:3000->127.0.1.2:54321\n",
            "p1013\nf22\nn127.0.1.2:3000\n",
            "p2001\ncvite\nf23\nn127.0.1.2:5173\n",
            "p2002\ncpython3.14\nf24\nn127.0.1.2:8000\n",
        );
        assert_eq!(parse_listeners(out, "127.0.1.2", 999), vec![2001, 2002]);
    }

    #[test]
    fn non_worktree_stops_own_ip_listeners_without_calling_docker() {
        let tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(tmp.path());
        let (_docker, lsof, kill) = setup_tools(&p);
        let r = FakeRunner::new();

        r.set(
            line(&lsof, &["-nP", "-Fpcn", "-iTCP@127.0.1.3", "-sTCP:LISTEN"]),
            "p5500\ncnode\nf7\nn127.0.1.3:3000\n",
        );

        let target = Target {
            name: "shop.lodo".into(),
            address: "127.0.1.3".into(),
            compose: "/p/shop/compose.dev.yml".into(),
            worktree: false,
        };
        let out = project(&p, &r, &target, &[]);
        assert_eq!(out.listeners, 1);
        assert_eq!(out.containers, 0);
        assert_eq!(out.summary_lines(), vec!["stopped 1 listener on 127.0.1.3"]);
        assert_eq!(
            r.calls(),
            vec![
                line(&lsof, &["-nP", "-Fpcn", "-iTCP@127.0.1.3", "-sTCP:LISTEN"]),
                line(&kill, &["-TERM", "5500"]),
            ]
        );
    }

    #[test]
    fn compose_none_and_shared_ip_or_label_are_skipped() {
        let tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(tmp.path());
        let (_docker, lsof, _kill) = setup_tools(&p);
        let r = FakeRunner::new();

        // compose=none skips docker; shared 127.0.0.1 skips lsof.
        let out = project(
            &p,
            &r,
            &Target {
                name: "shop2.lodo".into(),
                address: "127.0.0.1".into(),
                compose: store::NO_COMPOSE.into(),
                worktree: true,
            },
            &[],
        );
        assert!(out.is_empty());
        assert!(r.calls().is_empty());

        // Another remaining project with same label on another TLD skips docker;
        // another remaining project on same own IP skips lsof.
        let remaining = vec![
            Domain {
                name: "shop2.custom".into(),
                address: "127.0.1.9".into(),
                enabled: true,
                ..Domain::default()
            },
            Domain {
                name: "other.lodo".into(),
                address: "127.0.1.2".into(),
                enabled: true,
                ..Domain::default()
            },
        ];
        let out2 = project(
            &p,
            &r,
            &Target {
                name: "shop2.lodo".into(),
                address: "127.0.1.2".into(),
                compose: "/p/wt/compose.yml".into(),
                worktree: true,
            },
            &remaining,
        );
        assert!(out2.is_empty());
        assert!(r.calls().is_empty());

        // Remaining entries belonging to target's own project do NOT block cleanup.
        let own_only = vec![Domain {
            name: "web.shop2.lodo".into(),
            address: "127.0.1.2".into(),
            port: 3001,
            enabled: true,
            ..Domain::default()
        }];
        project(
            &p,
            &r,
            &Target {
                name: "shop2.lodo".into(),
                address: "127.0.1.2".into(),
                compose: store::NO_COMPOSE.into(),
                worktree: true,
            },
            &own_only,
        );
        assert_eq!(
            r.calls(),
            vec![line(
                &lsof,
                &["-nP", "-Fpcn", "-iTCP@127.0.1.2", "-sTCP:LISTEN"]
            )]
        );
    }

    #[test]
    fn stopped_docker_daemon_bails_early_and_notes_only_with_compose() {
        let tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(tmp.path());
        let (docker, lsof, _kill) = setup_tools(&p);
        let ps_cmd = line(
            &docker,
            &[
                "ps",
                "-aq",
                "--filter",
                "label=com.docker.compose.project=shop2",
            ],
        );

        // With linked compose: emits a note and skips network/volume ls.
        let r1 = FakeRunner::new();
        r1.fail(&ps_cmd, "Cannot connect to the Docker daemon");
        let out1 = project(
            &p,
            &r1,
            &Target {
                name: "shop2.lodo".into(),
                address: "127.0.1.2".into(),
                compose: "/p/wt/compose.dev.yml".into(),
                worktree: true,
            },
            &[],
        );
        assert!(out1.is_empty());
        assert_eq!(
            out1.notes,
            vec!["shop2: skipped docker cleanup: Cannot connect to the Docker daemon"]
        );
        assert_eq!(
            r1.calls(),
            vec![
                ps_cmd.clone(),
                line(&lsof, &["-nP", "-Fpcn", "-iTCP@127.0.1.2", "-sTCP:LISTEN"]),
            ]
        );

        // Without linked compose: stays quiet on daemon error.
        let r2 = FakeRunner::new();
        r2.fail(&ps_cmd, "Cannot connect to the Docker daemon");
        let out2 = project(
            &p,
            &r2,
            &Target {
                name: "shop2.lodo".into(),
                address: "127.0.1.2".into(),
                compose: String::new(),
                worktree: true,
            },
            &[],
        );
        assert!(out2.notes.is_empty());
    }

    #[test]
    fn kill_errors_ignore_no_such_process_and_note_permission_errors() {
        let tmp = tempfile::tempdir().unwrap();
        let p = Paths::for_test(tmp.path());
        let (_docker, lsof, kill) = setup_tools(&p);
        let lsof_cmd = line(&lsof, &["-nP", "-Fpcn", "-iTCP@127.0.1.4", "-sTCP:LISTEN"]);
        let kill_cmd = line(&kill, &["-TERM", "7000"]);

        let r1 = FakeRunner::new();
        r1.set(&lsof_cmd, "p7000\ncnode\nf5\nn127.0.1.4:3000\n");
        r1.fail(&kill_cmd, "kill: 7000: No such process");
        let out1 = project(
            &p,
            &r1,
            &Target {
                name: "app.lodo".into(),
                address: "127.0.1.4".into(),
                compose: String::new(),
                worktree: false,
            },
            &[],
        );
        assert_eq!(out1.listeners, 0);
        assert!(out1.notes.is_empty());

        let r2 = FakeRunner::new();
        r2.set(&lsof_cmd, "p7000\ncnode\nf5\nn127.0.1.4:3000\n");
        r2.fail(&kill_cmd, "kill: 7000: Operation not permitted");
        let out2 = project(
            &p,
            &r2,
            &Target {
                name: "app.lodo".into(),
                address: "127.0.1.4".into(),
                compose: String::new(),
                worktree: false,
            },
            &[],
        );
        assert_eq!(out2.listeners, 0);
        assert_eq!(
            out2.notes,
            vec!["127.0.1.4: stop listeners: kill: 7000: Operation not permitted"]
        );
    }
}
