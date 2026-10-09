package check_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/check"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

// healthyDomains are what the test Mac serves unless a test says otherwise: a
// name, a subdomain with a port and a disabled name. The domain with a port
// uses 127.0.0.1, where the test's stand-in for Caddy listens.
var healthyDomains = []store.Domain{
	{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
	{Name: "dashboard.crm.test", Address: "127.0.0.1", Port: 3000, Enabled: true},
	{Name: "old.test", Address: "127.0.1.2"},
}

// fiveDomains are more enabled names than a Detail lists.
var fiveDomains = []store.Domain{
	{Name: "a.test", Address: "127.0.1.1", Enabled: true},
	{Name: "b.test", Address: "127.0.1.2", Enabled: true},
	{Name: "c.test", Address: "127.0.1.3", Enabled: true},
	{Name: "d.test", Address: "127.0.1.4", Enabled: true},
	{Name: "e.test", Address: "127.0.1.5", Enabled: true},
}

// The eight checks, to build expected results.
var (
	dnsmasqCheck   = checkID{1, "dnsmasq"}
	configCheck    = checkID{2, "dnsmasq config"}
	loopbackCheck  = checkID{3, "loopback"}
	resolversCheck = checkID{4, "resolvers"}
	localCheck     = checkID{5, "resolver for .local"}
	generatedCheck = checkID{6, "generated files"}
	namesCheck     = checkID{7, "names resolve"}
	caddyCheck     = checkID{8, "caddy"}
)

type checkID struct {
	id   int
	name string
}

func (c checkID) pass(detail string) check.Check {
	return check.Check{ID: c.id, Name: c.name, OK: true, Detail: detail}
}

func (c checkID) fail(detail, fix string) check.Check {
	return check.Check{ID: c.id, Name: c.name, Detail: detail, Fix: fix}
}

func (c checkID) skip(detail string) check.Check {
	return check.Check{ID: c.id, Name: c.name, Skipped: true, Detail: detail}
}

func (c checkID) off(detail string) check.Check {
	return check.Check{ID: c.id, Name: c.name, Skipped: true, Off: true, Detail: detail}
}

func TestNewEnv(t *testing.T) {
	t.Parallel()

	p := paths.ForTest("/fake")
	r := run.NewFake()
	e := check.NewEnv(p, r)
	if !reflect.DeepEqual(e.Paths, p) {
		t.Errorf("Paths = %+v, want %+v", e.Paths, p)
	}
	if e.Runner != r {
		t.Errorf("Runner = %v, want the runner passed in", e.Runner)
	}
	if e.DNS != "127.0.0.1:53535" {
		t.Errorf("DNS = %q, want 127.0.0.1:53535", e.DNS)
	}
	if e.HTTPPort != 80 {
		t.Errorf("HTTPPort = %d, want 80", e.HTTPPort)
	}
	if e.HTTPSPort != 443 {
		t.Errorf("HTTPSPort = %d, want 443", e.HTTPSPort)
	}
	if e.RootCAs != nil {
		t.Errorf("RootCAs = %v, want nil, the CAs macOS trusts", e.RootCAs)
	}
	if e.RootUID != 0 {
		t.Errorf("RootUID = %d, want 0", e.RootUID)
	}
}

func TestEnv_Run(t *testing.T) {
	t.Parallel()

	m := newMac(t, healthyDomains)
	got := m.env.Run(t.Context(), m.domains)
	want := []check.Check{
		dnsmasqCheck.pass("running as tester, pid 42"),
		configCheck.pass("includes {root}/Users/tester/.config/lodo/dnsmasq.conf"),
		loopbackCheck.pass("50 of 50 addresses on lo0"),
		resolversCheck.pass("2 files"),
		localCheck.pass("absent"),
		generatedCheck.pass("match domains.json"),
		namesCheck.pass("2 of 2 resolve"),
		caddyCheck.pass("running, serves 1 site"),
	}
	for i := range want {
		want[i] = m.expand(want[i])
	}
	if !slices.Equal(got, want) {
		t.Errorf("Run =\n%s\nwant\n%s", formatChecks(got), formatChecks(want))
	}
}

func TestEnv_Run_Dnsmasq(t *testing.T) {
	t.Parallel()

	setBrew := func(running bool, user string) func(*testing.T, *mac) {
		return func(_ *testing.T, m *mac) { m.fake.Set(brewInfo(m.p, "dnsmasq"), brewJSON("dnsmasq", running, user)) }
	}
	runCases(t, []runCase{
		{
			name:   "not installed",
			change: func(t *testing.T, m *mac) { t.Helper(); removeFile(t, m.p.Dnsmasq) },
			want:   dnsmasqCheck.fail("not installed", "brew install dnsmasq"),
		},
		{
			name: "a root job from sudo brew services",
			change: func(_ *testing.T, m *mac) {
				m.fake.Set(launchctlPrint(m.p, "homebrew.mxcl.dnsmasq"), "system/homebrew.mxcl.dnsmasq = {\n}\n")
			},
			want: dnsmasqCheck.fail("a root job, system/homebrew.mxcl.dnsmasq, runs dnsmasq and shadows lodo's", "lodo setup"),
		},
		{
			name: "a root job under the newer label",
			change: func(_ *testing.T, m *mac) {
				m.fake.Set(launchctlPrint(m.p, "sh.brew.dnsmasq"), "system/sh.brew.dnsmasq = {\n}\n")
			},
			want: dnsmasqCheck.fail("a root job, system/sh.brew.dnsmasq, runs dnsmasq and shadows lodo's", "lodo setup"),
		},
		{
			name: "brew fails",
			change: func(_ *testing.T, m *mac) {
				m.fake.Fail(brewInfo(m.p, "dnsmasq"), `Error: No available formula with the name "dnsmasq".`)
			},
			want: dnsmasqCheck.fail(
				`read dnsmasq status: {root}/opt/homebrew/bin/brew services info dnsmasq --json: Error: No available formula with the name "dnsmasq".`,
				"lodo setup"),
		},
		{
			name:   "not running",
			change: setBrew(false, "tester"),
			want:   dnsmasqCheck.fail("not running", "lodo setup"),
		},
		{
			name:   "turned off",
			change: func(_ *testing.T, m *mac) { m.fake.Set(brewInfo(m.p, "dnsmasq"), brewOffJSON("dnsmasq")) },
			want:   dnsmasqCheck.off("turned off: no .test name resolves until it is on"),
		},
		{
			name:   "runs as another user",
			change: setBrew(true, "nobody"),
			want:   dnsmasqCheck.fail("runs as nobody", "lodo setup"),
		},
		{
			name:   "brew names no user",
			change: setBrew(true, ""),
			want:   dnsmasqCheck.pass("running as tester, pid 42"),
		},
	})
}

func TestEnv_Run_DnsmasqConfig(t *testing.T) {
	t.Parallel()

	runCases(t, []runCase{
		{
			name:   "missing",
			change: func(t *testing.T, m *mac) { t.Helper(); removeFile(t, m.p.SystemConf) },
			want:   configCheck.fail("missing", "lodo setup"),
		},
		{
			name:   "a folder where the file should be",
			change: func(t *testing.T, m *mac) { t.Helper(); replaceWithDir(t, m.p.SystemConf) },
			want:   configCheck.fail("read {root}/opt/homebrew/etc/dnsmasq.conf: is a directory", "lodo setup"),
		},
		{
			name:   "conf-file points at another file",
			change: writeConf("conf-file=/Users/tester/.config/localdns/dnsmasq.conf", "listen-address=127.0.0.1", "port=53535", "bind-interfaces"),
			want:   configCheck.fail("conf-file points at /Users/tester/.config/localdns/dnsmasq.conf", "lodo setup"),
		},
		{
			name:   "two conf-file lines",
			change: writeConf("conf-file={conf}", "conf-file=/x.conf", "listen-address=127.0.0.1", "port=53535", "bind-interfaces"),
			want:   configCheck.fail("2 conf-file lines", "lodo setup"),
		},
		{
			name:   "a commented conf-file line does not count",
			change: writeConf("#conf-file={conf}", "listen-address=127.0.0.1", "port=53535", "bind-interfaces"),
			want:   configCheck.fail("no conf-file line", "lodo setup"),
		},
		{
			name:   "port 53",
			change: writeConf("conf-file={conf}", "listen-address=127.0.0.1", "port=53", "bind-interfaces"),
			want:   configCheck.fail("port is 53, want 53535", "lodo setup"),
		},
		{
			name:   "two port lines",
			change: writeConf("conf-file={conf}", "listen-address=127.0.0.1", "port=53535", "port=53", "bind-interfaces"),
			want:   configCheck.fail("2 port lines", "lodo setup"),
		},
		{
			name:   "no port line",
			change: writeConf("conf-file={conf}", "listen-address=127.0.0.1", "bind-interfaces"),
			want:   configCheck.fail("no port line", "lodo setup"),
		},
		{
			name:   "listen-address with a second address",
			change: writeConf("conf-file={conf}", "listen-address=127.0.0.1,10.0.0.1", "port=53535", "bind-interfaces"),
			want:   configCheck.fail("listen-address is 127.0.0.1,10.0.0.1, want 127.0.0.1", "lodo setup"),
		},
		{
			name:   "no listen-address line",
			change: writeConf("conf-file={conf}", "port=53535", "bind-interfaces"),
			want:   configCheck.fail("no listen-address line", "lodo setup"),
		},
		{
			name:   "no bind-interfaces line",
			change: writeConf("conf-file={conf}", "listen-address=127.0.0.1", "port=53535", "#bind-interfaces"),
			want:   configCheck.fail("no bind-interfaces line", "lodo setup"),
		},
		{
			name:   "spaces, comments and other options",
			change: writeConf("# dnsmasq", "", "#port=53", "server=1.1.1.1", "  conf-file = {conf} ", "listen-address= 127.0.0.1", "port =53535", "\tbind-interfaces"),
			want:   configCheck.pass("includes {root}/Users/tester/.config/lodo/dnsmasq.conf"),
		},
	})
}

func TestEnv_Run_Loopback(t *testing.T) {
	t.Parallel()

	notLoaded := func(m *mac) { m.fake.Fail(launchctlPrint(m.p, system.LoopbackLabel), "Could not find service") }
	runCases(t, []runCase{
		{
			name:   "49 of the 50 aliases",
			change: func(_ *testing.T, m *mac) { m.fake.Set(run.Line(m.p.Ifconfig, "lo0"), lo0(49)) },
			want:   loopbackCheck.fail("49 of 50 addresses on lo0", "lodo setup"),
		},
		{
			name: "an address past the block does not count",
			change: func(_ *testing.T, m *mac) {
				m.fake.Set(run.Line(m.p.Ifconfig, "lo0"), lo0(49)+"\tinet 127.0.1.51 netmask 0xff000000\n")
			},
			want: loopbackCheck.fail("49 of 50 addresses on lo0", "lodo setup"),
		},
		{
			name: "job not loaded, no aliases",
			change: func(_ *testing.T, m *mac) {
				notLoaded(m)
				m.fake.Set(run.Line(m.p.Ifconfig, "lo0"), lo0(0))
			},
			want: loopbackCheck.fail("job not loaded; 0 of 50 addresses on lo0", "lodo setup"),
		},
		{
			name:   "job not loaded, aliases still up",
			change: func(_ *testing.T, m *mac) { notLoaded(m) },
			want:   loopbackCheck.fail("job not loaded; 50 of 50 addresses on lo0", "lodo setup"),
		},
	})
}

func TestEnv_Run_Resolvers(t *testing.T) {
	t.Parallel()

	runCases(t, []runCase{
		{
			name:   "script missing",
			change: func(t *testing.T, m *mac) { t.Helper(); removeFile(t, m.p.Script) },
			want:   resolversCheck.fail("script missing", "lodo setup"),
		},
		{
			name: "script is a symlink",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "apply-resolvers.sh")
				writeFile(t, target, system.Script(m.p))
				chmod(t, target, 0o755)
				removeFile(t, m.p.Script)
				symlink(t, target, m.p.Script)
			},
			want: resolversCheck.fail("script is a symlink", "lodo setup"),
		},
		{
			name:   "script is a folder",
			change: func(t *testing.T, m *mac) { t.Helper(); replaceWithDir(t, m.p.Script) },
			want:   resolversCheck.fail("script is not a regular file", "lodo setup"),
		},
		{
			name: "a file where the script's folder should be",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				dir := filepath.Dir(m.p.Script)
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				writeFile(t, dir, "")
			},
			want: resolversCheck.fail("lstat {root}/Library/Application Support/lodo/apply-resolvers.sh: not a directory", "lodo setup"),
		},
		{
			name:   "script not owned by root",
			change: func(_ *testing.T, m *mac) { m.env.RootUID++ },
			want:   resolversCheck.fail("script not owned by root", "lodo setup"),
		},
		{
			name:   "script mode 0644",
			change: func(t *testing.T, m *mac) { t.Helper(); chmod(t, m.p.Script, 0o644) },
			want:   resolversCheck.fail("script mode 0644, want 0755", "lodo setup"),
		},
		{
			name:   "script from an older lodo",
			change: func(t *testing.T, m *mac) { t.Helper(); writeFile(t, m.p.Script, "#!/bin/sh\nexit 0\n") },
			want:   resolversCheck.fail("script out of date", "lodo setup"),
		},
		{
			name: "sudo asks for a password",
			change: func(_ *testing.T, m *mac) {
				m.fake.Fail(run.Line(m.p.Sudo, "-n", "-k", "-l", m.p.Script), "sudo: a password is required")
			},
			want: resolversCheck.fail("sudo asks for a password: the sudoers rule is missing", "lodo setup"),
		},
		{
			name:   "a resolver file missing",
			change: func(t *testing.T, m *mac) { t.Helper(); removeFile(t, filepath.Join(m.p.ResolverDir, "crm.test")) },
			want:   resolversCheck.fail("missing for crm.test", "lodo apply"),
		},
		{
			name: "a resolver file lodo did not write",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				writeFile(t, filepath.Join(m.p.ResolverDir, "dashboard.crm.test"), "nameserver 127.0.0.1\n")
			},
			want: resolversCheck.fail("not written by lodo, so lodo apply won't replace: dashboard.crm.test",
				"sudo rm {root}/etc/resolver/dashboard.crm.test, then lodo apply"),
		},
		{
			name: "a hand-made file outranks a missing one",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				removeFile(t, filepath.Join(m.p.ResolverDir, "crm.test"))
				writeFile(t, filepath.Join(m.p.ResolverDir, "dashboard.crm.test"), "nameserver 127.0.0.1\n")
			},
			want: resolversCheck.fail("not written by lodo, so lodo apply won't replace: dashboard.crm.test",
				"sudo rm {root}/etc/resolver/dashboard.crm.test, then lodo apply"),
		},
		{
			name: "an lodo file with old content",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				writeFile(t, filepath.Join(m.p.ResolverDir, "crm.test"), system.Marker+"\nnameserver 127.0.0.1\n")
			},
			want: resolversCheck.fail("missing for crm.test", "lodo apply"),
		},
		{
			name:    "more resolver files missing than the detail lists",
			domains: fiveDomains,
			change: func(t *testing.T, m *mac) {
				t.Helper()
				for _, d := range m.domains {
					removeFile(t, filepath.Join(m.p.ResolverDir, d.Name))
				}
			},
			want: resolversCheck.fail("missing for a.test, b.test, c.test, and 2 more", "lodo apply"),
		},
		{
			name:    "one enabled domain",
			domains: healthyDomains[:1],
			want:    resolversCheck.pass("1 file"),
		},
		{
			name:    "no enabled domain",
			domains: healthyDomains[2:],
			want:    resolversCheck.pass("0 files"),
		},
	})
}

