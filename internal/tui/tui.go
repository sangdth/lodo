// Package tui is lodo's terminal UI: the domain list with each name's checks,
// a status bar for the system parts, and the keys that change the list.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sangdth/lodo/internal/check"
	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/dnsmasq"
	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/scan"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

// Backend is what the TUI reads, changes and checks. NewBackend returns the
// one that works on this Mac; tests use a fake.
type Backend interface {
	// Load reads domains.json.
	Load() ([]store.Domain, error)
	// Save writes domains.json.
	Save(domains []store.Domain) error
	// Apply makes the system match domains.
	Apply(ctx context.Context, domains []store.Domain) error
	// Report runs the doctor checks and probes every enabled domain.
	Report(ctx context.Context, domains []store.Domain) ([]check.Check, []check.Result)
	// PortsReady says why a domain with a port can't work yet, or returns nil.
	PortsReady() error
	// SetService turns dnsmasq or Caddy on or off.
	SetService(ctx context.Context, service string, on bool) error
	// Scan reads the project dir is in and proposes what the list lacks for
	// it, under name when the user chose one and with the labels the user gave
	// its apps; ok is false outside a project.
	Scan(ctx context.Context, dir, name string, labels map[string]string, domains []store.Domain) (
		p scan.Project, proposal scan.Proposal, ok bool)
	// ReadCompose returns the compose file at path.
	ReadCompose(path string) ([]byte, error)
	// LinkEnv writes DOCKER_HOST_IP=address into the .env the compose file at
	// composePath runs with, and returns that file.
	LinkEnv(ctx context.Context, composePath, address string) (string, error)
	// FixEnv writes the scan's .env fixes in the project at root, with
	// LinkEnv's guards; a line that changed since the scan is refused. Errors
	// from each file are joined.
	FixEnv(ctx context.Context, root string, fixes []scan.Fix) error
	// HostFixes returns the lines of the project at root that start a dev
	// server on every address, with address added. lodo doesn't write them.
	HostFixes(root, address string) []scan.Fix
	// Tail returns what dnsmasq logged since offset, and the next offset.
	Tail(offset int64) (string, int64, error)
}

type backend struct {
	paths  paths.Paths
	runner run.Runner
	env    check.Env
}

// NewBackend returns the Backend that works through p and r.
func NewBackend(p paths.Paths, r run.Runner) Backend {
	return backend{paths: p, runner: r, env: check.NewEnv(p, r)}
}

func (b backend) Load() ([]store.Domain, error) { return store.Load(b.paths.DomainsJSON) }

func (b backend) Save(domains []store.Domain) error { return store.Save(b.paths.DomainsJSON, domains) }

func (b backend) Apply(ctx context.Context, domains []store.Domain) error {
	return system.Apply(ctx, b.paths, b.runner, domains)
}

func (b backend) SetService(ctx context.Context, service string, on bool) error {
	return system.SetService(ctx, b.paths, b.runner, service, on)
}

func (b backend) Report(ctx context.Context, domains []store.Domain) ([]check.Check, []check.Result) {
	return b.env.Report(ctx, domains)
}

// PortsReady needs Caddy installed and Homebrew's Caddyfile importing lodo's.
func (b backend) PortsReady() error {
	if _, err := os.Stat(b.paths.Caddy); err != nil {
		return errors.New("caddy is not installed: brew install caddy, then lodo setup")
	}
	if !system.CaddySetUp(b.paths) {
		return errors.New("caddy is not set up for lodo: run lodo setup")
	}
	return nil
}

func (b backend) Scan(ctx context.Context, dir, name string, labels map[string]string, domains []store.Domain) (
	scan.Project, scan.Proposal, bool,
) {
	p, ok := scan.Find(dir, func(path string) bool { return system.Tracked(ctx, b.paths, b.runner, path) })
	if !ok {
		return scan.Project{}, scan.Proposal{}, false
	}
	return p, scan.Propose(p, domains, name, labels), true
}

func (b backend) ReadCompose(path string) ([]byte, error) {
	return fsutil.ReadRegular(path, fsutil.MaxProjectFile)
}

// LinkEnv writes DOCKER_HOST_IP=address into the .env the compose file at
// composePath runs with, inside the compose file's project
// (compose.ProjectDir): a package.json script's --env-file or a committed
// symlink could name any file this user can write.
func (b backend) LinkEnv(ctx context.Context, composePath, address string) (string, error) {
	return b.writeEnv(ctx, compose.ProjectDir(composePath), compose.EnvFile(composePath), true,
		func(content []byte) ([]byte, error) { return compose.SetEnv(content, compose.EnvVar, address), nil })
}

func (b backend) FixEnv(ctx context.Context, root string, fixes []scan.Fix) error {
	byFile := map[string][]scan.Fix{}
	for _, f := range fixes {
		byFile[f.File] = append(byFile[f.File], f)
	}
	var errs []error
	for _, file := range slices.Sorted(maps.Keys(byFile)) {
		group := byFile[file]
		_, err := b.writeEnv(ctx, root, filepath.Join(root, filepath.FromSlash(file)), false,
			func(content []byte) ([]byte, error) { return scan.ApplyEnv(content, group) })
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// writeEnv replaces the .env at env with edit's result, and returns that
// file. It writes only inside root once symlinks are followed (envInside),
// and never a file git tracks (system.Tracked): this Mac's addresses and
// names don't belong in a shared file, and a teammate's Mac may not have
// them. It writes the file an in-project symlink points at, through fsutil,
// and keeps the file's mode. A missing file reaches edit as empty when create
// is set, and is an error otherwise.
func (b backend) writeEnv(ctx context.Context, root, env string, create bool, edit func([]byte) ([]byte, error)) (
	string, error,
) {
	env, err := envInside(root, env)
	if err != nil {
		return env, err
	}
	if system.Tracked(ctx, b.paths, b.runner, env) {
		return env, fmt.Errorf("git tracks %s, so lodo leaves it: this Mac's addresses and names don't belong in a shared file", env)
	}
	content, err := fsutil.ReadRegular(env, fsutil.MaxProjectFile)
	mode := fs.FileMode(0o644)
	switch {
	case err == nil:
		if info, statErr := os.Stat(env); statErr == nil {
			mode = info.Mode().Perm()
		}
	case !errors.Is(err, fs.ErrNotExist):
		return env, err // ReadRegular names env
	case !create:
		return env, fmt.Errorf("%s is gone: i scans again", env)
	}
	out, err := edit(content)
	if err != nil {
		return env, err
	}
	if _, err := fsutil.WriteFile(env, out, mode); err != nil {
		return env, fmt.Errorf("write %s: %w", env, err)
	}
	return env, nil
}

// envInside returns env with its symlinks followed, or an error when that
// lies outside root. A .env that doesn't exist yet is resolved through its
// folder.
func envInside(root, env string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return env, fmt.Errorf("resolve %s: %w", root, err)
	}
	real, err := filepath.EvalSymlinks(env)
	if errors.Is(err, fs.ErrNotExist) {
		var dir string
		if dir, err = filepath.EvalSymlinks(filepath.Dir(env)); err == nil {
			real = filepath.Join(dir, filepath.Base(env))
		}
	}
	if err != nil {
		return env, fmt.Errorf("resolve %s: %w", env, err)
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return real, fmt.Errorf("lodo writes only a .env inside the project: %s is outside %s", real, realRoot)
	}
	return real, nil
}

func (b backend) HostFixes(root, address string) []scan.Fix { return scan.HostFixes(root, address) }

func (b backend) Tail(offset int64) (string, int64, error) { return dnsmasq.Tail(b.paths.Log, offset) }
