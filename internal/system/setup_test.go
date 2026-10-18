package system_test

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

// newMac returns paths under a temp root holding what a Mac with Homebrew
// and dnsmasq has, and a fake runner that finds no root dnsmasq job.
func newMac(t *testing.T, withCaddy bool) (paths.Paths, *run.Fake) {
	t.Helper()
	p := paths.ForTest(t.TempDir())
	writeFile(t, p.Brew, "")
	writeFile(t, p.Dnsmasq, "")
	writeFile(t, p.SystemConf, "# dnsmasq\n#port=5353\n")
	if withCaddy {
		writeFile(t, p.Caddy, "")
	}
	r := run.NewFake()
	for _, label := range system.DnsmasqSystemLabels {
		r.Fail(run.Line(p.Launchctl, "print", "system/"+label), "Could not find service")
	}
	setService(t, p, r, "dnsmasq", running)
	if withCaddy {
		setService(t, p, r, "caddy", running)
	}
	return p, r
}

// Service states setService gives a Homebrew job.
const (
	running = "running" // registered, loaded, with a process
	crashed = "crashed" // registered and loaded, no process
	off     = "off"     // no plist and not loaded, as brew services stop leaves it
)

// setService makes service's job look like state to brew.Info: it writes or
// removes the plist and sets what launchctl print shows.
func setService(t *testing.T, p paths.Paths, r *run.Fake, service, state string) {
	t.Helper()
	plist := filepath.Join(p.LaunchAgents, "sh.brew."+service+".plist")
	switch state {
	case running:
		writeFile(t, plist, "")
		r.Set(printJob(p, service), "\tstate = running\n\tpid = 42\n")
	case crashed:
		writeFile(t, plist, "")
		r.Set(printJob(p, service), "\tstate = not running\n")
	default:
		if err := os.Remove(plist); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		r.Fail(printJob(p, service), "Could not find service")
	}
}

// printJob is the launchctl print that reads service's state.
func printJob(p paths.Paths, service string) string {
	return run.Line(p.Launchctl, "print", "gui/501/sh.brew."+service)
}

// restartCalls are the calls brew.Restart makes for a loaded service.
func restartCalls(p paths.Paths, service string) []string {
	return []string{printJob(p, service), run.Line(p.Launchctl, "kickstart", "-k", "gui/501/sh.brew."+service)}
}

// setupCalls is the command sequence of a setup on a Mac without a root
// dnsmasq job, with Caddy when withCaddy is set.
func setupCalls(p paths.Paths, withCaddy bool) []string {
	install := func(mode, name, target string) string {
		return run.Line(p.Sudo, "-n", p.Install, "-o", "root", "-g", "wheel", "-m", mode, filepath.Join(p.Staging, name), target)
	}
	calls := []string{
		run.Line(p.Sudo, "-v"),
		run.Line(p.Launchctl, "print", "system/homebrew.mxcl.dnsmasq"),
		run.Line(p.Launchctl, "print", "system/sh.brew.dnsmasq"),
		run.Line(p.Sudo, "-n", p.Install, "-d", "-o", "root", "-g", "wheel", "-m", "755", filepath.Dir(p.Script)),
		install("755", "apply-resolvers.sh", p.Script),
		run.Line(p.Visudo, "-c", "-f", filepath.Join(p.Staging, "lodo")),
		install("440", "lodo", p.Sudoers),
		install("644", "io.lodo.loopback.plist", p.LoopbackPlist),
		run.Line(p.Sudo, "-n", p.Launchctl, "bootout", "system/io.lodo.loopback"),
		run.Line(p.Sudo, "-n", p.Launchctl, "bootstrap", "system", p.LoopbackPlist),
	}
	calls = append(calls, restartCalls(p, "dnsmasq")...)
	if withCaddy {
		calls = append(calls, run.Line(p.Caddy, "validate", "--config", p.SystemCaddyfile, "--adapter", "caddyfile"))
		calls = append(calls, restartCalls(p, "caddy")...)
		calls = append(calls, printJob(p, "caddy"))
	}
	return append(calls, run.Line(p.Sudo, "-n", "-k", p.Script))
}

func TestSetup(t *testing.T) {
	t.Parallel()

	for _, withCaddy := range []bool{true, false} {
		name := "without caddy"
		if withCaddy {
			name = "with caddy"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, r := newMac(t, withCaddy)
			var out strings.Builder
			if err := system.Setup(context.Background(), p, r, &out); err != nil {
				t.Fatalf("Setup: %v\n%s", err, out.String())
			}
			assertCalls(t, r.Calls(), setupCalls(p, withCaddy))

			assertFile(t, p.SystemConfBackup, "# dnsmasq\n#port=5353\n")
			assertFile(t, p.SystemConf, "# dnsmasq\n#port=5353\n\n"+system.ConfBlock(p))
			assertFile(t, p.DomainsJSON, "{\n  \"version\": 1,\n  \"domains\": []\n}\n")
			for _, f := range []string{p.DnsmasqConf, p.Resolvers, p.Caddyfile} {
				if _, err := os.Stat(f); err != nil {
					t.Errorf("generated file missing: %v", err)
				}
			}
			if withCaddy {
				assertFile(t, p.SystemCaddyfile, system.CaddyGlobalBlock+"\n"+system.CaddyBlock(p))
				if _, err := os.Stat(p.SystemCaddyfileBackup); !os.IsNotExist(err) {
					t.Errorf("Caddyfile backup made with no Caddyfile before: %v", err)
				}
			} else if !strings.Contains(out.String(), "– Caddy serves lodo's sites on port 80: Caddy is not installed") {
				t.Errorf("output does not say Caddy was skipped:\n%s", out.String())
			}
			if _, err := os.Stat(p.Staging); !os.IsNotExist(err) {
				t.Errorf("staging dir left behind: %v", err)
			}
		})
	}
}

