package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/system"
)

// module is what lodo is published as; latestTarget asks the toolchain for the
// newest commit on master, the release branch. @latest would follow GitHub's
// default branch instead. installTarget builds the version go list reports.
const module = "github.com/sangdth/lodo"

func latestTarget() string          { return module + "@master" }
func installTarget(v string) string { return module + "/cmd/lodo@" + v }

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
	out, err := a.runner.Run(ctx, goTool, "-C", dir, "list", "-m", "-f", "{{.Version}}", latestTarget())
	if err != nil {
		a.fail(fmt.Errorf("read the latest version: %w", err))
		return 1
	}
	newest := strings.TrimSpace(out)
	if newest == "" {
		a.fail(errors.New("read the latest version: go printed none"))
		return 1
	}
	// shortcut: an equality check, so a checkout built from a commit newer
	// than the published master is replaced by master, and printed as such.
	// Comparing the two needs semver ordering, which only matters once lodo
	// tags releases; until then the dev re-runs go install ./cmd/lodo.
	if newest == cur {
		fmt.Fprintf(a.stdout, "lodo %s is up to date\n", cur)
		return 0
	}
	// The reported version, not @latest: a push between the two commands
	// would build something other than what the line below prints.
	if _, err := a.runner.Run(ctx, goTool, "-C", dir, "install", installTarget(newest)); err != nil {
		a.fail(fmt.Errorf("install lodo %s: %w", newest, err))
		return 1
	}
	fmt.Fprintf(a.stdout, "updated %s -> %s\n", cur, newest)

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
	if table != "" {
		fmt.Fprintf(a.stdout, "\n%s", table) // each failed check prints its own fix
	}
	if err != nil && table == "" {
		fmt.Fprintf(a.stderr, "warn  %v\n", err)
	}
	return 0
}

// installDir returns where go install puts lodo: GOBIN, else the first GOPATH
// entry's bin, else ~/go/bin, which is what Go itself falls back to.
func (a app) installDir(ctx context.Context, goTool, dir string) (string, error) {
	out, err := a.runner.Run(ctx, goTool, "-C", dir, "env", "GOBIN", "GOPATH")
	if err != nil {
		return "", fmt.Errorf("find where go install puts lodo: %w", err)
	}
	lines := strings.Split(out, "\n") // GOBIN, then GOPATH; GOBIN is empty before Go 1.27
	if len(lines) > 0 {
		if gobin := strings.TrimSpace(lines[0]); gobin != "" {
			return gobin, nil
		}
	}
	if len(lines) > 1 {
		if first, _, _ := strings.Cut(strings.TrimSpace(lines[1]), string(os.PathListSeparator)); first != "" {
			return filepath.Join(first, "bin"), nil
		}
	}
	return filepath.Join(a.paths.Home, "go", "bin"), nil
}

// warnShadowed says when lodo runs from another copy than the one just
// installed: the user would start the old binary and think nothing changed.
func (a app) warnShadowed(bin string) {
	self, err := a.execPath()
	if err != nil {
		return
	}
	if installed := filepath.Join(bin, "lodo"); !scan.SameFolder(self, installed) {
		fmt.Fprintf(a.stderr, "warn  lodo runs from %s, not the new %s: put %s on PATH before it\n", self, installed, bin)
	}
}
