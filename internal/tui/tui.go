// Package tui is lodo's terminal UI: the domain list with each name's checks,
// a status bar for the system parts, and the keys that change the list.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sangdth/lodo/internal/check"
	"github.com/sangdth/lodo/internal/compose"
	"github.com/sangdth/lodo/internal/dnsmasq"
	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
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
	// Project returns the project dir is in and its compose files, best
	// first; root is empty outside a project.
	Project(dir string) (root string, files []string)
	// ReadCompose returns the compose file at path.
	ReadCompose(path string) ([]byte, error)
	// LinkEnv writes DOCKER_HOST_IP=address into the .env the compose file at
	// composePath runs with, and returns that file.
	LinkEnv(ctx context.Context, composePath, address string) (string, error)
	// NextDev returns the lines of the compose file's project that start
	// next dev on every address, with -H address added. lodo doesn't write them.
	NextDev(composePath, address string) []compose.Fix
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

// Project needs a git root with a lock file. A root it can't search counts as
// having no compose file: the question is only an offer.
func (b backend) Project(dir string) (string, []string) {
	root, ok := compose.ProjectRoot(dir)
	if !ok {
		return "", nil
	}
	files, err := compose.Find(root)
	if err != nil {
		return root, nil
	}
	return root, files
}

func (b backend) ReadCompose(path string) ([]byte, error) {
	return fsutil.ReadRegular(path, fsutil.MaxProjectFile)
}

// LinkEnv writes only a .env inside the compose file's project
// (compose.ProjectDir), once symlinks are followed: a package.json script's
// --env-file or a committed symlink could name any file this user can write.
// It refuses a .env that git tracks: the address belongs to this Mac, and a
// teammate's Mac may not have it. Git runs without the repository's
// fsmonitor and refuses a bare repository it finds on its own, so a cloned
// project can't make it run a command. LinkEnv writes the file an in-project
// symlink points at, and keeps the file's mode.
func (b backend) LinkEnv(ctx context.Context, composePath, address string) (string, error) {
	env, err := envInside(compose.ProjectDir(composePath), compose.EnvFile(composePath))
	if err != nil {
		return env, err
	}
	git := []string{"-c", "safe.bareRepository=explicit", "-c", "core.fsmonitor=false", "-C", filepath.Dir(env),
		"ls-files", "--error-unmatch", "--", filepath.Base(env)}
	if _, err := b.runner.Run(ctx, b.paths.Git, git...); err == nil {
		return env, fmt.Errorf("git tracks %s, so lodo leaves it: this Mac's address doesn't belong in a shared file", env)
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
	}
	if _, err := fsutil.WriteFile(env, compose.SetEnv(content, compose.EnvVar, address), mode); err != nil {
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

func (b backend) NextDev(composePath, address string) []compose.Fix {
	return compose.NextDev(composePath, address)
}

func (b backend) Tail(offset int64) (string, int64, error) { return dnsmasq.Tail(b.paths.Log, offset) }