func TestSetup_ScriptGetsEnabledNames(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, false)
	domains := []store.Domain{
		{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
		{Name: "old.test", Address: "127.0.1.2"},
	}
	if err := store.Save(p.DomainsJSON, domains); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := system.Setup(context.Background(), p, r, &out); err != nil {
		t.Fatalf("Setup: %v\n%s", err, out.String())
	}
	if got := r.Inputs(run.Line(p.Sudo, "-n", "-k", p.Script)); !slices.Equal(got, []string{"crm.test\n"}) {
		t.Errorf("resolver script input = %q, want the enabled name", got)
	}
}

func TestSetup_SecondRunKeepsFirstBackup(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, true)
	writeFile(t, p.SystemCaddyfile, "example.com {\n\trespond \"hi\"\n}\n")
	ctx := context.Background()
	for range 2 {
		if err := system.Setup(ctx, p, r, &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
	}
	assertFile(t, p.SystemConfBackup, "# dnsmasq\n#port=5353\n")
	assertFile(t, p.SystemConf, "# dnsmasq\n#port=5353\n\n"+system.ConfBlock(p))
	assertFile(t, p.SystemCaddyfileBackup, "example.com {\n\trespond \"hi\"\n}\n")
	assertFile(t, p.SystemCaddyfile, system.CaddyGlobalBlock+"\nexample.com {\n\trespond \"hi\"\n}\n\n"+system.CaddyBlock(p))
}

// TestSetup_RunsAreTheSame runs setup three times on a Mac with Caddy and an
// HTTPS name: after the first run, which makes the backups, every run prints
// the same lines, runs the same commands and leaves every file as it was.
func TestSetup_RunsAreTheSame(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, true)
	writeFile(t, p.SystemCaddyfile, "example.com {\n\trespond \"hi\"\n}\n")
	writeFile(t, p.CaddyRoot, rootPEM)
	saveDomains(t, p, []store.Domain{{Name: "crm.test", Address: "127.0.1.1", Port: 3000, Enabled: true, HTTPS: true}})

	type result struct {
		out   string
		calls []string
		files map[string]string
	}
	var runs []result
	for range 3 {
		before := len(r.Calls())
		var out strings.Builder
		if err := system.Setup(context.Background(), p, r, &out); err != nil {
			t.Fatalf("Setup: %v\n%s", err, out.String())
		}
		runs = append(runs, result{out.String(), r.Calls()[before:], snapshot(t, filepath.Dir(filepath.Dir(p.Home)))})
	}

	if want := strings.Replace(runs[0].out, "(backup in ", "(backup kept in ", 1); runs[1].out != want {
		t.Errorf("second run prints\n%s\nwant the first run's lines with the backup kept:\n%s", runs[1].out, want)
	}
	for i, run := range runs[1:] {
		if !slices.Equal(run.calls, runs[0].calls) {
			t.Errorf("run %d commands differ from the first:\n%q\nwant\n%q", i+2, run.calls, runs[0].calls)
		}
		if !maps.Equal(run.files, runs[0].files) {
			t.Errorf("run %d changed files: %v", i+2, changed(runs[0].files, run.files))
		}
	}
	if runs[2].out != runs[1].out {
		t.Errorf("third run prints\n%s\nwant the second run's:\n%s", runs[2].out, runs[1].out)
	}
}

// TestSetup_RepairsABackupLodoHadChanged covers a backup made after lodo had
// changed the file, as an early lodo did: it loses lodo's lines, and goes when
// nothing else is left.
func TestSetup_RepairsABackupLodoHadChanged(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, true)
	lodoOnly := system.Marker + "\nimport " + p.Caddyfile + "\n"
	writeFile(t, p.SystemCaddyfile, lodoOnly)
	writeFile(t, p.SystemCaddyfileBackup, lodoOnly)
	writeFile(t, p.SystemConfBackup, "# dnsmasq\nport=5353\n"+system.Marker+"\nconf-file="+p.DnsmasqConf+"\n")
	if err := system.Setup(context.Background(), p, r, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.SystemCaddyfileBackup); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a backup with only lodo's lines is kept: %v", err)
	}
	assertFile(t, p.SystemConfBackup, "# dnsmasq\n")
}

