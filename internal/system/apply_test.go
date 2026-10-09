package system_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/dnsmasq"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

const (
	caddyRunning = `[{"name":"caddy","running":true,"loaded":true,"user":"tester","pid":42,"status":"started","registered":true}]`
	caddyCrashed = `[{"name":"caddy","running":false,"loaded":true,"user":"tester","pid":null,"status":"error","registered":true}]`
	caddyOff     = `[{"name":"caddy","running":false,"loaded":false,"user":null,"pid":null,"status":"none","registered":false}]`
	dnsmasqOff   = `[{"name":"dnsmasq","running":false,"loaded":false,"user":null,"pid":null,"status":"none","registered":false}]`
)

func TestApply(t *testing.T) {
	t.Parallel()

	dnsOnly := []store.Domain{{Name: "crm.test", Address: "127.0.1.1", Enabled: true}}
	withPort := []store.Domain{
		{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
		{Name: "dashboard.crm.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
	}
	tests := []struct {
		name         string
		caddySetUp   bool
		before       []store.Domain // applied first, so the Caddyfile is current
		domains      []store.Domain
		dnsmasqInfo  string
		caddyInfo    string
		wantCaddy    []string // calls after the resolver script
		wantErrSubst string
	}{
		{name: "dns only, no caddy", domains: dnsOnly},
		{name: "port without caddy set up", domains: withPort, wantErrSubst: "Caddy is not set up for lodo"},
		{name: "dnsmasq turned off", domains: dnsOnly, dnsmasqInfo: dnsmasqOff},
		{name: "port added", caddySetUp: true, domains: withPort, wantCaddy: []string{"info", "validate", "restart"}},
		{name: "ports unchanged, caddy running", caddySetUp: true, before: withPort, domains: withPort, caddyInfo: caddyRunning, wantCaddy: []string{"info"}},
		{name: "ports unchanged, caddy crashed", caddySetUp: true, before: withPort, domains: withPort, caddyInfo: caddyCrashed, wantCaddy: []string{"info", "validate", "restart"}},
		{name: "port added, caddy turned off", caddySetUp: true, domains: withPort, caddyInfo: caddyOff, wantCaddy: []string{"info"}},
		{name: "last port removed", caddySetUp: true, before: withPort, domains: dnsOnly, wantCaddy: []string{"info", "validate", "restart"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, r := setUpMac(t, true)
			if tt.caddySetUp {
				writeFile(t, p.SystemCaddyfile, system.CaddyBlock(p))
			}
			if tt.before != nil {
				if _, err := system.WriteFiles(p, tt.before); err != nil {
					t.Fatal(err)
				}
			}
			r.Set(run.Line(p.Brew, "services", "info", "caddy", "--json"), tt.caddyInfo)
			r.Set(run.Line(p.Brew, "services", "info", "dnsmasq", "--json"), tt.dnsmasqInfo)

			err := system.Apply(context.Background(), p, r, tt.domains)
			if tt.wantErrSubst != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubst) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErrSubst)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := []string{run.Line(p.Brew, "services", "info", "dnsmasq", "--json")}
			if tt.dnsmasqInfo != dnsmasqOff {
				want = append(want, run.Line(p.Brew, "services", "restart", "dnsmasq"))
			}
			want = append(want, run.Line(p.Sudo, "-n", p.Script))
			for _, c := range tt.wantCaddy {
				want = append(want, caddyCall(p, c))
			}
			assertCalls(t, r.Calls(), want)
			assertFile(t, p.Resolvers, dnsmasq.ResolverList(tt.domains))
			wantInput := dnsmasq.ResolverList(tt.domains)
			if tt.dnsmasqInfo == dnsmasqOff {
				wantInput = "" // no resolver file may point at a port dnsmasq left
			}
			if got := r.Inputs(run.Line(p.Sudo, "-n", p.Script)); !slices.Equal(got, []string{wantInput}) {
				t.Errorf("resolver script input = %q, want %q", got, wantInput)
			}
		})
	}
}

