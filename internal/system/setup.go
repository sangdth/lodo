package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sangdth/lodo/internal/brew"
	"github.com/sangdth/lodo/internal/caddy"
	"github.com/sangdth/lodo/internal/fsutil"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
)

// DnsmasqSystemLabels are the launchd labels a root dnsmasq job started with
// `sudo brew services` can have. Such a job shadows lodo's user job.
var DnsmasqSystemLabels = []string{"homebrew.mxcl.dnsmasq", "sh.brew.dnsmasq"}

// Setup installs everything lodo needs and asks for the password once, before
// it changes anything. It prints one line per step and stops at the first
// failure, which it returns as a *StepError. Running it again repairs and
// updates an existing setup.
func Setup(ctx context.Context, p paths.Paths, r run.Runner, out io.Writer) error {
	s := steps{out: out}
	if err := s.do("Homebrew and dnsmasq are installed", "brew install dnsmasq", func() (string, error) {
		return "", preflight(p)
	}); err != nil {
		return err
	}
	if _, err := Sudoers(p); err != nil {
		return &StepError{Step: "check your user name", Err: err, Fix: "run lodo from an account with a plain short name"}
	}

	fmt.Fprintln(out, "lodo needs your password once, to install its root parts.")
	if err := r.RunTTY(ctx, p.Sudo, "-v"); err != nil {
		return &StepError{Step: "admin password", Err: err, Fix: "run lodo setup again and enter your password"}
	}

	if err := os.MkdirAll(p.Staging, 0o700); err != nil {
		return &StepError{Step: "create " + p.Staging, Err: err, Fix: "check ~/.config is writable"}
	}
	defer func() { _ = os.RemoveAll(p.Staging) }() // only holds copies of installed files

	for _, step := range []struct {
		name, fix string
		fn        func() (string, error)
	}{
		{"lodo's files in " + p.ConfigDir, "fix or remove " + p.DomainsJSON, func() (string, error) { return "", userFiles(p) }},
		{"Homebrew's dnsmasq.conf includes lodo's", "check " + p.SystemConf + " is writable", func() (string, error) { return systemConf(p) }},
		{"no root dnsmasq job", "sudo brew services stop dnsmasq", func() (string, error) { return stopSystemDnsmasq(ctx, p, r) }},
		{"resolver script installed", "lodo setup", func() (string, error) { return "", installScript(ctx, p, r) }},
		{"sudoers rule installed", "lodo setup", func() (string, error) { return "", installSudoers(ctx, p, r) }},
		{"loopback addresses " + store.OwnAddress(store.OwnFirst) + "–" + store.OwnAddress(store.OwnLast) + " on lo0", "lodo setup", func() (string, error) { return "", installLoopback(ctx, p, r) }},
		{"no " + filepath.Join(p.ResolverDir, "local"), "sudo rm " + filepath.Join(p.ResolverDir, "local"), func() (string, error) { return removeResolverLocal(ctx, p, r) }},
		{"dnsmasq runs as you on port 53535", "brew services restart dnsmasq", func() (string, error) {
			return "", brew.Restart(ctx, r, p.Brew, "dnsmasq")
		}},
		{"Caddy serves lodo's sites on port 80", "brew services restart caddy", func() (string, error) { return "", setupCaddy(ctx, p, r) }},
		{"Caddy's local CA trusted for HTTPS names", "caddy trust", func() (string, error) { return "", trustCaddy(ctx, p, r) }},
		{"sudo runs the resolver script without a password", "lodo setup", func() (string, error) {
			_, err := r.Run(ctx, p.Sudo, "-n", "-k", p.Script)
			return "", err
		}},
	} {
		if err := s.do(step.name, step.fix, step.fn); err != nil {
			return err
		}
	}

	var left []string
	for _, l := range p.Leftovers {
		if exists(l) {
			left = append(left, l)
		}
	}
	if len(left) > 0 {
		fmt.Fprintln(out, "Left alone, from LocalDNS (delete them by hand when you no longer need it):")
		for _, l := range left {
			s.note("%s", l)
		}
	}
	return nil
}

func preflight(p paths.Paths) error {
	for _, f := range []struct{ path, what string }{
		{p.Brew, "Homebrew at /opt/homebrew"},
		{p.Dnsmasq, "dnsmasq"},
		{p.SystemConf, "Homebrew's dnsmasq.conf"},
	} {
		if !exists(f.path) {
			return fmt.Errorf("%s not found at %s", f.what, f.path)
		}
	}
	return nil
}

func userFiles(p paths.Paths) error {
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil { //nolint:gosec // G301: ~/.config/lodo holds no secrets
		return fmt.Errorf("create %s: %w", p.ConfigDir, err)
	}
	if !exists(p.DomainsJSON) {
		if err := store.Save(p.DomainsJSON, nil); err != nil {
			return err
		}
	}
	domains, err := store.Load(p.DomainsJSON)
	if err != nil {
		return err
	}
	_, err = WriteFiles(p, domains)
	return err
}

// systemConf backs up Homebrew's dnsmasq.conf once, then rewrites it to
// include lodo's block.
func systemConf(p paths.Paths) (string, error) {
	old, err := os.ReadFile(p.SystemConf)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.SystemConf, err)
	}
	note := "backup kept in " + filepath.Base(p.SystemConfBackup)
	if !exists(p.SystemConfBackup) {
		if _, err := fsutil.WriteFile(p.SystemConfBackup, old, 0o644); err != nil {
			return "", err
		}
		note = "backup in " + filepath.Base(p.SystemConfBackup)
	}
	if _, err := fsutil.WriteFile(p.SystemConf, []byte(RewriteSystemConf(string(old), p)), 0o644); err != nil {
		return "", err
	}
	return note, nil
}

