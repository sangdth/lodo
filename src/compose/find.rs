//! Project root and Docker Compose file discovery.

use std::ffi::OsStr;
use std::fs;
use std::io;
use std::path::{Component, Path, PathBuf};
use std::sync::LazyLock;

use nix::unistd::{Uid, User};
use regex::Regex;

/// Maximum directory depth below `root` that `find` traverses:
/// `root/a/b/c/compose.yml` is found, `root/a/b/c/d/compose.yml` is not.
const MAX_DEPTH: usize = 3;

const LOCK_SUFFIXES: [&str; 4] = [".lock", ".lockb", "-lock.json", "-lock.yaml"];

const MANIFESTS: [&str; 6] = [
    "package.json",
    "go.mod",
    "pyproject.toml",
    "Cargo.toml",
    "Gemfile",
    "composer.json",
];

#[expect(clippy::expect_used)] // compile-time valid static regex
static FILE_NAME_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)^(?:docker[-.])?compose(?:\.([a-z0-9][a-z0-9_-]*))?\.ya?ml$")
        .expect("valid FILE_NAME_RE")
});

/// Lexically cleans `.` and `..` components without resolving symlinks (matching Go's `filepath.Clean`).
pub(crate) fn clean_path(path: &Path) -> PathBuf {
    let mut prefix: Option<&OsStr> = None;
    let mut has_root = false;
    let mut stack: Vec<&OsStr> = Vec::new();

    for comp in path.components() {
        match comp {
            Component::Prefix(p) => prefix = Some(p.as_os_str()),
            Component::RootDir => has_root = true,
            Component::CurDir => {}
            Component::ParentDir => {
                if stack.last().is_some_and(|last| *last != OsStr::new("..")) {
                    stack.pop();
                } else if !has_root {
                    stack.push(OsStr::new(".."));
                }
            }
            Component::Normal(c) => stack.push(c),
        }
    }

    let mut out = PathBuf::new();
    if let Some(p) = prefix {
        out.push(p);
    }
    if has_root {
        out.push("/");
    }
    for part in stack {
        out.push(part);
    }
    if out.as_os_str().is_empty() {
        out.push(".");
    }
    out
}

/// Converts `path` into a lexically cleaned absolute path (matching Go's `filepath.Abs`).
pub(crate) fn abs_path(path: &Path) -> io::Result<PathBuf> {
    if path.is_absolute() {
        Ok(clean_path(path))
    } else {
        Ok(clean_path(&std::env::current_dir()?.join(path)))
    }
}

/// Reports whether `path` stays inside its base directory without escaping via `..` or a leading `/`
/// (matching Go's `filepath.IsLocal`).
pub(crate) fn is_local_path(path: &Path) -> bool {
    if path.as_os_str().is_empty() || path.is_absolute() {
        return false;
    }
    let cleaned = clean_path(path);
    !matches!(cleaned.components().next(), Some(Component::ParentDir))
}

fn user_home_dir() -> Option<PathBuf> {
    if let Some(home) = std::env::var_os("HOME")
        && !home.is_empty()
    {
        return Some(clean_path(Path::new(&home)));
    }
    User::from_uid(Uid::current())
        .ok()
        .flatten()
        .map(|u| clean_path(&u.dir))
}

/// Returns the project `dir` is in: the nearest `.git` entry (directory or worktree file)
/// above `dir`, or outside a repository the nearest folder above `dir` holding both a lock file
/// and a dependency manifest.
///
/// The user's home directory is never treated as a project root, and `None` is returned if a
/// directory on the way up cannot be read.
pub fn project_root(dir: &Path) -> Option<PathBuf> {
    project_root_with_home(dir, user_home_dir().as_deref())
}

fn project_root_with_home(dir: &Path, home: Option<&Path>) -> Option<PathBuf> {
    let abs = abs_path(dir).ok()?;
    let found = match walk_up(&abs, is_git_root) {
        Ok(Some(root)) => root,
        Ok(None) => walk_up(&abs, is_package_root).ok().flatten()?,
        Err(_) => return None,
    };
    if home.is_some_and(|h| found == clean_path(h)) {
        return None;
    }
    Some(found)
}

