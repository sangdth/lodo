package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

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
// files, restarts dnsmasq when its config changed or it stopped, runs the
// resolver script through sudo (which also flushes the DNS cache), and
// restarts Caddy when its sites changed or it stopped. A service turned off with SetService stays off; with dnsmasq off
// the script gets no names, so no /etc/resolver file points at its port.
// Before setup it returns ErrNotSetUp and touches nothing. The caller probes
// the names afterward.
func Apply(ctx context.Context, p paths.Paths, r run.Runner, domains []store.Domain) error {
	if !SetupDone(p) {
		return ErrNotSetUp
	}
	changed, err := WriteFiles(p, domains)
	if err != nil {
		return err
	}
	names := ""
	if st := brew.Info(ctx, r, p, "dnsmasq"); !st.Off() {
		if !st.Running || slices.Contains(changed, p.DnsmasqConf) {
			if err := brew.Restart(ctx, r, p, "dnsmasq"); err != nil {
				return err
			}
		}
		names = dnsmasq.ResolverList(domains)
	}
	if err := runScript(ctx, p, r, names); err != nil {
		return err
	}
	return applyCaddy(ctx, p, r, domains, slices.Contains(changed, p.Caddyfile))
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
	needed := slices.ContainsFunc(domains, store.Domain.HasSite)
	if !CaddySetUp(p) {
		if needed {
			return errCaddyNotSetUp
		}
		return nil
	}
	st := brew.Info(ctx, r, p, "caddy")
	if st.Off() {
		return nil // turned off: it reads the new Caddyfile when it is turned on
	}
	if !changed && (!needed || st.Running) {
		return nil
	}
	return restartCaddy(ctx, p, r)
}

// caddySettle is how long restartCaddy waits before it checks Caddy still
// runs. brew.Restart returns once launchd starts the job; a Caddy
// that can't load its config exits a few milliseconds later, and launchd
// starts it again only every 10 seconds. Tests set it to 0.
var caddySettle = time.Second

// restartCaddy validates Homebrew's Caddyfile, restarts Caddy, and returns an
// error with the last error in Caddy's log when Caddy stopped right after it
// started.
func restartCaddy(ctx context.Context, p paths.Paths, r run.Runner) error {
	if err := caddy.Validate(ctx, r, p.Caddy, p.SystemCaddyfile); err != nil {
		return err
	}
	started := time.Now()
	if err := brew.Restart(ctx, r, p, "caddy"); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(caddySettle):
	}
	if brew.Info(ctx, r, p, "caddy").Running {
		return nil
	}
	if msg := caddy.LogError(p.CaddyLog, started); msg != "" {
		return fmt.Errorf("caddy stopped right after it started: %s", msg)
	}
	return fmt.Errorf("caddy stopped right after it started; see %s", p.CaddyLog)
}

// Services are the brew services SetService turns on and off.
var Services = []string{"dnsmasq", "caddy"}

// SetService turns a service on or off. On starts the service and registers it
// to start at login, Caddy through restartCaddy; off stops and
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
		return brew.Stop(ctx, r, p, service)
	}
	if !CaddySetUp(p) {
		return errors.New("caddy is not set up for lodo: brew install caddy, then lodo setup")
	}
	return restartCaddy(ctx, p, r)
}

// setDnsmasq turns dnsmasq off after removing lodo's resolver files, or on
// before writing them for the enabled names in domains.json.
func setDnsmasq(ctx context.Context, p paths.Paths, r run.Runner, on bool) error {
	if !on {
		if err := runScript(ctx, p, r, ""); err != nil {
			return err
		}
		return brew.Stop(ctx, r, p, "dnsmasq")
	}
	domains, err := store.Load(p.DomainsJSON)
	if err != nil {
		return err
	}
	if err := brew.Restart(ctx, r, p, "dnsmasq"); err != nil {
		return err
	}
	return runScript(ctx, p, r, dnsmasq.ResolverList(domains))
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