func TestEnv_Run_ResolverForLocal(t *testing.T) {
	t.Parallel()

	want := localCheck.fail("{root}/etc/resolver/local sends every .local name to one server, away from Bonjour",
		"sudo rm {root}/etc/resolver/local")
	runCases(t, []runCase{
		{
			name: "present",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				writeFile(t, filepath.Join(m.p.ResolverDir, "local"), "nameserver 127.0.0.1\nport 53535\n")
			},
			want: want,
		},
		{
			name: "a dangling symlink",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				symlink(t, filepath.Join(m.root, "nowhere"), filepath.Join(m.p.ResolverDir, "local"))
			},
			want: want,
		},
		{
			name: "a file where /etc/resolver should be",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				if err := os.RemoveAll(m.p.ResolverDir); err != nil {
					t.Fatal(err)
				}
				writeFile(t, m.p.ResolverDir, "")
			},
			want: localCheck.pass("absent"),
		},
	})
}

func TestEnv_Run_GeneratedFiles(t *testing.T) {
	t.Parallel()

	runCases(t, []runCase{
		{
			name:   "dnsmasq.conf edited by hand",
			change: func(t *testing.T, m *mac) { t.Helper(); writeFile(t, m.p.DnsmasqConf, "address=/x.test/127.0.0.1\n") },
			want:   generatedCheck.fail("out of date: dnsmasq.conf", "lodo apply"),
		},
		{
			name:   "resolvers missing",
			change: func(t *testing.T, m *mac) { t.Helper(); removeFile(t, m.p.Resolvers) },
			want:   generatedCheck.fail("out of date: resolvers", "lodo apply"),
		},
		{
			name: "every file stale",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				for _, f := range []string{m.p.DnsmasqConf, m.p.Resolvers, m.p.Caddyfile} {
					writeFile(t, f, "stale\n")
				}
			},
			want: generatedCheck.fail("out of date: dnsmasq.conf, resolvers, Caddyfile", "lodo apply"),
		},
	})
}

