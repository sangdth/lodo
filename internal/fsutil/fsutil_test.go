package fsutil_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/fsutil"
)

func TestWriteFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		existing    *string
		data        string
		wantChanged bool
	}{
		{name: "new file", data: "a\n", wantChanged: true},
		{name: "different content", existing: ptr("old\n"), data: "new\n", wantChanged: true},
		{name: "same content", existing: ptr("same\n"), data: "same\n", wantChanged: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "f.conf")
			if tt.existing != nil {
				if err := os.WriteFile(path, []byte(*tt.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := fsutil.WriteFile(path, []byte(tt.data), 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.data {
				t.Errorf("content = %q, want %q", got, tt.data)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("dir holds %d entries, want only the file (no temp files left)", len(entries))
			}
			if tt.wantChanged {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o644 {
					t.Errorf("mode = %v, want 0644", info.Mode().Perm())
				}
			}
		})
	}
}

func TestWriteFile_MissingDir(t *testing.T) {
	t.Parallel()

	_, err := fsutil.WriteFile(filepath.Join(t.TempDir(), "missing", "f"), []byte("x"), 0o644)
	if err == nil {
		t.Fatal("err = nil, want an error for a missing directory")
	}
}

func TestReadRegular(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) string // returns the path to read
		want    string
		wantErr string
	}{
		{
			name: "regular file",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return writeTemp(t, dir, "f", "abc")
			},
			want: "abc",
		},
		{
			name: "symlink to a regular file",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return symlink(t, writeTemp(t, dir, "f", "abc"), filepath.Join(dir, "link"))
			},
			want: "abc",
		},
		{
			name:    "directory",
			setup:   func(_ *testing.T, dir string) string { return dir },
			wantErr: "not a regular file",
		},
		{
			// A char device is refused by Stat alone: it is never opened.
			name: "symlink to /dev/null",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return symlink(t, "/dev/null", filepath.Join(dir, "link"))
			},
			wantErr: "not a regular file",
		},
		{
			name: "over the limit",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return writeTemp(t, dir, "f", "abcde")
			},
			wantErr: "larger than 4 bytes",
		},
		{
			name: "exactly the limit",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				return writeTemp(t, dir, "f", "abcd")
			},
			want: "abcd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := fsutil.ReadRegular(tt.setup(t, t.TempDir()), 4)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("content = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadRegular_Missing(t *testing.T) {
	t.Parallel()

	_, err := fsutil.ReadRegular(filepath.Join(t.TempDir(), "missing"), fsutil.MaxProjectFile)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func ptr(s string) *string { return &s }