fn walk_up(dir: &Path, check: fn(&Path) -> io::Result<bool>) -> io::Result<Option<PathBuf>> {
    let mut cur = dir;
    loop {
        if check(cur)? {
            return Ok(Some(cur.to_path_buf()));
        }
        match cur.parent() {
            Some(parent) if parent != cur => cur = parent,
            _ => return Ok(None),
        }
    }
}

fn is_git_root(dir: &Path) -> io::Result<bool> {
    match fs::symlink_metadata(dir.join(".git")) {
        Ok(_) => Ok(true),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(e) => Err(e),
    }
}

fn is_package_root(dir: &Path) -> io::Result<bool> {
    let mut has_lock = false;
    let mut has_manifest = false;
    for entry_res in fs::read_dir(dir)? {
        let entry = entry_res?;
        if entry.file_type()?.is_dir() {
            continue;
        }
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            continue;
        };
        if is_lock_file(&name) {
            has_lock = true;
        }
        if MANIFESTS.contains(&name.as_str()) {
            has_manifest = true;
        }
        if has_lock && has_manifest {
            return Ok(true);
        }
    }
    Ok(false)
}

fn is_lock_file(name: &str) -> bool {
    match name {
        "go.sum" => true,
        "skills-lock.json" => false,
        _ => LOCK_SUFFIXES.iter().any(|s| name.ends_with(s)),
    }
}

struct Match {
    path: PathBuf,
    rank: u8,
    depth: usize,
}

/// Returns the Docker Compose files under `root`, ordered best-first, as absolute paths.
///
/// Looks at most 3 folders deep, skipping hidden directories, `node_modules`, `vendor`, and
/// `testdata`. Development variants rank first (`0`), plain compose files second (`1`), other
/// variants third (`2`), and production variants last (`3`); ties break by shallower depth then path.
/// Follows `root` if `root` itself is a symlink, does not follow directory symlinks below `root`,
/// and includes only entries whose target is a regular file.
pub fn find(root: &Path) -> Result<Vec<PathBuf>, String> {
    let root = abs_path(root).map_err(|e| format!("find compose files: {e}"))?;
    let root_entries = fs::read_dir(&root)
        .map_err(|e| format!("find compose files in {}: {e}", root.display()))?;

    let mut found = Vec::new();
    walk_dir(root_entries, &root, 0, &mut found);

    found.sort_by(|a, b| {
        a.rank
            .cmp(&b.rank)
            .then_with(|| a.depth.cmp(&b.depth))
            .then_with(|| {
                a.path
                    .as_os_str()
                    .as_encoded_bytes()
                    .cmp(b.path.as_os_str().as_encoded_bytes())
            })
    });

    Ok(found.into_iter().map(|m| m.path).collect())
}

fn walk_dir(entries: fs::ReadDir, dir: &Path, depth: usize, found: &mut Vec<Match>) {
    for entry_res in entries {
        let Ok(entry) = entry_res else {
            continue;
        };
        let Ok(ft) = entry.file_type() else {
            continue;
        };
        let file_name = entry.file_name();
        let Some(name) = file_name.to_str() else {
            continue;
        };
        let full_path = dir.join(&file_name);

        if ft.is_dir() {
            if depth >= MAX_DEPTH || skip_dir(name) {
                continue;
            }
            if let Ok(sub_entries) = fs::read_dir(&full_path) {
                walk_dir(sub_entries, &full_path, depth + 1, found);
            }
            continue;
        }

        let Some(caps) = FILE_NAME_RE.captures(name) else {
            continue;
        };
        // Follow symlinks on candidate files: accept regular files only, never directories, FIFOs, or devices.
        if let Ok(info) = fs::metadata(&full_path)
            && info.file_type().is_file()
        {
            let variant = caps.get(1).map_or("", |m| m.as_str());
            found.push(Match {
                path: full_path,
                rank: rank(variant),
                depth,
            });
        }
    }
}

fn rank(variant: &str) -> u8 {
    match variant.to_ascii_lowercase().as_str() {
        "dev" | "develop" | "development" | "local" => 0,
        "" => 1,
        "prod" | "production" => 3,
        _ => 2,
    }
}

