package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/sangdth/lodo/internal/brew"
	"github.com/sangdth/lodo/internal/caddy"
	"github.com/sangdth/lodo/internal/dnsmasq"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
)

// ErrNotSetUp means lodo setup has not run. Apply then changes nothing: it
// would start dnsmasq against a config that does not include lodo's.
var ErrNotSetUp = errors.New("lodo is not set up: run lodo setup")

// Apply makes the running system match domains: it writes the generated
// files, restarts dnsmasq, runs the resolver script through sudo (which also
// flushes the DNS cache), and restarts Caddy when its sites changed or it
// stopped. A service turned off with SetService stays off; with dnsmasq off
// the script gets no names, so no /etc/resolver file points at its port.
// Before setup it returns ErrNotSetUp and touches nothing. The caller probes
// the names afterward.
func Apply(ctx context.Context, p paths.Paths, r run.Runner, domains []store.Domain) error {
	if !SetupDone(p) {
		return ErrNotSetUp
	}
	changes, err := WriteFiles(p, domains)
	if err != nil {
		return err
	}
	names := ""
	if !turnedOff(ctx, p, r, "dnsmasq") {
		if err := brew.Restart(ctx, r, p.Brew, "dnsmasq"); err != nil {
			return err
		}
		names = dnsmasq.ResolverList(domains)
	}
	if err := runScript(ctx, p, r, names); err != nil {
		return err
	}
	return applyCaddy(ctx, p, r, domains, changes.Caddy)
}

// runScript runs the resolver script through sudo with names, as
// dnsmasq.ResolverList writes them, on its standard input. An empty names
// removes every file the script wrote.
func runScript(ctx context.Context, p paths.Paths, r run.Runner, names string) error {
	if _, err := r.RunInput(ctx, names, p.Sudo, "-n", p.Script); err != nil {
		return fmt.Errorf("update %s: %w", p.ResolverDir, err)
	}
	return nil
}

// errCaddyNotSetUp means a domain has a port but setup never pointed
// Homebrew's Caddy at lodo's Caddyfile.
var errCaddyNotSetUp = errors.New("a domain has a port but Caddy is not set up for lodo: brew install caddy, then lodo setup")

func applyCaddy(ctx context.Context, p paths.Paths, r run.Runner, domains []store.Domain, changed bool) error {
	needed := slices.ContainsFunc(domains, func(d store.Domain) bool { return d.Enabled && d.Port > 0 })
	if !CaddySetUp(p) {
		if needed {
			return errCaddyNotSetUp
		}
		return nil
	}
	st, err := brew.Info(ctx, r, p.Brew, "caddy")
	if err == nil && st.Off() {
		return nil // turned off: it reads the new Caddyfile when it is turned on
	}
	if !changed {
		if !needed {
			return nil
		}
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

// Services are the brew services SetService turns on and off.
var Services = []string{"dnsmasq", "caddy"}

// SetService turns a service on or off. On validates Caddy's config first,
// then starts the service and registers it to start at login; off stops and
// unregisters it, which Apply and lodo doctor read as turned off. dnsmasq's
// resolver files follow it: off removes them before it stops, so no account's
// .test lookups go to a port nobody holds, and on writes them once it runs.
func SetService(ctx context.Context, p paths.Paths, r run.Runner, service string, on bool) error {
	if !SetupDone(p) {
		return ErrNotSetUp
	}
	if !slices.Contains(Services, service) {
		return fmt.Errorf("unknown service %q", service)
	}
	if service == "dnsmasq" {
		return setDnsmasq(ctx, p, r, on)
	}
	if !on {
		return brew.Stop(ctx, r, p.Brew, service)
	}
	if service == "caddy" {
		if !CaddySetUp(p) {
			return errors.New("caddy is not set up for lodo: brew install caddy, then lodo setup")
		}
		if err := caddy.Validate(ctx, r, p.Caddy, p.SystemCaddyfile); err != nil {
			return err
		}
	}
	return brew.Restart(ctx, r, p.Brew, service)
}

// setDnsmasq turns dnsmasq off after removing lodo's resolver files, or on
// before writing them for the enabled names in domains.json.
func setDnsmasq(ctx context.Context, p paths.Paths, r run.Runner, on bool) error {
	if !on {
		if err := runScript(ctx, p, r, ""); err != nil {
			return err
		}
		return brew.Stop(ctx, r, p.Brew, "dnsmasq")
	}
	domains, err := store.Load(p.DomainsJSON)
	if err != nil {
		return err
	}
	if err := brew.Restart(ctx, r, p.Brew, "dnsmasq"); err != nil {
		return err
	}
	return runScript(ctx, p, r, dnsmasq.ResolverList(domains))
}

// turnedOff reports whether service was turned off. When brew can't say, it
// is not: the restart that follows shows what is wrong.
func turnedOff(ctx context.Context, p paths.Paths, r run.Runner, service string) bool {
	st, err := brew.Info(ctx, r, p.Brew, service)
	return err == nil && st.Off()
}

// SetupDone reports whether lodo setup has run: the resolver script is
// installed and Homebrew's dnsmasq.conf includes lodo's file.
func SetupDone(p paths.Paths) bool {
	return exists(p.Script) && hasLine(p.SystemConf, "conf-file="+p.DnsmasqConf)
}

// CaddySetUp reports whether Homebrew's Caddyfile imports lodo's.
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
