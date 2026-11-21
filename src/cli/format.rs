//! Text formatters for `doctor`, `apply`, `scan`, `dev`, and `exec`.

use std::fmt::Write as _;
use std::path::Path;

use crate::check::{Check, ProbeResult};
use crate::store::{self, Domain};
use crate::{compose, ls, scan};

/// Renders the `lodo doctor` table: one line per check, plus an indented `fix:` line under each failure.
pub fn format_checks(checks: &[Check]) -> String {
    let width = checks.iter().map(|c| c.name.len()).max().unwrap_or(0);
    let mut b = String::new();
    for c in checks {
        let mark = if c.ok {
            "✓"
        } else if c.skipped {
            "–"
        } else {
            "✗"
        };
        let _ = writeln!(b, "{mark} {} {:<width$}  {}", c.id, c.name, c.detail);
        if !c.ok && !c.skipped && !c.fix.is_empty() {
            let _ = writeln!(b, "    {:width$}  fix: {}", "", c.fix);
        }
    }
    b
}

/// Renders one line per probed domain for `lodo apply`, marking stopped apps with `–`.
pub fn format_results(results: &[ProbeResult]) -> String {
    if results.is_empty() {
        return "no enabled domains\n".to_string();
    }
    let mut name_width = 0;
    let mut target_width = 0;
    let mut targets = Vec::with_capacity(results.len());
    for r in results {
        let mut t = r.address.clone();
        if r.port > 0 {
            let _ = write!(t, " port {}", r.port);
        }
        if r.secure {
            t.push_str(", https");
        }
        name_width = name_width.max(r.name.len());
        target_width = target_width.max(t.len());
        targets.push(t);
    }

    let mut b = String::new();
    for (r, target) in results.iter().zip(&targets) {
        let mark = if r.down {
            "–"
        } else if !r.ok() {
            "✗"
        } else {
            "✓"
        };
        let line = format!(
            "{mark} {:<name_width$}  {:<target_width$}  {}",
            r.name, target, r.detail
        );
        b.push_str(line.trim_end_matches(' '));
        b.push('\n');
    }
    b
}

/// Exit code for `lodo apply` after probing: `1` when any domain failed for more than its app not running.
pub fn probe_code(results: &[ProbeResult]) -> u8 {
    if results.iter().any(|r| !r.ok() && !r.down) {
        1
    } else {
        0
    }
}

/// Renders `lodo scan`'s human-readable output with masked `.env` values.
pub fn format_proposal(p: &scan::Proposal, home: &Path) -> String {
    let mut b = String::new();
    let _ = writeln!(b, "scan of {}", ls::short_path(&p.root, home));
    if p.name.is_empty() {
        let _ = writeln!(b, "its folder's name makes no {} name", store::SUFFIX);
        return b;
    }
    if !p.can_accept() && p.notes.is_empty() && !p.listed {
        b.push_str("found no dev server or compose file\n");
        return b;
    }
    if !p.can_accept() {
        let _ = writeln!(b, "{} is set up: nothing to add or link", p.name);
    }

    let width = p.add.iter().map(|d| d.name.len()).max().unwrap_or(0);
    for d in &p.add {
        if d.port > 0 {
            let _ = writeln!(
                b,
                "add   {:<width$}  {}  port {}",
                d.name, d.address, d.port
            );
        } else {
            let _ = writeln!(b, "add   {:<width$}  {}", d.name, d.address);
        }
    }
    if p.claim {
        let _ = writeln!(b, "save  this folder as {}'s project", p.name);
    }
    if !p.compose.is_empty() {
        let rel = Path::new(&p.compose)
            .strip_prefix(&p.root)
            .map(|r| r.to_string_lossy().into_owned())
            .unwrap_or_else(|_| p.compose.clone());
        let _ = writeln!(b, "link  ./{}", rel.replace('\\', "/"));
    }
    b.push_str(&format_vars(p));
    if p.can_accept() {
        b.push_str("\nrun lodo dev here to add and link these\n");
    }
    if !p.address.is_empty() {
        b.push_str("\nlodo dev and lodo exec set the env lines; the .env files stay as they are\n");
    }
    b
}

/// Renders the `env` and `note` lines `lodo dev` and `lodo exec` print, keeping credentials masked.
pub fn format_vars(proposal: &scan::Proposal) -> String {
    let mut b = String::new();
    for (k, v) in compose_vars(proposal) {
        let _ = writeln!(b, "env   {k}={v}");
    }
    for v in &proposal.env {
        let _ = writeln!(b, "env   {}={}", v.key, v.value);
    }
    for n in &proposal.notes {
        let _ = writeln!(b, "note  {n}");
    }
    b
}

pub(super) fn compose_vars(proposal: &scan::Proposal) -> Vec<(String, String)> {
    let mut vars = Vec::new();
    if !proposal.address.is_empty() {
        vars.push((compose::ENV_VAR.to_string(), proposal.address.clone()));
    }
    if proposal.worktree && !proposal.name.is_empty() {
        let (label, _) = store::cut_project(&proposal.name);
        vars.push((compose::PROJECT_VAR.to_string(), label));
    }
    vars
}

/// Renders one line per active site Caddy serves for `project`.
pub fn format_sites(domains: &[Domain], project: &str) -> String {
    let sites: Vec<&Domain> = domains
        .iter()
        .filter(|d| store::project(&d.name) == project && d.has_site())
        .collect();
    let name_width = sites.iter().map(|d| d.name.len()).max().unwrap_or(0);
    let url_width = sites.iter().map(|d| site_url(d).len()).max().unwrap_or(0);

    let mut b = String::new();
    for d in sites {
        let url = site_url(d);
        let _ = writeln!(
            b,
            "{:<name_width$}  {:<url_width$}  -> {}:{}",
            d.name, url, d.address, d.port
        );
    }
    b
}

fn site_url(d: &Domain) -> String {
    if d.https {
        format!("https://{}", d.name)
    } else {
        format!("http://{}", d.name)
    }
}
