// Package fsutil writes files so a reader sees either the old content or the
// new, never a partial file, and reads a project's files only when they are
// regular files of a bounded size.
package fsutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile replaces path with data through a temporary file and a rename.
// It skips the write when path already holds data, and reports whether it
// wrote.
func WriteFile(path string, data []byte, perm fs.FileMode) (changed bool, err error) {
	old, err := os.ReadFile(path) //nolint:gosec // G304: callers pass paths from paths.Paths
	if err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name()) // best effort: the write already failed
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close() // the write error is the one to report
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close() // the chmod error is the one to report
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close() // the sync error is the one to report
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// MaxProjectFile is the most ReadRegular reads of a file a project holds: a
// compose file, package.json, a script or a .env is far smaller.
const MaxProjectFile = 1 << 20

// ReadRegular returns the content of the regular file at path, following
// symlinks. It refuses a directory, a device or a FIFO before it opens one,
// since opening a FIFO blocks and a device can be endless, and it refuses a
// file over limit bytes. A missing file's error matches fs.ErrNotExist.
func ReadRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read %s: not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: callers pass a project's own files
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read only: a close error loses nothing
	// The path can change between Stat and Open; check the file opened.
	if info, err = f.Stat(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read %s: not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("read %s: larger than %d bytes", path, limit)
	}
	return data, nil
}