// snapshot reads every regular file under root, by path.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// changed lists the paths whose content differs between two snapshots.
func changed(a, b map[string]string) []string {
	var paths []string
	for path := range maps.Keys(a) {
		if a[path] != b[path] {
			paths = append(paths, path)
		}
	}
	for path := range maps.Keys(b) {
		if _, ok := a[path]; !ok {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

func TestSetup_StopsRootDnsmasq(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, false)
	label := "homebrew.mxcl.dnsmasq"
	r.Set(run.Line(p.Launchctl, "print", "system/"+label), "state = running")
	plist := filepath.Join(p.LaunchDaemons, label+".plist")
	writeFile(t, plist, "<plist/>")
	var out strings.Builder
	if err := system.Setup(context.Background(), p, r, &out); err != nil {
		t.Fatal(err)
	}
	calls := r.Calls()
	for _, want := range []string{
		run.Line(p.Sudo, "-n", p.Launchctl, "bootout", "system/"+label),
		run.Line(p.Sudo, "-n", p.Rm, "-f", plist),
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("missing call %q", want)
		}
	}
	if !strings.Contains(out.String(), "stopped system/homebrew.mxcl.dnsmasq, removed "+plist) {
		t.Errorf("output does not report the root job:\n%s", out.String())
	}
}

func TestSetup_RemovesResolverLocal(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, false)
	local := p.ResolverLocal
	writeFile(t, local, "nameserver 127.0.0.1\n")
	if err := system.Setup(context.Background(), p, r, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.Calls(), run.Line(p.Sudo, "-n", p.Rm, "-f", local)) {
		t.Errorf("no call removes %s", local)
	}
}

func TestSetup_ReportsLeftovers(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, false)
	writeFile(t, filepath.Join(p.Leftovers[0], "dnsmasq.conf"), "")
	var out strings.Builder
	if err := system.Setup(context.Background(), p, r, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Left alone, from LocalDNS") || !strings.Contains(out.String(), p.Leftovers[0]) {
		t.Errorf("output does not list the leftover:\n%s", out.String())
	}
	if !exists(p.Leftovers[0]) {
		t.Error("leftover removed")
	}
}

func TestSetup_Failures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		prepare   func(t *testing.T, p paths.Paths, r *run.Fake)
		wantStep  string
		wantFix   string
		wantCalls int // commands run before stopping
	}{
		{
			name:     "dnsmasq missing",
			prepare:  func(t *testing.T, p paths.Paths, _ *run.Fake) { t.Helper(); removeFile(t, p.Dnsmasq) },
			wantStep: "Homebrew and dnsmasq are installed", wantFix: "brew install dnsmasq", wantCalls: 0,
		},
		{
			name:     "password refused",
			prepare:  func(_ *testing.T, p paths.Paths, r *run.Fake) { r.Fail(run.Line(p.Sudo, "-v"), "Sorry, try again.") },
			wantStep: "admin password", wantFix: "run lodo setup again and enter your password", wantCalls: 1,
		},
		{
			name: "invalid domains.json",
			prepare: func(t *testing.T, p paths.Paths, _ *run.Fake) {
				t.Helper()
				writeFile(t, p.DomainsJSON, `{"version":1,"domains":[{"name":"X","address":"127.0.0.1","enabled":true}]}`)
			},
			wantStep: "lodo's files in", wantFix: "fix or remove", wantCalls: 1,
		},
		{
			name: "visudo rejects the rule",
			prepare: func(_ *testing.T, p paths.Paths, r *run.Fake) {
				r.Fail(run.Line(p.Visudo, "-c", "-f", filepath.Join(p.Staging, "lodo")), "syntax error")
			},
			wantStep: "sudoers rule installed", wantFix: "lodo setup", wantCalls: 6,
		},
		{
			name: "sudo rule does not work",
			prepare: func(_ *testing.T, p paths.Paths, r *run.Fake) {
				r.Fail(run.Line(p.Sudo, "-n", "-k", p.Script), "sudo: a password is required")
			},
			wantStep: "sudo runs the resolver script without a password", wantFix: "lodo setup", wantCalls: 13,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, r := newMac(t, false)
			tt.prepare(t, p, r)
			err := system.Setup(context.Background(), p, r, &strings.Builder{})
			se, ok := errors.AsType[*system.StepError](err)
			if !ok {
				t.Fatalf("err = %v, want *system.StepError", err)
			}
			if !strings.HasPrefix(se.Step, tt.wantStep) || !strings.HasPrefix(se.Fix, tt.wantFix) {
				t.Errorf("step %q fix %q, want %q and %q", se.Step, se.Fix, tt.wantStep, tt.wantFix)
			}
			if got := len(r.Calls()); got != tt.wantCalls {
				t.Errorf("ran %d commands before stopping, want %d: %q", got, tt.wantCalls, r.Calls())
			}
		})
	}
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s =\n%q\nwant\n%q", filepath.Base(path), got, want)
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
