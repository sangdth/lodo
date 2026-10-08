package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sangdth/lcd/internal/fsutil"
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

func ptr(s string) *string { return &s }