func TestEnv_Run_NamesResolve(t *testing.T) {
	t.Parallel()

	runCases(t, []runCase{
		{
			name:    "no enabled domain",
			domains: healthyDomains[2:],
			want:    namesCheck.pass("no enabled domains"),
		},
		{
			name: "macOS resolves a name to another address",
			change: func(_ *testing.T, m *mac) {
				m.fake.Set(dscacheutil(m.p, "crm.test"), macOSOutput("crm.test", "127.0.1.9"))
			},
			want: namesCheck.fail("crm.test: macOS: 127.0.1.9, want 127.0.1.1", "lodo apply"),
		},
		{
			name: "dnsmasq turned off",
			change: func(_ *testing.T, m *mac) {
				m.fake.Set(brewInfo(m.p, "dnsmasq"), brewOffJSON("dnsmasq"))
				m.fake.Set(dscacheutil(m.p, "crm.test"), macOSOutput("crm.test", "127.0.1.9"))
			},
			want: namesCheck.fail("crm.test: macOS: 127.0.1.9, want 127.0.1.1",
				"turn dnsmasq on: brew services start dnsmasq, or tab and space in lodo"),
		},
		{
			name: "caddy cannot reach an app",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				m.env.HTTPPort = startHTTP(t, respond(http.StatusBadGateway, "Server", "Caddy"))
			},
			want: namesCheck.fail("dashboard.crm.test: app down: nothing answers on 127.0.0.1:3000", "lodo apply"),
		},
		{
			name:    "more names fail than the detail lists",
			domains: fiveDomains,
			change: func(_ *testing.T, m *mac) {
				for _, d := range m.domains {
					m.fake.Set(dscacheutil(m.p, d.Name), "")
				}
			},
			want: namesCheck.fail(
				"a.test: macOS: no address; b.test: macOS: no address; c.test: macOS: no address; and 2 more", "lodo apply"),
		},
	})
}

