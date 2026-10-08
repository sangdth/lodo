// Package tui is oo's terminal UI: the domain list with each name's checks,
// a status bar for the system parts, and the keys that change the list.
package tui

import (
	"context"
	"errors"
	"os"

	"github.com/sangdth/oo/internal/check"
	"github.com/sangdth/oo/internal/compose"
	"github.com/sangdth/oo/internal/dnsmasq"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/run"
	"github.com/sangdth/oo/internal/store"
	"github.com/sangdth/oo/internal/system"
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
	// Copy puts text on the clipboard.
	Copy(ctx context.Context, text string) error
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

// PortsReady needs Caddy installed and Homebrew's Caddyfile importing oo's.
func (b backend) PortsReady() error {
	if _, err := os.Stat(b.paths.Caddy); err != nil {
		return errors.New("caddy is not installed: brew install caddy, then oo setup")
	}
	if !system.CaddySetUp(b.paths) {
		return errors.New("caddy is not set up for oo: run oo setup")
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
	return os.ReadFile(path) //nolint:gosec // G304: the user named this compose file
}

func (b backend) Copy(ctx context.Context, text string) error {
	_, err := b.runner.RunInput(ctx, text, b.paths.Pbcopy)
	return err
}

func (b backend) Tail(offset int64) (string, int64, error) { return dnsmasq.Tail(b.paths.Log, offset) }