// stopSystemDnsmasq boots out and removes a root dnsmasq job.
func stopSystemDnsmasq(ctx context.Context, p paths.Paths, r run.Runner) (string, error) {
	var done []string
	for _, label := range DnsmasqSystemLabels {
		if _, err := r.Run(ctx, p.Launchctl, "print", "system/"+label); err == nil {
			if _, err := r.Run(ctx, p.Sudo, "-n", p.Launchctl, "bootout", "system/"+label); err != nil {
				return "", err
			}
			done = append(done, "stopped system/"+label)
		}
		plist := filepath.Join(p.LaunchDaemons, label+".plist")
		if exists(plist) {
			if _, err := r.Run(ctx, p.Sudo, "-n", p.Rm, "-f", plist); err != nil {
				return "", err
			}
			done = append(done, "removed "+plist)
		}
	}
	return strings.Join(done, ", "), nil
}

func installScript(ctx context.Context, p paths.Paths, r run.Runner) error {
	if _, err := r.Run(ctx, p.Sudo, "-n", p.Install, "-d", "-o", "root", "-g", "wheel", "-m", "755", filepath.Dir(p.Script)); err != nil {
		return err
	}
	return installFile(ctx, p, r, Script(p), p.Script, "755")
}

func installSudoers(ctx context.Context, p paths.Paths, r run.Runner) error {
	rule, err := Sudoers(p)
	if err != nil {
		return err
	}
	staged, err := stage(p, p.Sudoers, rule)
	if err != nil {
		return err
	}
	if _, err := r.Run(ctx, p.Visudo, "-c", "-f", staged); err != nil {
		return fmt.Errorf("the rendered rule failed visudo, nothing installed: %w", err)
	}
	_, err = r.Run(ctx, p.Sudo, "-n", p.Install, "-o", "root", "-g", "wheel", "-m", "440", staged, p.Sudoers)
	return err
}

func installLoopback(ctx context.Context, p paths.Paths, r run.Runner) error {
	if err := installFile(ctx, p, r, LoopbackPlist(), p.LoopbackPlist, "644"); err != nil {
		return err
	}
	// bootout fails when the job isn't loaded yet; bootstrap is what counts.
	_, _ = r.Run(ctx, p.Sudo, "-n", p.Launchctl, "bootout", "system/"+LoopbackLabel)
	_, err := r.Run(ctx, p.Sudo, "-n", p.Launchctl, "bootstrap", "system", p.LoopbackPlist)
	return err
}

func removeResolverLocal(ctx context.Context, p paths.Paths, r run.Runner) (string, error) {
	local := filepath.Join(p.ResolverDir, "local")
	if !exists(local) {
		return "", nil
	}
	if _, err := r.Run(ctx, p.Sudo, "-n", p.Rm, "-f", local); err != nil {
		return "", err
	}
	return "removed it: it sent every .local name away from Bonjour", nil
}

// setupCaddy points Homebrew's Caddyfile at lodo's and restarts Caddy. Without
// Caddy installed it skips: only domains with a port need it.
func setupCaddy(ctx context.Context, p paths.Paths, r run.Runner) error {
	if !exists(p.Caddy) {
		return skipped("Caddy is not installed; domains with a port need it: brew install caddy, then lodo setup")
	}
	old, err := os.ReadFile(p.SystemCaddyfile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", p.SystemCaddyfile, err)
	}
	if err == nil && !exists(p.SystemCaddyfileBackup) {
		if _, err := fsutil.WriteFile(p.SystemCaddyfileBackup, old, 0o644); err != nil {
			return err
		}
	}
	if _, err := fsutil.WriteFile(p.SystemCaddyfile, []byte(RewriteSystemCaddyfile(string(old), p)), 0o644); err != nil {
		return err
	}
	if err := caddy.Validate(ctx, r, p.Caddy, p.SystemCaddyfile); err != nil {
		return err
	}
	return brew.Restart(ctx, r, p.Brew, "caddy")
}

// trustCaddy adds Caddy's local root certificate to the System keychain, so
// browsers trust the certificates tls internal makes. caddy trust fetches the
// root from Caddy's admin API, so Caddy must be running, and runs sudo itself,
// which Setup's sudo -v already covered. It skips unless an enabled domain has
// HTTPS on.
func trustCaddy(ctx context.Context, p paths.Paths, r run.Runner) error {
	domains, err := store.Load(p.DomainsJSON)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(domains, func(d store.Domain) bool { return d.Enabled && d.HTTPS }) {
		return skipped("no enabled name has HTTPS on")
	}
	if !exists(p.Caddy) {
		return skipped("Caddy is not installed; names with HTTPS on need it: brew install caddy, then lodo setup")
	}
	// Setup has just restarted Caddy, and its admin API may not listen yet.
	for try := 1; ; try++ {
		err := r.RunTTY(ctx, p.Caddy, "trust")
		if err == nil || try == trustTries {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(trustWait):
		}
	}
}

// caddy trust gets trustTries tries, trustWait apart, while Caddy starts.
const (
	trustTries = 5
	trustWait  = 400 * time.Millisecond
)

// installFile stages content and installs it as root with mode.
func installFile(ctx context.Context, p paths.Paths, r run.Runner, content, target, mode string) error {
	staged, err := stage(p, target, content)
	if err != nil {
		return err
	}
	_, err = r.Run(ctx, p.Sudo, "-n", p.Install, "-o", "root", "-g", "wheel", "-m", mode, staged, target)
	return err
}

// stage writes content to the staging directory under target's base name.
func stage(p paths.Paths, target, content string) (string, error) {
	staged := filepath.Join(p.Staging, filepath.Base(target))
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("stage %s: %w", filepath.Base(target), err)
	}
	return staged, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
