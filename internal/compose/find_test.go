package compose_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/oo/internal/compose"
)

// tree creates each path under root, with its parent folders: a path ending
// in / is a folder, any other an empty file.
func tree(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// lock sets dir's mode until the test ends, and skips the test for root,
// whom permissions don't stop.
func lock(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder")
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Error(err)
		}
	})
}

func TestProjectRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		paths []string // created under a temp dir
		dir   string   // where ProjectRoot starts, under the temp dir
		want  string   // the root it returns, under the temp dir; empty means none
	}{
		{name: "git folder and package-lock.json", paths: []string{".git/", "package-lock.json"}, dir: ".", want: "."},
		{name: "git file of a worktree", paths: []string{".git", "pnpm-lock.yaml"}, dir: ".", want: "."},
		{name: "subfolder of a project", paths: []string{".git/", "yarn.lock", "apps/web/src/"}, dir: "apps/web/src", want: "."},
		{name: "go.sum", paths: []string{".git/", "go.mod", "go.sum"}, dir: ".", want: "."},
		{name: "bun.lockb", paths: []string{".git/", "bun.lockb"}, dir: ".", want: "."},
		{name: "Cargo.lock", paths: []string{".git/", "Cargo.lock"}, dir: ".", want: "."},
		{name: "no lock file", paths: []string{".git/", "package.json", "go.mod"}, dir: "."},
		{name: "lock file only in a subfolder", paths: []string{".git/", "web/package-lock.json"}, dir: "web"},
		{name: "folder named like a lock file", paths: []string{".git/", "deps.lock/"}, dir: "."},
		{name: "nearest git root decides", paths: []string{".git/", "go.sum", "sub/.git", "sub/x/"}, dir: "sub/x"},
		{name: "nested repo with its own lock file", paths: []string{".git/", "go.sum", "sub/.git/", "sub/yarn.lock"}, dir: "sub", want: "sub"},
		{name: "outside any repo", paths: []string{"package-lock.json", "a/"}, dir: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			tree(t, tmp, tt.paths...)
			got, ok := compose.ProjectRoot(filepath.Join(tmp, tt.dir))
			want, wantOK := "", tt.want != ""
			if wantOK {
				want = filepath.Join(tmp, tt.want)
			}
			if got != want || ok != wantOK {
				t.Errorf("ProjectRoot(%s) = %q, %v; want %q, %v", tt.dir, got, ok, want, wantOK)
			}
		})
	}
}

func TestProjectRoot_RelativeDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	tree(t, tmp, ".git/", "go.sum", "cmd/")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, filepath.Join(tmp, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := compose.ProjectRoot(rel); got != tmp || !ok {
		t.Errorf("ProjectRoot(%s) = %q, %v; want %q, true", rel, got, ok, tmp)
	}
}

func TestProjectRoot_Unreadable(t *testing.T) {
	t.Parallel()

	t.Run("root", func(t *testing.T) {
		t.Parallel()

		tmp := t.TempDir()
		tree(t, tmp, ".git/", "go.sum")
		lock(t, tmp, 0o311) // .git can be found, the lock file can't
		if got, ok := compose.ProjectRoot(tmp); ok {
			t.Errorf("ProjectRoot of an unreadable root = %q, true; want false", got)
		}
	})
	t.Run("folder on the way up", func(t *testing.T) {
		t.Parallel()

		// A .git under the locked folder can't be ruled out, so the
		// project above it doesn't count.
		tmp := t.TempDir()
		tree(t, tmp, ".git/", "go.sum", "locked/app/")
		lock(t, filepath.Join(tmp, "locked"), 0)
		if got, ok := compose.ProjectRoot(filepath.Join(tmp, "locked", "app")); ok {
			t.Errorf("ProjectRoot past a locked folder = %q, true; want false", got)
		}
	})
}

