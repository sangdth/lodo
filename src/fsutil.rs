//! Atomic file writes that skip unchanged content, and bounded regular-file reads.

use std::fs::{self, File, OpenOptions, Permissions};
use std::io::{self, Read, Write};
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};

/// Maximum size (`1 MiB`) `read_regular` accepts for a project file.
pub const MAX_PROJECT_FILE: usize = 1 << 20;

static TEMP_SEQ: AtomicU64 = AtomicU64::new(0);

#[derive(Debug, thiserror::Error)]
pub enum FsError {
    #[error("{action} {path}: {source}")]
    Io {
        action: &'static str,
        path: String,
        #[source]
        source: io::Error,
    },
    #[error("read {path}: not a regular file")]
    NotRegular { path: String },
    #[error("read {path}: larger than {max} bytes")]
    TooLarge { path: String, max: usize },
}

impl FsError {
    pub fn is_not_found(&self) -> bool {
        matches!(self, Self::Io { source, .. } if source.kind() == io::ErrorKind::NotFound)
    }
}

/// Reports whether `path` exists (following symlinks).
pub fn exists(path: &Path) -> bool {
    fs::metadata(path).is_ok()
}

/// Replaces `path` with `data` atomically via a temp file + `sync_all` + `rename`.
/// Skips writing when `path` already holds `data`, returning `Ok(false)`.
pub fn write_file(path: &Path, data: &[u8], perm: u32) -> Result<bool, FsError> {
    let path_str = path.display().to_string();
    match fs::read(path) {
        Ok(old) if old == data => return Ok(false),
        Ok(_) => {}
        Err(e) if e.kind() == io::ErrorKind::NotFound => {}
        Err(e) => {
            return Err(FsError::Io {
                action: "read",
                path: path_str,
                source: e,
            });
        }
    }

    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    let base = path.file_name().and_then(|n| n.to_str()).unwrap_or("tmp");
    let pid = std::process::id();
    let seq = TEMP_SEQ.fetch_add(1, Ordering::Relaxed);
    let tmp_path = parent.join(format!(".{base}.{pid}.{seq}"));

    let result = (|| -> io::Result<()> {
        let mut f = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp_path)?;
        f.write_all(data)?;
        f.set_permissions(Permissions::from_mode(perm))?;
        f.sync_all()?;
        drop(f);
        fs::rename(&tmp_path, path)?;
        Ok(())
    })();

    if let Err(e) = result {
        let _ = fs::remove_file(&tmp_path);
        return Err(FsError::Io {
            action: "write",
            path: path_str,
            source: e,
        });
    }
    Ok(true)
}

/// Reads the regular file at `path` (following symlinks), refusing directories,
/// FIFOs, devices, and files larger than `MAX_PROJECT_FILE` bytes.
pub fn read_regular(path: &Path) -> Result<Vec<u8>, FsError> {
    let path_str = path.display().to_string();
    let info = fs::metadata(path).map_err(|e| FsError::Io {
        action: "read",
        path: path_str.clone(),
        source: e,
    })?;
    if !info.file_type().is_file() {
        return Err(FsError::NotRegular { path: path_str });
    }

    let file = File::open(path).map_err(|e| FsError::Io {
        action: "read",
        path: path_str.clone(),
        source: e,
    })?;
    let opened_info = file.metadata().map_err(|e| FsError::Io {
        action: "read",
        path: path_str.clone(),
        source: e,
    })?;
    if !opened_info.file_type().is_file() {
        return Err(FsError::NotRegular { path: path_str });
    }

    let mut data = Vec::new();
    file.take((MAX_PROJECT_FILE + 1) as u64)
        .read_to_end(&mut data)
        .map_err(|e| FsError::Io {
            action: "read",
            path: path_str.clone(),
            source: e,
        })?;
    if data.len() > MAX_PROJECT_FILE {
        return Err(FsError::TooLarge {
            path: path_str,
            max: MAX_PROJECT_FILE,
        });
    }
    Ok(data)
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::{PermissionsExt, symlink};

    use super::*;

    #[test]
    fn write_file_creates_updates_and_skips_unchanged() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("f.conf");

        assert!(write_file(&path, b"a\n", 0o644).unwrap());
        assert_eq!(fs::read_to_string(&path).unwrap(), "a\n");
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o644
        );

        assert!(!write_file(&path, b"a\n", 0o644).unwrap());
        assert!(write_file(&path, b"new\n", 0o644).unwrap());
        assert_eq!(fs::read_to_string(&path).unwrap(), "new\n");
        assert_eq!(fs::read_dir(dir.path()).unwrap().count(), 1);
    }

    #[test]
    fn write_file_errors_on_missing_parent() {
        let dir = tempfile::tempdir().unwrap();
        let err = write_file(&dir.path().join("missing/f"), b"x", 0o644).unwrap_err();
        assert!(err.to_string().contains("write "));
    }

    #[test]
    fn read_regular_accepts_regular_and_symlink_and_rejects_special_or_oversized() {
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("f");
        fs::write(&file, b"abc").unwrap();
        assert_eq!(read_regular(&file).unwrap(), b"abc");

        let link = dir.path().join("link");
        symlink(&file, &link).unwrap();
        assert_eq!(read_regular(&link).unwrap(), b"abc");

        let dir_err = read_regular(dir.path()).unwrap_err();
        assert!(dir_err.to_string().contains("not a regular file"));

        let dev_link = dir.path().join("devnull");
        symlink("/dev/null", &dev_link).unwrap();
        let dev_err = read_regular(&dev_link).unwrap_err();
        assert!(dev_err.to_string().contains("not a regular file"));

        let exact = dir.path().join("exact");
        fs::write(&exact, vec![b'a'; MAX_PROJECT_FILE]).unwrap();
        assert_eq!(read_regular(&exact).unwrap().len(), MAX_PROJECT_FILE);

        let over = dir.path().join("over");
        fs::write(&over, vec![b'a'; MAX_PROJECT_FILE + 1]).unwrap();
        let over_err = read_regular(&over).unwrap_err();
        assert!(over_err.to_string().contains("larger than 1048576 bytes"));

        let missing = read_regular(&dir.path().join("missing")).unwrap_err();
        assert!(missing.is_not_found());
    }
}
