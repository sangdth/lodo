package system

import (
	"context"
	"path/filepath"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
)

// Tracked reports whether git tracks the file at path; pass the real path to
// ask about what a symlink points at. Git runs without the repository's
// fsmonitor and refuses a bare repository it finds on its own, so a cloned
// project can't make it run a command. Pathspecs are literal, so a file named
// :x is asked about as :x. A git error counts as untracked.
func Tracked(ctx context.Context, p paths.Paths, r run.Runner, path string) bool {
	_, err := r.Run(ctx, p.Git, "--literal-pathspecs", "-c", "safe.bareRepository=explicit", "-c", "core.fsmonitor=false",
		"-C", filepath.Dir(path), "ls-files", "--error-unmatch", "--", filepath.Base(path))
	return err == nil
}