func TestEnv_Run_Caddy(t *testing.T) {
	t.Parallel()

	validate := func(p paths.Paths) string {
		return run.Line(p.Caddy, "validate", "--config", p.SystemCaddyfile, "--adapter", "caddyfile")
	}
	setLsof := func(rows ...string) func(*testing.T, *mac) {
		return func(_ *testing.T, m *mac) {
			m.fake.Set(lsof(m.p, m.env.HTTPPort), lsofHeader+strings.Join(rows, ""))
		}
	}
	const (
		nginxRow = "nginx      501 tester    6u  IPv4 0x2      0t0  TCP *:80 (LISTEN)\n"
		caddyRow = "caddy     4242 tester    7u  IPv6 0x1      0t0  TCP *:80 (LISTEN)\n"
	)
	runCases(t, []runCase{
		{
			name: "no enabled domain has a port, caddy running",
			domains: []store.Domain{
				{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
				{Name: "dashboard.crm.test", Address: "127.0.0.1", Port: 3000},
			},
			want: caddyCheck.pass("running, no enabled domain has a port"),
		},
		{
			name: "no enabled domain has a port, caddy not running",
			domains: []store.Domain{
				{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
			},
			change: func(_ *testing.T, m *mac) { m.fake.Set(brewInfo(m.p, "caddy"), brewJSON("caddy", false, "")) },
			want:   caddyCheck.skip("no enabled domain has a port"),
		},
		{
			name: "turned off, no port needs it",
			domains: []store.Domain{
				{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
			},
			change: func(_ *testing.T, m *mac) { m.fake.Set(brewInfo(m.p, "caddy"), brewOffJSON("caddy")) },
			want:   caddyCheck.off("turned off: no http://name.test reaches its port until it is on"),
		},
		{
			name:   "turned off",
			change: func(_ *testing.T, m *mac) { m.fake.Set(brewInfo(m.p, "caddy"), brewOffJSON("caddy")) },
			want:   caddyCheck.off("turned off: no http://name.test reaches its port until it is on"),
		},
		{
			name:   "not installed",
			change: func(t *testing.T, m *mac) { t.Helper(); removeFile(t, m.p.Caddy) },
			want:   caddyCheck.fail("not installed", "brew install caddy, then lodo setup"),
		},
		{
			name: "Homebrew's Caddyfile without lodo's import",
			change: func(t *testing.T, m *mac) {
				t.Helper()
				writeFile(t, m.p.SystemCaddyfile, "localhost {\n\trespond \"hi\"\n}\n")
			},
			want: caddyCheck.fail("Homebrew's Caddyfile does not import lodo's", "lodo setup"),
		},
		{
			name: "brew fails",
			change: func(_ *testing.T, m *mac) {
				m.fake.Fail(brewInfo(m.p, "caddy"), "Error: Formula caddy is not installed.")
			},
			want: caddyCheck.fail("read caddy status: {root}/opt/homebrew/bin/brew services info caddy --json: Error: Formula caddy is not installed.",
				"lodo setup"),
		},
		{
			name:   "not running",
			change: func(_ *testing.T, m *mac) { m.fake.Set(brewInfo(m.p, "caddy"), brewJSON("caddy", false, "")) },
			want:   caddyCheck.fail("not running", "lodo setup"),
		},
		{
			name: "caddy rejects the config",
			change: func(_ *testing.T, m *mac) {
				m.fake.Fail(validate(m.p), "2026/10/08 12:00:00.000\tINFO\tusing config from file\n"+
					"Error: adapting config using caddyfile: Caddyfile:3: unrecognized directive: bogus")
			},
			want: caddyCheck.fail("caddy validate: adapting config using caddyfile: Caddyfile:3: unrecognized directive: bogus", "lodo apply"),
		},
		{
			name:   "validate fails with several lines of output",
			change: func(_ *testing.T, m *mac) { m.fake.Fail(validate(m.p), "line one\nline two") },
			want: caddyCheck.fail("caddy validate: {root}/opt/homebrew/bin/caddy validate --config {root}/opt/homebrew/etc/Caddyfile"+
				" --adapter caddyfile: line one; line two", "lodo apply"),
		},
		{
			name:   "another program has the port",
			change: setLsof(nginxRow),
			want:   caddyCheck.fail("port {port} is taken by nginx", "stop nginx, then lodo apply"),
		},
		{
			name:   "caddy among several listeners",
			change: setLsof(nginxRow, caddyRow),
			want:   caddyCheck.pass("running, serves 1 site"),
		},
		{
			name:   "nothing listens",
			change: func(_ *testing.T, m *mac) { m.fake.Fail(lsof(m.p, m.env.HTTPPort), "") },
			want:   caddyCheck.fail("nothing listens on port {port}", "lodo setup"),
		},
		{
			name: "two sites",
			domains: append(slices.Clone(healthyDomains),
				store.Domain{Name: "api.crm.test", Address: "127.0.0.1", Port: 3001, Enabled: true}),
			want: caddyCheck.pass("running, serves 2 sites"),
		},
	})
}

func TestEnv_Report(t *testing.T) {
	t.Parallel()

	m := newMac(t, healthyDomains)
	checks, results := m.env.Report(t.Context(), m.domains)
	if len(checks) != 8 || !checks[6].OK {
		t.Fatalf("checks = %+v, want eight with names resolving", checks)
	}
	if len(results) != 2 || !results[0].OK() || !results[1].OK() {
		t.Errorf("results = %+v, want both enabled domains passing", results)
	}
}

func TestEnv_Prerequisites(t *testing.T) {
	t.Parallel()

	m := newMac(t, healthyDomains)
	m.fake.Fail(run.Line(m.p.Launchctl, "print", "system/io.lodo.loopback"), "Could not find service")
	got := m.env.Prerequisites(t.Context(), m.domains)
	if len(got) != 5 {
		t.Fatalf("got %d checks, want checks 1 to 5", len(got))
	}
	for i, c := range got {
		if c.ID != i+1 {
			t.Errorf("check %d has ID %d", i, c.ID)
		}
	}
	if failed := check.Failed(got); len(failed) != 1 || failed[0].ID != 3 {
		t.Errorf("failed = %+v, want only the loopback check", failed)
	}
}

func TestEnv_Prerequisites_DnsmasqOff(t *testing.T) {
	t.Parallel()

	m := newMac(t, healthyDomains)
	m.fake.Set(brewInfo(m.p, "dnsmasq"), brewOffJSON("dnsmasq"))
	if failed := check.Failed(m.env.Prerequisites(t.Context(), m.domains)); len(failed) != 0 {
		t.Errorf("failed = %+v; a dnsmasq turned off must not keep the TUI shut", failed)
	}
}

func TestFailed(t *testing.T) {
	t.Parallel()

	ok := check.Check{ID: 1, OK: true}
	skipped := check.Check{ID: 2, Skipped: true}
	bad := check.Check{ID: 3, Detail: "missing", Fix: "lodo setup"}
	worse := check.Check{ID: 4, Detail: "not running", Fix: "lodo setup"}
	tests := []struct {
		name   string
		checks []check.Check
		want   []check.Check
	}{
		{name: "none", checks: nil, want: nil},
		{name: "passed and skipped", checks: []check.Check{ok, skipped}, want: nil},
		{name: "turned off", checks: []check.Check{{ID: 1, Skipped: true, Off: true}}, want: nil},
		{name: "failures in order", checks: []check.Check{ok, bad, skipped, worse}, want: []check.Check{bad, worse}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := check.Failed(tt.checks); !slices.Equal(got, tt.want) {
				t.Errorf("Failed = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// runCase breaks one part of a healthy Mac and names the check that must
// notice.
type runCase struct {
	name    string
	domains []store.Domain             // nil means healthyDomains
	change  func(t *testing.T, m *mac) // nil changes nothing
	want    check.Check                // {root} stands for the temp root, {port} for the HTTP port
}

// runCases runs every check on a Mac changed as each case says and compares
// the check the case names.
func runCases(t *testing.T, tests []runCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			domains := tt.domains
			if domains == nil {
				domains = healthyDomains
			}
			m := newMac(t, domains)
			if tt.change != nil {
				tt.change(t, m)
			}
			got := m.env.Run(t.Context(), m.domains)[tt.want.ID-1]
			if want := m.expand(tt.want); got != want {
				t.Errorf("check %d =\n%+v\nwant\n%+v", want.ID, got, want)
			}
		})
	}
}

// mac is a Mac in a temp dir where setup and apply ran for domains: every
// file is in place, every service runs, and every check passes.
type mac struct {
	root    string
	p       paths.Paths
	fake    *run.Fake
	env     check.Env
	domains []store.Domain
}

func newMac(t *testing.T, domains []store.Domain) *mac {
	t.Helper()
	root := t.TempDir()
	p := paths.ForTest(root)
	m := &mac{root: root, p: p, fake: run.NewFake(), domains: domains}

	writeFile(t, p.Dnsmasq, "")
	writeFile(t, p.SystemConf, "# dnsmasq\n\n"+system.ConfBlock(p))
	writeFile(t, p.Script, system.Script(p))
	chmod(t, p.Script, 0o755)
	for _, d := range domains {
		if d.Enabled {
			writeFile(t, filepath.Join(p.ResolverDir, d.Name), system.ResolverFile())
		}
	}
	if _, err := system.WriteFiles(p, domains); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.Caddy, "")
	writeFile(t, p.SystemCaddyfile, system.CaddyBlock(p))

	for _, label := range system.DnsmasqSystemLabels {
		m.fake.Fail(launchctlPrint(p, label), `Could not find service "`+label+`" in domain for system`)
	}
	m.fake.Set(launchctlPrint(p, system.LoopbackLabel), "system/io.lodo.loopback = {\n\tstate = not running\n}\n")
	m.fake.Set(run.Line(p.Ifconfig, "lo0"), lo0(store.OwnLast))
	m.fake.Set(brewInfo(p, "dnsmasq"), brewJSON("dnsmasq", true, "tester"))
	m.fake.Set(brewInfo(p, "caddy"), brewJSON("caddy", true, "tester"))
	m.fake.Set(run.Line(p.Sudo, "-n", "-k", "-l", p.Script), p.Script+"\n")
	answers := map[string]string{}
	for _, d := range domains {
		answers[d.Name] = d.Address
		m.fake.Set(dscacheutil(p, d.Name), macOSOutput(d.Name, d.Address))
	}

	m.env = check.NewEnv(p, m.fake)
	m.env.DNS = startDNS(t, fromMap(answers))
	m.env.HTTPPort = startHTTP(t, respond(http.StatusOK, "Server", "Caddy", "Via", "1.1 Caddy"))
	m.env.RootUID = uint32(os.Getuid())
	m.fake.Set(lsof(p, m.env.HTTPPort), lsofHeader+"caddy    4242 tester    7u  IPv6 0x1      0t0  TCP *:80 (LISTEN)\n")
	return m
}

// expand fills in the placeholders of an expected check: {root} for the temp
// root that holds every path, {port} for the HTTP port.
func (m *mac) expand(c check.Check) check.Check {
	r := strings.NewReplacer("{root}", m.root, "{port}", strconv.Itoa(m.env.HTTPPort))
	c.Detail, c.Fix = r.Replace(c.Detail), r.Replace(c.Fix)
	return c
}

// writeConf returns a change that writes Homebrew's dnsmasq.conf from lines,
// where {conf} stands for lodo's dnsmasq.conf.
func writeConf(lines ...string) func(*testing.T, *mac) {
	return func(t *testing.T, m *mac) {
		t.Helper()
		content := strings.Join(lines, "\n") + "\n"
		writeFile(t, m.p.SystemConf, strings.ReplaceAll(content, "{conf}", m.p.DnsmasqConf))
	}
}

func launchctlPrint(p paths.Paths, label string) string {
	return run.Line(p.Launchctl, "print", "system/"+label)
}

func brewInfo(p paths.Paths, service string) string {
	return run.Line(p.Brew, "services", "info", service, "--json")
}

// brewJSON is brew services info --json output, trimmed to the fields lodo
// reads, for a registered job. An empty user is JSON null, as brew prints for
// a job that never ran.
func brewJSON(service string, running bool, user string) string {
	u, pid, status := "null", "null", "none"
	if user != "" {
		u = strconv.Quote(user)
	}
	if running {
		pid, status = "42", "started"
	}
	return fmt.Sprintf(`[{"name":%q,"running":%t,"loaded":%t,"user":%s,"pid":%s,"status":%q,"registered":true}]`,
		service, running, running, u, pid, status)
}

// brewOffJSON is brew services info --json output for a job brew services
// stop turned off: stopped and no longer registered.
func brewOffJSON(service string) string {
	return fmt.Sprintf(`[{"name":%q,"running":false,"loaded":false,"user":null,"pid":null,"status":"none","registered":false}]`, service)
}

// lo0 is ifconfig lo0 output on a Mac with the first n own addresses.
func lo0(n int) string {
	var b strings.Builder
	b.WriteString("lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384\n" +
		"\toptions=1203<RXCSUM,TXCSUM,TXSTATUS,SW_TIMESTAMP>\n" +
		"\tinet 127.0.0.1 netmask 0xff000000\n" +
		"\tinet6 ::1 prefixlen 128 \n" +
		"\tinet6 fe80::1%lo0 prefixlen 64 scopeid 0x1 \n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "\tinet %s netmask 0xff000000\n", store.OwnAddress(i))
	}
	b.WriteString("\tnd6 options=201<PERFORMNUD,DAD>\n")
	return b.String()
}

const lsofHeader = "COMMAND   PID   USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n"

func lsof(p paths.Paths, port int) string {
	return run.Line(p.Lsof, "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN")
}

func formatChecks(checks []check.Check) string {
	var b strings.Builder
	for _, c := range checks {
		fmt.Fprintf(&b, "  %+v\n", c)
	}
	return b.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func replaceWithDir(t *testing.T, path string) {
	t.Helper()
	removeFile(t, path)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
