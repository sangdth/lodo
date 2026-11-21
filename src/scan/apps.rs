//! Monorepo workspace and app folder discovery.

use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};

use regex::Regex;
use saphyr::{LoadableYamlNode, SafelyIndex, Yaml};

use crate::compose::{clean_path, is_local_path};
use crate::fsutil;

use super::devserver::{dev_server, read_package};

/// Maximum number of apps `find` collects in a single project.
const MAX_APPS: usize = 50;

/// A project directory whose `package.json` `dev` script starts a dev server.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct App {
    /// Slash-separated path relative to the project root (`"."` for the root itself).
    pub dir: String,
    /// Detected port, or `0` when `lodo` cannot determine it.
    pub port: u16,
}

/// Discovers the apps inside `root`: in a single-package repo any `dev` script qualifies;
/// in a workspace, packages running a known dev server or packages under `apps/` with any `dev` script qualify.
pub(crate) fn find_apps(root: &Path) -> Vec<App> {
    let dirs = workspaces(root);
    let mut apps = Vec::new();

    let (srv, known, has_dev) = dev_server(root);
    if known || (has_dev && dirs.is_empty()) {
        apps.push(App {
            dir: ".".to_string(),
            port: srv.port,
        });
    }

    for dir in dirs {
        let (srv, known, has_dev) = dev_server(&root.join(&dir));
        if known || (has_dev && dir.starts_with("apps/")) {
            apps.push(App {
                dir,
                port: srv.port,
            });
        }
        if apps.len() == MAX_APPS {
            break;
        }
    }

    apps
}

fn workspaces(root: &Path) -> Vec<String> {
    let mut patterns = Vec::new();

    if let Ok(data) = fsutil::read_regular(&root.join("pnpm-workspace.yaml"))
        && let Ok(text) = std::str::from_utf8(&data)
        && let Ok(docs) = Yaml::load_from_str(text)
        && let Some(doc) = docs.first()
        && let Some(pkgs) = doc.get("packages").and_then(Yaml::as_vec)
    {
        for item in pkgs {
            if let Some(s) = item.as_str() {
                patterns.push(s.to_string());
            }
        }
    }

    if let Some(pkg) = read_package(root)
        && let Some(ws) = pkg.workspaces
    {
        if let Some(arr) = ws.as_array() {
            for item in arr {
                if let Some(s) = item.as_str() {
                    patterns.push(s.to_string());
                }
            }
        } else if let Some(arr) = ws.get("packages").and_then(serde_json::Value::as_array) {
            for item in arr {
                if let Some(s) = item.as_str() {
                    patterns.push(s.to_string());
                }
            }
        }
    }

    let mut found = BTreeSet::new();
    for raw in patterns {
        let trimmed = raw.trim();
        let trimmed = trimmed.strip_prefix("./").unwrap_or(trimmed);
        if trimmed.is_empty() || trimmed.starts_with('!') {
            continue;
        }
        let normalized = trimmed.replace("**", "*");
        let normalized = normalized.trim_matches('/');
        if normalized.is_empty() {
            continue;
        }

        let cleaned = clean_path(Path::new(normalized));
        if !is_local_path(&cleaned) {
            continue;
        }

        let segments: Vec<&str> = cleaned
            .iter()
            .filter_map(|os| os.to_str())
            .filter(|s| !s.is_empty() && *s != ".")
            .collect();
        if segments.is_empty() || segments.contains(&"node_modules") {
            continue;
        }

        let mut matches = Vec::new();
        expand_glob(root, &segments, 0, &mut Vec::new(), &mut matches);
        for rel in matches {
            let full = root.join(&rel);
            if fs::metadata(full.join("package.json")).is_ok_and(|info| info.file_type().is_file())
            {
                found.insert(rel);
            }
        }
    }

    found.into_iter().collect()
}

fn expand_glob(
    dir: &Path,
    segments: &[&str],
    idx: usize,
    rel_parts: &mut Vec<String>,
    out: &mut Vec<String>,
) {
    if idx == segments.len() {
        out.push(rel_parts.join("/"));
        return;
    }
    let seg = segments[idx];
    if !has_glob_meta(seg) {
        if seg == "node_modules" {
            return;
        }
        let next = dir.join(seg);
        if fs::metadata(&next).is_ok() {
            rel_parts.push(seg.to_string());
            expand_glob(&next, segments, idx + 1, rel_parts, out);
            rel_parts.pop();
        }
        return;
    }

    let Some(re) = glob_segment_regex(seg) else {
        return;
    };
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    let mut children: Vec<(String, PathBuf)> = entries
        .filter_map(Result::ok)
        .filter_map(|e| {
            let name = e.file_name().into_string().ok()?;
            if name == "node_modules" || !re.is_match(&name) {
                return None;
            }
            Some((name, e.path()))
        })
        .collect();
    children.sort_by(|a, b| a.0.cmp(&b.0));

    for (name, path) in children {
        rel_parts.push(name);
        expand_glob(&path, segments, idx + 1, rel_parts, out);
        rel_parts.pop();
    }
}

fn has_glob_meta(s: &str) -> bool {
    s.contains(['*', '?', '[', '\\'])
}

fn glob_segment_regex(seg: &str) -> Option<Regex> {
    let mut pat = String::from("^");
    let chars: Vec<char> = seg.chars().collect();
    let mut i = 0;
    while i < chars.len() {
        match chars[i] {
            '*' => pat.push_str(".*"),
            '?' => pat.push('.'),
            '\\' => {
                i += 1;
                let c = *chars.get(i)?;
                pat.push_str(&regex::escape(&c.to_string()));
            }
            '[' => {
                i += 1;
                let mut cls = String::from("[");
                if matches!(chars.get(i), Some('^' | '!')) {
                    cls.push('^');
                    i += 1;
                }
                let start = i;
                while i < chars.len() && (chars[i] != ']' || i == start) {
                    if chars[i] == '\\' {
                        i += 1;
                        let escaped = *chars.get(i)?;
                        cls.push('\\');
                        cls.push(escaped);
                    } else {
                        cls.push(chars[i]);
                    }
                    i += 1;
                }
                if i >= chars.len() {
                    return None;
                }
                cls.push(']');
                pat.push_str(&cls);
            }
            c => pat.push_str(&regex::escape(&c.to_string())),
        }
        i += 1;
    }
    pat.push('$');
    Regex::new(&pat).ok()
}
