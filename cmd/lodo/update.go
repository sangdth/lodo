package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sangdth/lodo/internal/system"
)

// module is what lodo is published as, and latestPkg the package go install
// builds: the one the README tells end users to install.
const (
	module    = "github.com/sangdth/lodo"
	latestPkg = module + "/cmd/lodo@latest"
)

// updateTimeout bounds go list and go install: on a cold module cache they
// download and compile for longer than run.Exec's default 30 seconds, which
// applies to a context with no deadline.
const updateTimeout = 5 * time.Minute

// update installs the latest published lodo through the Go toolchain that
// installed it, and says when it is already current. It refuses a build from a
// modified checkout, which a release would replace without the change the
// user is working on. Once the new binary is in place it warns when another
// copy comes first on PATH, and shows the new binary's doctor, whose fixes are
// what a changed root script or Caddyfile needs; the update itself succeeded.
func (a app) update(ctx context.Context) int {
	cur := a.version()
	if strings.HasSuffix(cur, "+dirty") {
		a.fail(errors.New("this lodo was built from a modified checkout: go install ./cmd/lodo from the repo"))
		return 1
	}
	goTool, err := a.lookPath("go")
	if err != nil {
		a.fail(errors.New("lodo updates through Go, the toolchain it was installed with: brew install go, then lodo update"))
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	// -C keeps Go from reading the go.mod or go.work of the folder lodo runs
	// in: it need not parse, and may ask for a newer Go.
	dir := os.TempDir()
	out, err := a.runner.Run(ctx, goTool, "-C", dir, "list", "-m", "-f", "{{.Version}}", module)
	if err != nil {
		a.fail(fmt.Errorf("read the latest version: %w", err))
		return 1
	}
	latest := strings.TrimSpace(out)
	if latest == "" {
		a.fail(errors.New("read the latest version: go printed none"))
		return 1
	}
	// shortcut: an equality check, so a checkout built from a commit newer
	// than the published master is replaced by master, and printed as such.
	// Comparing the two needs semver ordering, which only matters once lodo
	// tags releases; until then the dev re-runs go install ./cmd/lodo.
	if latest == cur {
		fmt.Fprintf(a.stdout, "lodo %s is up to date\n", cur)
		return 0
	}
	if _, err := a.runner.Run(ctx, goTool, "-C", dir, "install", latestPkg); err != nil {
		a.fail(fmt.Errorf("install lodo %s: %w", latest, err))
		return 1
	}
	fmt.Fprintf(a.stdout, "updated %s -> %s\n", cur, latest)

	bin, err := a.installDir(ctx, goTool, dir)
	if err != nil {
		fmt.Fprintf(a.stderr, "warn  %v\n", err)
		return 0
	}
	a.warnShadowed(bin)
	if !system.SetupDone(a.paths) {
		return 0
	}
	table, err := a.runner.Run(ctx, filepath.Join(bin, "lodo"), "doctor")
	fmt.Fprint(a.stdout, table)
	if err != nil {
		fmt.Fprintln(a.stderr, "note  some checks need a fix above: lodo setup or lodo apply")
	}
	return 0
}

// installDir returns where go install puts lodo: GOBIN, else the first GOPATH
// entry's bin, else ~/go/bin, which is what Go itself falls back to.
func (a app) installDir(ctx context.Context, goTool, dir string) (string, error) {
	goenv := func(name string) (string, error) {
		out, err := a.runner.Run(ctx, goTool, "-C", dir, "env", name)
		if err != nil {
			return "", fmt.Errorf("read go's %s: %w", strings.ToLower(name), err)
		}
		return strings.TrimSpace(out), nil
	}
	gobin, err := goenv("GOBIN")
	if err != nil {
		return "", err
	}
	if gobin != "" {
		return gobin, nil
	}
	gopath, err := goenv("GOPATH")
	if err != nil {
		return "", err
	}
	if first, _, _ := strings.Cut(gopath, string(os.PathListSeparator)); first != "" {
		return filepath.Join(first, "bin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find your home folder: %w", err)
	}
	return filepath.Join(home, "go", "bin"), nil
}

// warnShadowed says when lodo runs from another copy than the one just
// installed: the user would start the old binary and think nothing changed.
func (a app) warnShadowed(bin string) {
	self, err := a.execPath()
	if err != nil {
		return
	}
	if self, installed := resolved(self), resolved(filepath.Join(bin, "lodo")); self != installed {
		fmt.Fprintf(a.stderr, "warn  lodo runs from %s, not the new %s: put %s on PATH before it\n", self, installed, bin)
	}
}

// resolved returns path with symlinks followed, or cleaned when it is gone.
func resolved(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}
