// Package tui is lcd's terminal UI: the domain list with each name's checks,
// a status bar for the system parts, and the keys that change the list.
package tui

import (
	"context"

	"github.com/sangdth/lcd/internal/check"
	"github.com/sangdth/lcd/internal/paths"
	"github.com/sangdth/lcd/internal/run"
	"github.com/sangdth/lcd/internal/store"
	"github.com/sangdth/lcd/internal/system"
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

func (b backend) Report(ctx context.Context, domains []store.Domain) ([]check.Check, []check.Result) {
	return b.env.Report(ctx, domains)
}
