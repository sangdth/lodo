package system_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sangdth/lodo/internal/dnsmasq"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
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
		before       []store.Domain // applied first, so the generated files are current
		domains      []store.Domain
		dnsmasq      string // the service state; empty means running
		caddy        string
		keepDnsmasq  bool     // dnsmasq runs and its config is unchanged, so it is not restarted
		wantCaddy    []string // calls after the resolver script
		wantErrSubst string
	}{
		{name: "dns only, no caddy", domains: dnsOnly},
		{name: "port without caddy set up", domains: withPort, wantErrSubst: "Caddy is not set up for lodo"},
		{name: "dnsmasq turned off", domains: dnsOnly, dnsmasq: off},
		{name: "dnsmasq config unchanged", before: dnsOnly, domains: dnsOnly, keepDnsmasq: true},
		{name: "dnsmasq config unchanged, dnsmasq crashed", before: dnsOnly, domains: dnsOnly, dnsmasq: crashed},
		{name: "port added", caddySetUp: true, domains: withPort, wantCaddy: []string{"info", "validate", "restart", "info"}},
		{name: "ports unchanged, caddy running", caddySetUp: true, before: withPort, domains: withPort, keepDnsmasq: true, wantCaddy: []string{"info"}},
		{name: "ports unchanged, caddy crashed", caddySetUp: true, before: withPort, domains: withPort, caddy: crashed, keepDnsmasq: true, wantCaddy: []string{"info", "validate", "restart", "info"}, wantErrSubst: "caddy stopped right after it started"},
		{name: "port added, caddy turned off", caddySetUp: true, domains: withPort, caddy: off, wantCaddy: []string{"info"}},
		{name: "last port removed", caddySetUp: true, before: withPort, domains: dnsOnly, wantCaddy: []string{"info", "validate", "restart", "info"}},
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
			if tt.dnsmasq != "" {
				setService(t, p, r, "dnsmasq", tt.dnsmasq)
			}
			if tt.caddy != "" {
				setService(t, p, r, "caddy", tt.caddy)
			}

			err := system.Apply(context.Background(), p, r, tt.domains)
			if tt.wantErrSubst != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubst) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErrSubst)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := []string{printJob(p, "dnsmasq")}
			if tt.dnsmasq != off && !tt.keepDnsmasq {
				want = append(want, restartCalls(p, "dnsmasq")...)
			}
			want = append(want, run.Line(p.Sudo, "-n", p.Script))
			for _, c := range tt.wantCaddy {
				want = append(want, caddyCalls(p, c)...)
			}
			assertCalls(t, r.Calls(), want)
			assertFile(t, p.Resolvers, dnsmasq.ResolverList(tt.domains))
			wantInput := dnsmasq.ResolverList(tt.domains)
			if tt.dnsmasq == off {
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
		restart := restartCalls(p, "dnsmasq")
		r.Fail(restart[1], "Could not kickstart service")
		err := system.Apply(context.Background(), p, r, domains)
		if err == nil || !strings.Contains(err.Error(), "restart dnsmasq") {
			t.Fatalf("err = %v", err)
		}
		assertCalls(t, r.Calls(), append([]string{printJob(p, "dnsmasq")}, restart...))
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
		{name: "dnsmasq on writes resolver files after", service: "dnsmasq", on: true, want: []string{"restart dnsmasq", "script"}, wantInput: []string{"crm.test\n"}},
		{name: "caddy off", service: "caddy", want: []string{"services stop caddy"}},
		{name: "caddy on validates first", service: "caddy", on: true, want: []string{"validate", "restart caddy", "info caddy"}},
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
					want = append(want, caddyCalls(p, "validate")...)
				case "script":
					want = append(want, run.Line(p.Sudo, "-n", p.Script))
				case "restart dnsmasq", "restart caddy":
					want = append(want, restartCalls(p, strings.Fields(w)[1])...)
				case "info caddy":
					want = append(want, printJob(p, "caddy"))
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

func TestSetService_CaddyStopsAfterStart(t *testing.T) {
	t.Parallel()

	const bindErr = "loading initial config: loading new config: http app module: start: " +
		"listening on 127.0.1.1:80: listen tcp 127.0.1.1:80: bind: permission denied"
	log := `{"level":"error","ts":1,"msg":"an older failure"}` + "\n" +
		`{"level":"info","ts":2,"msg":"using config from file"}` + "\n" +
		`{"level":"error","ts":3,"msg":"` + bindErr + `"}` + "\n" +
		`{"level":"info","ts":4,"msg":"servers shutting down"}` + "\n"
	tests := []struct {
		name    string
		state   string // caddy's state after the restart
		log     string
		logAge  time.Duration // how long before the restart Caddy last wrote its log
		wantErr string        // "" when SetService succeeds
	}{
		{name: "still running", state: running, log: log},
		{name: "stopped, log has the reason", state: crashed, log: log, wantErr: "caddy stopped right after it started: " + bindErr},
		{name: "stopped, log is from before", state: crashed, log: log, logAge: time.Hour, wantErr: "; see "},
		{name: "stopped, no log", state: crashed, wantErr: "; see "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, r := setUpMac(t, true)
			writeFile(t, p.SystemCaddyfile, system.CaddyBlock(p))
			if tt.log != "" {
				writeFile(t, p.CaddyLog, tt.log)
				// Caddy writes its log after the restart; a test writes it before.
				written := time.Now().Add(time.Minute - tt.logAge)
				if err := os.Chtimes(p.CaddyLog, written, written); err != nil {
					t.Fatal(err)
				}
			}
			setService(t, p, r, "caddy", tt.state)

			err := system.SetService(context.Background(), p, r, "caddy", true)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
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

// caddyCalls are the calls Apply makes for what: info, validate or restart.
func caddyCalls(p paths.Paths, what string) []string {
	switch what {
	case "info":
		return []string{printJob(p, "caddy")}
	case "validate":
		return []string{run.Line(p.Caddy, "validate", "--config", p.SystemCaddyfile, "--adapter", "caddyfile")}
	default:
		return restartCalls(p, "caddy")
	}
}
