// Package fsutil writes files so a reader sees either the old content or the
// new, never a partial file.
package fsutil

import (
	"bytes"
	"errors"
	"fmt"
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
