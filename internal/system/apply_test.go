package system_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sangdth/lcd/internal/dnsmasq"
	"github.com/sangdth/lcd/internal/paths"
	"github.com/sangdth/lcd/internal/run"
	"github.com/sangdth/lcd/internal/store"
	"github.com/sangdth/lcd/internal/system"
)

const (
	caddyRunning = `[{"name":"caddy","running":true,"loaded":true,"user":"tester","pid":42,"status":"started"}]`
	caddyStopped = `[{"name":"caddy","running":false,"loaded":false,"user":null,"pid":null,"status":"none"}]`
)

func TestApply(t *testing.T) {
	t.Parallel()

	dnsOnly := []store.Domain{{Name: "crm.local", Address: "127.0.1.1", Enabled: true}}
	withPort := []store.Domain{
		{Name: "crm.local", Address: "127.0.1.1", Enabled: true},
		{Name: "dashboard.crm.local", Address: "127.0.1.1", Port: 3000, Enabled: true},
	}
	tests := []struct {
		name         string
		caddySetUp   bool
		before       []store.Domain // applied first, so the Caddyfile is current
		domains      []store.Domain
		caddyInfo    string
		wantCaddy    []string // calls after the resolver script
		wantErrSubst string
	}{
		{name: "dns only, no caddy", domains: dnsOnly},
		{name: "port without caddy set up", domains: withPort, wantErrSubst: "Caddy is not set up for lcd"},
		{name: "port added", caddySetUp: true, domains: withPort, wantCaddy: []string{"validate", "restart"}},
		{name: "ports unchanged, caddy running", caddySetUp: true, before: withPort, domains: withPort, caddyInfo: caddyRunning, wantCaddy: []string{"info"}},
		{name: "ports unchanged, caddy stopped", caddySetUp: true, before: withPort, domains: withPort, caddyInfo: caddyStopped, wantCaddy: []string{"info", "validate", "restart"}},
		{name: "last port removed", caddySetUp: true, before: withPort, domains: dnsOnly, wantCaddy: []string{"validate", "restart"}},
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

			err := system.Apply(context.Background(), p, r, tt.domains)
			if tt.wantErrSubst != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubst) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErrSubst)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := []string{
				run.Line(p.Brew, "services", "restart", "dnsmasq"),
				run.Line(p.Sudo, "-n", p.Script),
			}
			for _, c := range tt.wantCaddy {
				want = append(want, caddyCall(p, c))
			}
			assertCalls(t, r.Calls(), want)
			assertFile(t, p.Resolvers, dnsmasq.ResolverList(tt.domains))
		})
	}
}

func TestApply_StopsAtFirstFailure(t *testing.T) {
	t.Parallel()

	domains := []store.Domain{{Name: "crm.local", Address: "127.0.1.1", Enabled: true}}

	t.Run("dnsmasq restart", func(t *testing.T) {
		t.Parallel()
		p, r := setUpMac(t, false)
		r.Fail(run.Line(p.Brew, "services", "restart", "dnsmasq"), "Error: Failure while executing")
		err := system.Apply(context.Background(), p, r, domains)
		if err == nil || !strings.Contains(err.Error(), "restart dnsmasq") {
			t.Fatalf("err = %v", err)
		}
		if len(r.Calls()) != 1 {
			t.Errorf("calls = %q, want only the restart", r.Calls())
		}
		assertFile(t, p.Resolvers, "crm.local\n") // files are written before any command
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

func TestApply_BeforeSetup(t *testing.T) {
	t.Parallel()

	domains := []store.Domain{{Name: "crm.local", Address: "127.0.1.1", Enabled: true}}
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

// setUpMac is newMac after setup: Homebrew's dnsmasq.conf includes lcd's and
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
