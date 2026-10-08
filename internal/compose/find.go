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

// ProjectRoot returns the git root dir is in when it holds a lock file, so oo
// knows it started in a project. ok is false otherwise.
func ProjectRoot(dir string) (root string, ok bool) {
	root, ok = gitRoot(dir)
	if !ok || !hasLockFile(root) {
		return "", false
	}
	return root, true
}

// gitRoot returns dir, or the nearest folder above it, that holds an entry
// named .git: a worktree's is a file.
func gitRoot(dir string) (string, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		_, err = os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if !errors.Is(err, fs.ErrNotExist) || parent == dir {
			return "", false
		}
		dir = parent
	}
}

// hasLockFile reports whether dir directly holds a lock file.
func hasLockFile(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return !e.IsDir() && isLockFile(e.Name()) })
}

// isLockFile reports whether name is a package manager's lock file.
func isLockFile(name string) bool {
	if name == "go.sum" {
		return true
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
// node_modules or vendor. Files for development come first, then the plain
// file, then other variants, and files for production last; a tie goes to
// the shallower file, then to the path. A folder it can't read is skipped.
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
	err = fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
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
		if m := fileNameRE.FindStringSubmatch(d.Name()); m != nil {
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

// skipDir reports whether Find stays out of a folder: a hidden one, or one a
// package manager fills.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor"
}
