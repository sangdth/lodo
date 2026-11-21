//! Git worktree detection and automatic `<main><n>.<tld>` slot assignment.

use std::path::{Path, PathBuf};

use crate::compose;
use crate::store::{self, Domain};

use super::env::{env_project, repo_root};
use super::{Project, listed_project, same_folder, taken_ref};

impl Project {
    /// Reports whether this project is a git worktree (its main repository checkout is a different folder).
    pub fn is_worktree(&self) -> bool {
        !self.repo.is_empty() && !same_folder(&self.repo, &self.root)
    }

    /// Alias of [`Project::is_worktree`].
    pub fn worktree(&self) -> bool {
        self.is_worktree()
    }
}

/// Returns the root folder of the git worktree `dir` is inside, or `None` when `dir` is outside
/// a git repository or inside its main checkout.
pub fn worktree_root(dir: &Path) -> Option<PathBuf> {
    let root = compose::project_root(dir)?;
    let root_str = root.to_string_lossy().into_owned();
    let repo = repo_root(&root);
    let proj = Project {
        root: root_str,
        repo,
        ..Project::default()
    };
    proj.is_worktree().then_some(root)
}

/// Returns the main checkout's project `Domain` when `p` is a git worktree whose main checkout is
/// either listed in `domains` or named as an unlisted `.lodo`/`.test` host in `p.env`.
pub(crate) fn main_project(
    p: &Project,
    domains: &[Domain],
    gone: &dyn Fn(&Path) -> bool,
) -> Option<Domain> {
    if !p.is_worktree() {
        return None;
    }
    if let Some(d) = listed_project(domains, &p.repo, gone) {
        return Some(d);
    }
    let name = env_project(p, domains);
    if !name.is_empty() {
        return Some(Domain {
            name,
            address: p.env_address.clone(),
            ..Domain::default()
        });
    }
    None
}

/// Picks `<label><n><suffix>` on `127.0.1.<n>` for the lowest `n` whose address is free or holds
/// only a gone/unset folder's `<label><n><suffix>`.
pub(crate) fn worktree_slot(
    p: &Project,
    domains: &[Domain],
    main: &Domain,
    gone: &dyn Fn(&Path) -> bool,
) -> Option<(String, String)> {
    let (label, suffix) = store::cut_project(&main.name);
    let p_root = Path::new(&p.root);

    for n in store::OWN_FIRST..=store::OWN_LAST {
        let candidate_name = format!("{label}{n}{suffix}");
        let address = store::own_address(n);
        if store::validate_project(&candidate_name).is_err() {
            return None; // label is too long: every larger n will also fail
        }
        let owner = store::find(domains, &candidate_name);
        let others = address == main.address
            || domains
                .iter()
                .any(|d| d.address == address && store::project(&d.name) != candidate_name);
        if others {
            continue;
        }
        match owner {
            None if !domains
                .iter()
                .any(|d| store::project(&d.name) == candidate_name) =>
            {
                return Some((candidate_name, address));
            }
            Some(owner) if owner.address == address && !taken_ref(owner, p_root, gone) => {
                return Some((candidate_name, address));
            }
            _ => {}
        }
    }
    None
}

/// Copies the `https` flag from the corresponding domain under `main` so a worktree's URLs match
/// its main checkout's scheme.
pub(crate) fn from_main(
    mut d: Domain,
    project: &str,
    main: Option<&Domain>,
    domains: &[Domain],
) -> Domain {
    let Some(main) = main else {
        return d;
    };
    if main.name.is_empty() || d.port == 0 {
        return d;
    }
    if let Some(prefix) = d.name.strip_suffix(project)
        && let Some(m) = store::find(domains, &format!("{prefix}{}", main.name))
    {
        d.https = m.https;
    }
    d
}
