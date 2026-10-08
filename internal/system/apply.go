package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/sangdth/oo/internal/brew"
	"github.com/sangdth/oo/internal/caddy"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/run"
	"github.com/sangdth/oo/internal/store"
)

// ErrNotSetUp means oo setup has not run. Apply then changes nothing: it
// would start dnsmasq against a config that does not include oo's.
var ErrNotSetUp = errors.New("oo is not set up: run oo setup")

// Apply makes the running system match domains: it writes the generated
// files, restarts dnsmasq, runs the resolver script through sudo (which also
// flushes the DNS cache), and restarts Caddy when its sites changed or it
// stopped. Before setup it returns ErrNotSetUp and touches nothing. The
// caller probes the names afterward.
func Apply(ctx context.Context, p paths.Paths, r run.Runner, domains []store.Domain) error {
	if !SetupDone(p) {
		return ErrNotSetUp
	}
	changes, err := WriteFiles(p, domains)
	if err != nil {
		return err
	}
	if err := brew.Restart(ctx, r, p.Brew, "dnsmasq"); err != nil {
		return err
	}
	if _, err := r.Run(ctx, p.Sudo, "-n", p.Script); err != nil {
		return fmt.Errorf("update %s: %w", p.ResolverDir, err)
	}
	return applyCaddy(ctx, p, r, domains, changes.Caddy)
}

// errCaddyNotSetUp means a domain has a port but setup never pointed
// Homebrew's Caddy at oo's Caddyfile.
var errCaddyNotSetUp = errors.New("a domain has a port but Caddy is not set up for oo: brew install caddy, then oo setup")

func applyCaddy(ctx context.Context, p paths.Paths, r run.Runner, domains []store.Domain, changed bool) error {
	needed := slices.ContainsFunc(domains, func(d store.Domain) bool { return d.Enabled && d.Port > 0 })
	if !CaddySetUp(p) {
		if needed {
			return errCaddyNotSetUp
		}
		return nil
	}
	if !changed {
		if !needed {
			return nil
		}
		st, err := brew.Info(ctx, r, p.Brew, "caddy")
		if err != nil {
			return err
		}
		if st.Running {
			return nil
		}
	}
	if err := caddy.Validate(ctx, r, p.Caddy, p.SystemCaddyfile); err != nil {
		return err
	}
	return brew.Restart(ctx, r, p.Brew, "caddy")
}

// SetupDone reports whether oo setup has run: the resolver script is
// installed and Homebrew's dnsmasq.conf includes oo's file.
func SetupDone(p paths.Paths) bool {
	return exists(p.Script) && hasLine(p.SystemConf, "conf-file="+p.DnsmasqConf)
}

// CaddySetUp reports whether Homebrew's Caddyfile imports oo's.
func CaddySetUp(p paths.Paths) bool {
	return hasLine(p.SystemCaddyfile, "import "+p.Caddyfile)
}

// hasLine reports whether the file at path has a line that, trimmed, is want.
func hasLine(path, want string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path comes from paths.Paths
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(data)) {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
