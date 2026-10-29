// Package tui is lodo's terminal UI: the domain list with each name's checks,
// a status bar for the system parts, the keys that change the list, and the
// setup form for a project, which lodo dev also runs on its own.
package tui

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/sangdth/lodo/internal/check"
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
	// Gone reports whether nothing is at path any more, such as a project's
	// folder since it was renamed or moved.
	Gone(path string) bool
	// SetService turns dnsmasq or Caddy on or off.
	SetService(ctx context.Context, service string, on bool) error
	// Scan reads the project dir is in and proposes what the list lacks for
	// it, under name when the user chose one; ok is false outside a project.
	Scan(ctx context.Context, dir, name string, domains []store.Domain) (p scan.Project, proposal scan.Proposal, ok bool)
	// ReadCompose returns the compose file at path.
	ReadCompose(path string) ([]byte, error)
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

func (b backend) Gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func (b backend) Scan(ctx context.Context, dir, name string, domains []store.Domain) (scan.Project, scan.Proposal, bool) {
	p, ok := scan.Find(dir)
	if !ok {
		return scan.Project{}, scan.Proposal{}, false
	}
	return p, scan.Propose(p, domains, name, b.Gone), true
}

func (b backend) ReadCompose(path string) ([]byte, error) {
	return fsutil.ReadRegular(path)
}

func (b backend) Tail(offset int64) (string, int64, error) { return dnsmasq.Tail(b.paths.Log, offset) }