fn skip_dir(name: &str) -> bool {
    name.starts_with('.') || matches!(name, "node_modules" | "vendor" | "testdata" | "target")
}

#[cfg(test)]
mod tests {
    use std::fs::Permissions;
    use std::os::unix::fs::{PermissionsExt, symlink};

    use nix::sys::stat::Mode;
    use nix::unistd::mkfifo;

    use super::*;

    fn tree(root: &Path, paths: &[&str]) {
        for p in paths {
            let full = root.join(p);
            if p.ends_with('/') {
                fs::create_dir_all(&full).unwrap();
            } else {
                if let Some(parent) = full.parent() {
                    fs::create_dir_all(parent).unwrap();
                }
                fs::write(&full, b"").unwrap();
            }
        }
    }

    struct ModeGuard {
        dir: PathBuf,
    }

    impl ModeGuard {
        fn lock(dir: &Path, mode: u32) -> Option<Self> {
            if Uid::effective().is_root() {
                return None;
            }
            fs::set_permissions(dir, Permissions::from_mode(mode)).unwrap();
            Some(Self {
                dir: dir.to_path_buf(),
            })
        }
    }

    impl Drop for ModeGuard {
        fn drop(&mut self) {
            let _ = fs::set_permissions(&self.dir, Permissions::from_mode(0o755));
        }
    }

    fn rel_from(base: &Path, target: &Path) -> PathBuf {
        let cleaned_base = clean_path(base);
        let cleaned_target = clean_path(target);
        let base_comps: Vec<_> = cleaned_base.components().collect();
        let target_comps: Vec<_> = cleaned_target.components().collect();
        let common = base_comps
            .iter()
            .zip(target_comps.iter())
            .take_while(|(a, b)| a == b)
            .count();
        let mut out = PathBuf::new();
        for _ in common..base_comps.len() {
            out.push("..");
        }
        for comp in &target_comps[common..] {
            out.push(comp.as_os_str());
        }
        if out.as_os_str().is_empty() {
            out.push(".");
        }
        out
    }

    #[test]
    fn project_root_cases() {
        let cases: &[(&str, &[&str], &str, Option<&str>)] = &[
            (
                "git folder and package-lock.json",
                &[".git/", "package-lock.json"],
                ".",
                Some("."),
            ),
            (
                "git file of a worktree",
                &[".git", "pnpm-lock.yaml"],
                ".",
                Some("."),
            ),
            (
                "subfolder of a project",
                &[".git/", "yarn.lock", "apps/web/src/"],
                "apps/web/src",
                Some("."),
            ),
            (
                "git without a lock file",
                &[".git/", "package.json"],
                ".",
                Some("."),
            ),
            (
                "git root wins over a lock file below it",
                &[".git/", "web/package-lock.json", "web/package.json"],
                "web",
                Some("."),
            ),
            (
                "nearest git root decides",
                &[".git/", "go.sum", "sub/.git", "sub/x/"],
                "sub/x",
                Some("sub"),
            ),
            (
                "lock file and manifest without git",
                &["package.json", "pnpm-lock.yaml", "a/"],
                "a",
                Some("."),
            ),
            ("go.sum without git", &["go.mod", "go.sum"], ".", Some(".")),
            (
                "nearest lock folder without git",
                &["go.mod", "go.sum", "web/package.json", "web/bun.lockb"],
                "web",
                Some("web"),
            ),
            ("lock file without a manifest", &["x.lock", "a/"], "a", None),
            ("manifest without a lock file", &["package.json"], ".", None),
            (
                "skills-lock.json is no lock file",
                &["package.json", "skills-lock.json"],
                ".",
                None,
            ),
            (
                "folder named like a lock file",
                &["package.json", "deps.lock/"],
                ".",
                None,
            ),
            ("neither", &["a/"], "a", None),
        ];

        for (name, paths, dir, want) in cases {
            let tmp = tempfile::tempdir().unwrap();
            tree(tmp.path(), paths);
            let got = project_root(&tmp.path().join(dir));
            let expected = want.map(|w| clean_path(&tmp.path().join(w)));
            assert_eq!(got, expected, "case {name}");
        }
    }