func TestFind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		paths []string // created under root
		want  []string // under root, best first
	}{
		{
			name: "development first, production last",
			paths: []string{
				"compose.prod.yml", "compose.yml", "compose.override.yml", "compose.dev.yaml",
				"docker-compose.production.yaml", "docker-compose.traefik.yml", "compose.local.yml",
			},
			want: []string{
				"compose.dev.yaml", "compose.local.yml", "compose.yml", "compose.override.yml",
				"docker-compose.traefik.yml", "compose.prod.yml", "docker-compose.production.yaml",
			},
		},
		{
			name:  "every development variant",
			paths: []string{"compose.local.yml", "compose.development.yml", "compose.develop.yml", "compose.dev.yml"},
			want:  []string{"compose.dev.yml", "compose.develop.yml", "compose.development.yml", "compose.local.yml"},
		},
		{
			name:  "variant in capitals",
			paths: []string{"compose.yml", "COMPOSE.DEV.YAML", "Docker-Compose.PROD.yml"},
			want:  []string{"COMPOSE.DEV.YAML", "compose.yml", "Docker-Compose.PROD.yml"},
		},
		{
			name:  "shallower first, then path",
			paths: []string{"b/compose.yml", "a/b/compose.yml", "compose.yml", "a/compose.yml"},
			want:  []string{"compose.yml", "a/compose.yml", "b/compose.yml", "a/b/compose.yml"},
		},
		{
			name:  "rank before depth",
			paths: []string{"compose.yml", "docker/compose.dev.yml", "compose.prod.yml"},
			want:  []string{"docker/compose.dev.yml", "compose.yml", "compose.prod.yml"},
		},
		{
			name:  "three folders deep at most",
			paths: []string{"a/b/c/compose.yml", "a/b/c/d/compose.dev.yml"},
			want:  []string{"a/b/c/compose.yml"},
		},
		{
			name: "skipped folders",
			paths: []string{
				"node_modules/pkg/compose.yml", "vendor/compose.yml", ".hidden/compose.yml",
				"apps/.cache/compose.yml", "apps/node_modules/compose.yml", "apps/api/compose.yml",
			},
			want: []string{"apps/api/compose.yml"},
		},
		{
			name:  "no compose file",
			paths: []string{"README.md", "deploy/compose.yml.bak"},
			want:  []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			tree(t, root, tt.paths...)
			got, err := compose.Find(root)
			if err != nil {
				t.Fatalf("Find: %v", err)
			}
			want := make([]string, len(tt.want))
			for i, p := range tt.want {
				want[i] = filepath.Join(root, filepath.FromSlash(p))
			}
			if !slices.Equal(got, want) {
				t.Errorf("Find =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

func TestFind_Names(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		match bool
	}{
		{name: "compose.yml", match: true},
		{name: "compose.yaml", match: true},
		{name: "docker-compose.yml", match: true},
		{name: "docker.compose.yaml", match: true},
		{name: "compose.dev.yaml", match: true},
		{name: "docker-compose.prod.yml", match: true},
		{name: "docker-compose.traefik.yml", match: true},
		{name: "compose.ci_2.yml", match: true},
		{name: "Docker-Compose.YML", match: true},
		{name: "compose-dev.yml"},
		{name: "my-compose.yml"},
		{name: "compose.yml.bak"},
		{name: "compose.json"},
		{name: "compose.dev.local.yml"},
		{name: "compose..yml"},
		{name: "compose.-dev.yml"},
		{name: "compose_dev.yml"},
		{name: "dockercompose.yml"},
		{name: "docker_compose.yml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			tree(t, root, tt.name)
			got, err := compose.Find(root)
			if err != nil {
				t.Fatalf("Find: %v", err)
			}
			if match := len(got) == 1; match != tt.match {
				t.Errorf("Find found %q; want a match: %v", got, tt.match)
			}
		})
	}
}

func TestFind_RelativeRoot(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	tree(t, tmp, "compose.yml")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, tmp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := compose.Find(rel)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if want := []string{filepath.Join(tmp, "compose.yml")}; !slices.Equal(got, want) {
		t.Errorf("Find(%s) = %q, want %q", rel, got, want)
	}
}

// TestFind_Symlinks checks that Find walks a root reached through a symlink,
// and follows no symlink below it.
func TestFind_Symlinks(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	tree(t, tmp, "project/compose.yml", "elsewhere/compose.dev.yml")
	if err := os.Symlink(filepath.Join(tmp, "elsewhere"), filepath.Join(tmp, "project", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "project"), filepath.Join(tmp, "link")); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(tmp, "project"), filepath.Join(tmp, "link")} {
		got, err := compose.Find(root)
		if err != nil {
			t.Fatalf("Find(%s): %v", root, err)
		}
		if want := []string{filepath.Join(root, "compose.yml")}; !slices.Equal(got, want) {
			t.Errorf("Find(%s) = %q, want %q", root, got, want)
		}
	}
}

func TestFind_UnreadableFolder(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tree(t, root, "locked/compose.dev.yml", "compose.yml")
	lock(t, filepath.Join(root, "locked"), 0)
	got, err := compose.Find(root)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if want := []string{filepath.Join(root, "compose.yml")}; !slices.Equal(got, want) {
		t.Errorf("Find = %q, want %q", got, want)
	}
}

func TestFind_RootErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		t.Parallel()

		_, err := compose.Find(filepath.Join(t.TempDir(), "gone"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Find of a missing root: %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		tree(t, root, "compose.yml")
		lock(t, root, 0o311)
		_, err := compose.Find(root)
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("Find of an unreadable root: %v, want fs.ErrPermission", err)
		}
	})
}
