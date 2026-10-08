package system

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sangdth/oo/internal/brew"
	"github.com/sangdth/oo/internal/fsutil"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/run"
	"github.com/sangdth/oo/internal/store"
)

// Uninstall removes everything Setup installed and puts Homebrew's configs
// back. It keeps ~/.config/oo, so domains.json survives, and leaves dnsmasq
// stopped. Every step tolerates a part that is already gone.
func Uninstall(ctx context.Context, p paths.Paths, r run.Runner, out io.Writer) error {
	s := steps{out: out}
	fmt.Fprintln(out, "oo needs your password to remove its root parts.")
	if err := r.RunTTY(ctx, p.Sudo, "-v"); err != nil {
		return &StepError{Step: "admin password", Err: err, Fix: "run oo uninstall again and enter your password"}
	}
	for _, step := range []struct {
		name, fix string
		fn        func() (string, error)
	}{
		{"oo's /etc/resolver files removed", "oo uninstall", func() (string, error) { return "", removeResolverFiles(ctx, p, r) }},
		{"dnsmasq stopped", "brew services stop dnsmasq", func() (string, error) { return "", brew.Stop(ctx, r, p.Brew, "dnsmasq") }},
		{"Homebrew's dnsmasq.conf restored", "edit " + p.SystemConf + " by hand", func() (string, error) { return restoreSystemConf(p) }},
		{"Caddy no longer serves oo's sites", "edit " + p.SystemCaddyfile + " by hand", func() (string, error) { return restoreCaddy(ctx, p, r) }},
		{"loopback addresses removed", "sudo launchctl bootout system/" + LoopbackLabel, func() (string, error) { return "", removeLoopback(ctx, p, r) }},
		{"sudoers rule and resolver script removed", "sudo rm " + p.Sudoers + " '" + p.Script + "'", func() (string, error) { return "", removeRootFiles(ctx, p, r) }},
	} {
		if err := s.do(step.name, step.fix, step.fn); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Kept %s. dnsmasq is stopped; to run it as root on port 53 again: sudo brew services start dnsmasq\n", p.ConfigDir)
	return nil
}

// removeResolverFiles empties the resolver list and runs the script, which
// deletes every file it wrote and flushes the cache.
func removeResolverFiles(ctx context.Context, p paths.Paths, r run.Runner) error {
	if !exists(p.Script) {
		return nil
	}
	if _, err := fsutil.WriteFile(p.Resolvers, nil, 0o644); err != nil {
		return err
	}
	_, err := r.Run(ctx, p.Sudo, "-n", p.Script)
	return err
}

func restoreSystemConf(p paths.Paths) (string, error) {
	if exists(p.SystemConfBackup) {
		if err := os.Rename(p.SystemConfBackup, p.SystemConf); err != nil {
			return "", fmt.Errorf("restore %s: %w", p.SystemConf, err)
		}
		return "from " + filepath.Base(p.SystemConfBackup), nil
	}
	old, err := os.ReadFile(p.SystemConf)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.SystemConf, err)
	}
	if _, err := fsutil.WriteFile(p.SystemConf, []byte(StripSystemConf(string(old))), 0o644); err != nil {
		return "", err
	}
	return "no backup: removed oo's block", nil
}

func restoreCaddy(ctx context.Context, p paths.Paths, r run.Runner) (string, error) {
	if !CaddySetUp(p) {
		return "", nil
	}
	if err := brew.Stop(ctx, r, p.Brew, "caddy"); err != nil {
		return "", err
	}
	if exists(p.SystemCaddyfileBackup) {
		if err := os.Rename(p.SystemCaddyfileBackup, p.SystemCaddyfile); err != nil {
			return "", fmt.Errorf("restore %s: %w", p.SystemCaddyfile, err)
		}
		return "stopped Caddy, restored " + filepath.Base(p.SystemCaddyfile), nil
	}
	old, err := os.ReadFile(p.SystemCaddyfile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p.SystemCaddyfile, err)
	}
	rest := StripSystemCaddyfile(string(old), p)
	if strings.TrimSpace(rest) == "" {
		if err := os.Remove(p.SystemCaddyfile); err != nil {
			return "", fmt.Errorf("remove %s: %w", p.SystemCaddyfile, err)
		}
		return "stopped Caddy, removed oo's " + filepath.Base(p.SystemCaddyfile), nil
	}
	if _, err := fsutil.WriteFile(p.SystemCaddyfile, []byte(rest), 0o644); err != nil {
		return "", err
	}
	return "stopped Caddy, removed oo's import", nil
}

// removeLoopback unloads the job and takes the own block off lo0. Missing
// pieces are fine: ifconfig fails for an address that isn't there.
func removeLoopback(ctx context.Context, p paths.Paths, r run.Runner) error {
	_, _ = r.Run(ctx, p.Sudo, "-n", p.Launchctl, "bootout", "system/"+LoopbackLabel)
	if _, err := r.Run(ctx, p.Sudo, "-n", p.Rm, "-f", p.LoopbackPlist); err != nil {
		return err
	}
	for i := store.OwnFirst; i <= store.OwnLast; i++ {
		_, _ = r.Run(ctx, p.Sudo, "-n", p.Ifconfig, "lo0", "-alias", store.OwnAddress(i))
	}
	return nil
}

func removeRootFiles(ctx context.Context, p paths.Paths, r run.Runner) error {
	if _, err := r.Run(ctx, p.Sudo, "-n", p.Rm, "-f", p.Sudoers, p.Script); err != nil {
		return err
	}
	// rmdir refuses a directory that isn't empty, which is the safe outcome.
	_, _ = r.Run(ctx, p.Sudo, "-n", p.Rmdir, filepath.Dir(p.Script))
	return nil
}
