package compose

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// maxDepth is how many folders below the root Find looks:
// root/a/b/c/compose.yml is found, root/a/b/c/d/compose.yml is not.
const maxDepth = 3

// lockSuffixes end the names of package managers' lock files: yarn.lock,
// bun.lockb, package-lock.json, pnpm-lock.yaml. Go's is go.sum.
var lockSuffixes = []string{".lock", ".lockb", "-lock.json", "-lock.yaml"}

// fileNameRE matches a compose file's name in any case: compose.yml,
// docker-compose.yaml, compose.dev.yaml. The group is the variant, dev.
var fileNameRE = regexp.MustCompile(`(?i)^(?:docker[-.])?compose(?:\.([a-z0-9][a-z0-9_-]*))?\.ya?ml$`)

// ProjectRoot returns the project dir is in: the nearest git root above it,
// or outside a repository the nearest folder above it that holds a lock file
// and the manifest it pins: a stray *.lock in a temp folder makes no project.
// The home folder is never a project, so a dotfiles repository there doesn't
// make every folder one. ok is false outside a project, and when a folder on
// the way up can't be read: a .git under it can't be ruled out.
func ProjectRoot(dir string) (root string, ok bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	root, found, err := walkUp(dir, isGitRoot)
	if err == nil && !found {
		root, found, err = walkUp(dir, isPackageRoot)
	}
	if err != nil || !found {
		return "", false
	}
	if home, err := os.UserHomeDir(); err == nil && root == filepath.Clean(home) {
		return "", false
	}
	return root, true
}

// walkUp returns dir, or the nearest folder above it, for which is reports
// true. It stops at the first folder is can't check.
func walkUp(dir string, is func(string) (bool, error)) (string, bool, error) {
	for {
		ok, err := is(dir)
		if err != nil || ok {
			return dir, ok, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

// isGitRoot reports whether dir holds an entry named .git: a worktree's is a
// file.
func isGitRoot(dir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// manifests are the files a lock file pins.
var manifests = []string{"package.json", "go.mod", "pyproject.toml", "Cargo.toml", "Gemfile", "composer.json"}

// isPackageRoot reports whether dir directly holds a lock file and a manifest.
func isPackageRoot(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	file := func(match func(string) bool) bool {
		return slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return !e.IsDir() && match(e.Name()) })
	}
	return file(isLockFile) && file(func(name string) bool { return slices.Contains(manifests, name) }), nil
}

// isLockFile reports whether name is a package manager's lock file.
// skills-lock.json pins an agent's skills, not a project's dependencies.
func isLockFile(name string) bool {
	switch name {
	case "go.sum":
		return true
	case "skills-lock.json":
		return false
	}
	for _, s := range lockSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Find returns the compose files under root, best first, as absolute paths.
// It looks maxDepth folders down at most, and never in hidden folders,
// node_modules, vendor or testdata. Files for development come first, then the plain
// file, then other variants, and files for production last; a tie goes to
// the shallower file, then to the path. A folder it can't read is skipped, and
// so is a compose name that isn't a regular file once symlinks are followed.
func Find(root string) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("find compose files: %w", err)
	}
	type match struct {
		path        string
		rank, depth int
	}
	var found []match
	// os.DirFS opens root through a symlink, so a project reached through a
	// link is searched. WalkDir follows no symlink below root.
	fsys := os.DirFS(root)
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		depth := strings.Count(p, "/")
		switch {
		case p == ".":
			return err
		case err != nil:
			return nil // a folder it can't read
		case d.IsDir():
			if depth >= maxDepth || skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		m := fileNameRE.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		// A symlink to a compose file counts; one to a folder, a device or
		// a FIFO doesn't, since reading it fails or never ends.
		if info, err := fs.Stat(fsys, p); err == nil && info.Mode().IsRegular() {
			found = append(found, match{path: filepath.Join(root, filepath.FromSlash(p)), rank: rank(m[1]), depth: depth})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("find compose files in %s: %w", root, err)
	}
	slices.SortFunc(found, func(a, b match) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.depth, b.depth), strings.Compare(a.path, b.path))
	})
	paths := make([]string, len(found))
	for i, m := range found {
		paths[i] = m.path
	}
	return paths, nil
}

// rank orders compose files by variant: the ones for development first, then
// the plain file, then any other, and the ones for production last.
func rank(variant string) int {
	switch strings.ToLower(variant) {
	case "dev", "develop", "development", "local":
		return 0
	case "":
		return 1
	case "prod", "production":
		return 3
	default:
		return 2
	}
}

// skipDir reports whether Find stays out of a folder: a hidden one, one a
// package manager fills, or Go's testdata, whose files are fixtures.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata"
}

// ProjectDir returns the project the file at path belongs to (ProjectRoot), or
// the file's folder outside one.
func ProjectDir(path string) string {
	if root, ok := ProjectRoot(filepath.Dir(path)); ok {
		return root
	}
	return filepath.Dir(path)
}