    #[test]
    fn project_root_excludes_home() {
        let tmp = tempfile::tempdir().unwrap();
        tree(
            tmp.path(),
            &[".git/", "package.json", "package-lock.json", "Downloads/"],
        );
        assert_eq!(
            project_root_with_home(&tmp.path().join("Downloads"), Some(tmp.path())),
            None
        );
    }

    #[test]
    fn project_root_relative_dir() {
        let tmp = tempfile::tempdir().unwrap();
        tree(tmp.path(), &[".git/", "go.sum", "cmd/"]);
        let wd = std::env::current_dir().unwrap();
        let rel = rel_from(&wd, &tmp.path().join("cmd"));
        assert_eq!(project_root(&rel), Some(clean_path(tmp.path())));
    }

    #[test]
    fn project_root_unreadable() {
        {
            let tmp = tempfile::tempdir().unwrap();
            tree(tmp.path(), &["package.json", "go.sum"]);
            if let Some(_guard) = ModeGuard::lock(tmp.path(), 0o311) {
                assert_eq!(project_root(tmp.path()), None);
            }
        }
        {
            let tmp = tempfile::tempdir().unwrap();
            tree(tmp.path(), &[".git/", "go.sum", "locked/app/"]);
            let locked = tmp.path().join("locked");
            if let Some(_guard) = ModeGuard::lock(&locked, 0o000) {
                assert_eq!(project_root(&locked.join("app")), None);
            }
        }
    }

    #[test]
    fn find_ranking_depth_and_skipped_dirs() {
        let cases: &[(&str, &[&str], &[&str])] = &[
            (
                "development first, production last",
                &[
                    "compose.prod.yml",
                    "compose.yml",
                    "compose.override.yml",
                    "compose.dev.yaml",
                    "docker-compose.production.yaml",
                    "docker-compose.traefik.yml",
                    "compose.local.yml",
                ],
                &[
                    "compose.dev.yaml",
                    "compose.local.yml",
                    "compose.yml",
                    "compose.override.yml",
                    "docker-compose.traefik.yml",
                    "compose.prod.yml",
                    "docker-compose.production.yaml",
                ],
            ),
            (
                "every development variant",
                &[
                    "compose.local.yml",
                    "compose.development.yml",
                    "compose.develop.yml",
                    "compose.dev.yml",
                ],
                &[
                    "compose.dev.yml",
                    "compose.develop.yml",
                    "compose.development.yml",
                    "compose.local.yml",
                ],
            ),
            (
                "variant in capitals",
                &["compose.yml", "COMPOSE.DEV.YAML", "Docker-Compose.PROD.yml"],
                &["COMPOSE.DEV.YAML", "compose.yml", "Docker-Compose.PROD.yml"],
            ),
            (
                "shallower first, then path",
                &[
                    "b/compose.yml",
                    "a/b/compose.yml",
                    "compose.yml",
                    "a/compose.yml",
                ],
                &[
                    "compose.yml",
                    "a/compose.yml",
                    "b/compose.yml",
                    "a/b/compose.yml",
                ],
            ),
            (
                "rank before depth",
                &["compose.yml", "docker/compose.dev.yml", "compose.prod.yml"],
                &["docker/compose.dev.yml", "compose.yml", "compose.prod.yml"],
            ),
            (
                "three folders deep at most",
                &["a/b/c/compose.yml", "a/b/c/d/compose.dev.yml"],
                &["a/b/c/compose.yml"],
            ),
            (
                "skipped folders",
                &[
                    "node_modules/pkg/compose.yml",
                    "vendor/compose.yml",
                    ".hidden/compose.yml",
                    "testdata/compose.yml",
                    "apps/.cache/compose.yml",
                    "apps/node_modules/compose.yml",
                    "apps/api/compose.yml",
                ],
                &["apps/api/compose.yml"],
            ),
            (
                "no compose file",
                &["README.md", "deploy/compose.yml.bak"],
                &[],
            ),
        ];

        for (name, paths, want) in cases {
            let tmp = tempfile::tempdir().unwrap();
            tree(tmp.path(), paths);
            let got = find(tmp.path()).unwrap();
            let expected: Vec<PathBuf> = want.iter().map(|p| tmp.path().join(p)).collect();
            assert_eq!(got, expected, "case {name}");
        }
    }