func TestApply_StopsAtFirstFailure(t *testing.T) {
	t.Parallel()

	domains := []store.Domain{{Name: "crm.test", Address: "127.0.1.1", Enabled: true}}

	t.Run("dnsmasq restart", func(t *testing.T) {
		t.Parallel()
		p, r := setUpMac(t, false)
		r.Fail(run.Line(p.Brew, "services", "restart", "dnsmasq"), "Error: Failure while executing")
		err := system.Apply(context.Background(), p, r, domains)
		if err == nil || !strings.Contains(err.Error(), "restart dnsmasq") {
			t.Fatalf("err = %v", err)
		}
		if len(r.Calls()) != 2 {
			t.Errorf("calls = %q, want only the status read and the restart", r.Calls())
		}
		assertFile(t, p.Resolvers, "crm.test\n") // files are written before any command
	})
	t.Run("resolver script", func(t *testing.T) {
		t.Parallel()
		p, r := setUpMac(t, false)
		r.Fail(run.Line(p.Sudo, "-n", p.Script), "sudo: a password is required")
		err := system.Apply(context.Background(), p, r, domains)
		if err == nil || !strings.Contains(err.Error(), "sudo: a password is required") || !strings.Contains(err.Error(), p.ResolverDir) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSetService(t *testing.T) {
	t.Parallel()

	domains := []store.Domain{
		{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
		{Name: "old.test", Address: "127.0.1.2"},
	}
	tests := []struct {
		name      string
		service   string
		on        bool
		want      []string
		wantInput []string // the resolver script's standard input, per run
	}{
		{name: "dnsmasq off removes resolver files first", service: "dnsmasq", want: []string{"script", "services stop dnsmasq"}, wantInput: []string{""}},
		{name: "dnsmasq on writes resolver files after", service: "dnsmasq", on: true, want: []string{"services restart dnsmasq", "script"}, wantInput: []string{"crm.test\n"}},
		{name: "caddy off", service: "caddy", want: []string{"services stop caddy"}},
		{name: "caddy on validates first", service: "caddy", on: true, want: []string{"validate", "services restart caddy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, r := setUpMac(t, true)
			writeFile(t, p.SystemCaddyfile, system.CaddyBlock(p))
			if err := store.Save(p.DomainsJSON, domains); err != nil {
				t.Fatal(err)
			}
			if err := system.SetService(context.Background(), p, r, tt.service, tt.on); err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, w := range tt.want {
				switch w {
				case "validate":
					want = append(want, caddyCall(p, "validate"))
				case "script":
					want = append(want, run.Line(p.Sudo, "-n", p.Script))
				default:
					want = append(want, run.Line(p.Brew, strings.Fields(w)...))
				}
			}
			assertCalls(t, r.Calls(), want)
			if got := r.Inputs(run.Line(p.Sudo, "-n", p.Script)); !slices.Equal(got, tt.wantInput) {
				t.Errorf("resolver script input = %q, want %q", got, tt.wantInput)
			}
		})
	}
	t.Run("dnsmasq stays on when the resolver files stay", func(t *testing.T) {
		t.Parallel()
		p, r := setUpMac(t, false)
		r.Fail(run.Line(p.Sudo, "-n", p.Script), "sudo: a password is required")
		if err := system.SetService(context.Background(), p, r, "dnsmasq", false); err == nil {
			t.Fatal("err = nil, want the script's failure")
		}
		assertCalls(t, r.Calls(), []string{run.Line(p.Sudo, "-n", p.Script)})
	})
	t.Run("refuses before setup", func(t *testing.T) {
		t.Parallel()
		p, r := newMac(t, false)
		if err := system.SetService(context.Background(), p, r, "dnsmasq", false); !errors.Is(err, system.ErrNotSetUp) {
			t.Fatalf("err = %v, want ErrNotSetUp", err)
		}
		if len(r.Calls()) != 0 {
			t.Errorf("ran commands before setup: %q", r.Calls())
		}
	})
	t.Run("refuses another service", func(t *testing.T) {
		t.Parallel()
		p, r := setUpMac(t, false)
		if err := system.SetService(context.Background(), p, r, "nginx", false); err == nil || len(r.Calls()) != 0 {
			t.Errorf("err %v, calls %q; want an error and no command", err, r.Calls())
		}
	})
}

func TestApply_BeforeSetup(t *testing.T) {
	t.Parallel()

	domains := []store.Domain{{Name: "crm.test", Address: "127.0.1.1", Enabled: true}}
	tests := []struct {
		name    string
		prepare func(t *testing.T, p paths.Paths)
	}{
		{name: "nothing installed", prepare: func(*testing.T, paths.Paths) {}},
		{name: "script but no conf block", prepare: func(t *testing.T, p paths.Paths) { t.Helper(); writeFile(t, p.Script, "#!/bin/sh\n") }},
		{name: "conf block but no script", prepare: func(t *testing.T, p paths.Paths) {
			t.Helper()
			writeFile(t, p.SystemConf, system.ConfBlock(p))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, r := newMac(t, false)
			tt.prepare(t, p)
			if err := system.Apply(context.Background(), p, r, domains); !errors.Is(err, system.ErrNotSetUp) {
				t.Fatalf("err = %v, want ErrNotSetUp", err)
			}
			if calls := r.Calls(); len(calls) != 0 {
				t.Errorf("ran commands before setup: %q", calls)
			}
			if exists(p.Resolvers) {
				t.Error("wrote generated files before setup")
			}
		})
	}
}

// setUpMac is newMac after setup: Homebrew's dnsmasq.conf includes lodo's and
// the resolver script is installed.
func setUpMac(t *testing.T, withCaddy bool) (paths.Paths, *run.Fake) {
	t.Helper()
	p, r := newMac(t, withCaddy)
	writeFile(t, p.SystemConf, system.RewriteSystemConf("", p))
	writeFile(t, p.Script, system.Script(p))
	return p, r
}

func caddyCall(p paths.Paths, what string) string {
	switch what {
	case "info":
		return run.Line(p.Brew, "services", "info", "caddy", "--json")
	case "validate":
		return run.Line(p.Caddy, "validate", "--config", p.SystemCaddyfile, "--adapter", "caddyfile")
	default:
		return run.Line(p.Brew, "services", "restart", "caddy")
	}
}