    #[test]
    fn find_file_names() {
        let cases: &[(&str, bool)] = &[
            ("compose.yml", true),
            ("compose.yaml", true),
            ("docker-compose.yml", true),
            ("docker.compose.yaml", true),
            ("compose.dev.yaml", true),
            ("docker-compose.prod.yml", true),
            ("docker-compose.traefik.yml", true),
            ("compose.ci_2.yml", true),
            ("Docker-Compose.YML", true),
            ("compose-dev.yml", false),
            ("my-compose.yml", false),
            ("compose.yml.bak", false),
            ("compose.json", false),
            ("compose.dev.local.yml", false),
            ("compose..yml", false),
            ("compose.-dev.yml", false),
            ("compose_dev.yml", false),
            ("dockercompose.yml", false),
            ("docker_compose.yml", false),
        ];

        for (name, should_match) in cases {
            let tmp = tempfile::tempdir().unwrap();
            tree(tmp.path(), &[name]);
            let got = find(tmp.path()).unwrap();
            assert_eq!(got.len() == 1, *should_match, "name {name}");
        }
    }

    #[test]
    fn find_relative_root_and_symlinks() {
        let tmp = tempfile::tempdir().unwrap();
        tree(tmp.path(), &["compose.yml"]);
        let wd = std::env::current_dir().unwrap();
        let rel = rel_from(&wd, tmp.path());
        assert_eq!(
            find(&rel).unwrap(),
            vec![clean_path(tmp.path()).join("compose.yml")]
        );

        let sym_tmp = tempfile::tempdir().unwrap();
        tree(
            sym_tmp.path(),
            &["project/compose.yml", "elsewhere/compose.dev.yml"],
        );
        symlink(
            sym_tmp.path().join("elsewhere"),
            sym_tmp.path().join("project/linked"),
        )
        .unwrap();
        symlink(sym_tmp.path().join("project"), sym_tmp.path().join("link")).unwrap();

        for root in [sym_tmp.path().join("project"), sym_tmp.path().join("link")] {
            assert_eq!(find(&root).unwrap(), vec![root.join("compose.yml")]);
        }
    }

    #[test]
    fn find_accepts_only_regular_files_and_skips_unreadable_subfolders() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path().join("project");
        tree(tmp.path(), &["project/package.json", "elsewhere/real.yml"]);
        symlink(
            tmp.path().join("elsewhere/real.yml"),
            root.join("compose.dev.yml"),
        )
        .unwrap();
        symlink("/dev/null", root.join("compose.yml")).unwrap();
        symlink(
            tmp.path().join("elsewhere"),
            root.join("docker-compose.yml"),
        )
        .unwrap();
        mkfifo(
            &root.join("compose.local.yml"),
            Mode::S_IRUSR | Mode::S_IWUSR,
        )
        .unwrap();
        assert_eq!(find(&root).unwrap(), vec![root.join("compose.dev.yml")]);

        let sub_tmp = tempfile::tempdir().unwrap();
        tree(sub_tmp.path(), &["locked/compose.dev.yml", "compose.yml"]);
        if let Some(_guard) = ModeGuard::lock(&sub_tmp.path().join("locked"), 0o000) {
            assert_eq!(
                find(sub_tmp.path()).unwrap(),
                vec![sub_tmp.path().join("compose.yml")]
            );
        }
    }

    #[test]
    fn find_root_errors() {
        let tmp = tempfile::tempdir().unwrap();
        assert!(find(&tmp.path().join("gone")).is_err());

        let locked = tempfile::tempdir().unwrap();
        tree(locked.path(), &["compose.yml"]);
        if let Some(_guard) = ModeGuard::lock(locked.path(), 0o311) {
            assert!(find(locked.path()).is_err());
        }
    }
}
