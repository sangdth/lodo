package brew_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/oo/internal/brew"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/run"
)

// failure is what brew prints when launchctl refuses a job.
const failure = "Error: Failure while executing; `/bin/launchctl bootstrap gui/501 " +
	"/Users/tester/Library/LaunchAgents/homebrew.mxcl.dnsmasq.plist` exited with 5."

func TestInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		service string
		file    string // testdata file brew prints
		out     string // what brew prints when file is empty
		stderr  string // brew fails with this when set
		want    brew.Status
		wantErr string // prefix of the error; empty means none
	}{
		{
			name: "dnsmasq not loaded, captured", service: "dnsmasq", file: "dnsmasq-none.json",
			want: brew.Status{
				Name: "dnsmasq", User: "sang", Status: "none",
				File: "/Users/sang/Library/LaunchAgents/homebrew.mxcl.dnsmasq.plist",
			},
		},
		{
			name: "caddy with null user and pid, captured", service: "caddy", file: "caddy-none.json",
			want: brew.Status{Name: "caddy", Status: "none", File: "/opt/homebrew/opt/caddy/sh.brew.caddy.plist"},
		},
		{
			name: "dnsmasq running, hand-made", service: "dnsmasq", file: "dnsmasq-started.json",
			want: brew.Status{
				Name: "dnsmasq", Running: true, Loaded: true, User: "sang", PID: 4242, Status: "started",
				File: "/Users/sang/Library/LaunchAgents/homebrew.mxcl.dnsmasq.plist",
			},
		},
		{name: "empty list", service: "dnsmasq", out: "[]", wantErr: "parse dnsmasq status: "},
		{name: "bad JSON", service: "caddy", out: `[{"name": "caddy",`, wantErr: "parse caddy status: "},
		{name: "brew fails", service: "dnsmasq", stderr: failure, wantErr: "read dnsmasq status: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := paths.ForTest(t.TempDir())
			line := p.Brew + " services info " + tt.service + " --json"
			f := run.NewFake()
			switch {
			case tt.stderr != "":
				f.Fail(line, tt.stderr)
			case tt.file != "":
				f.Set(line, readTestdata(t, tt.file))
			default:
				f.Set(line, tt.out)
			}

			got, err := brew.Info(t.Context(), f, p.Brew, tt.service)
			if calls := f.Calls(); !slices.Equal(calls, []string{line}) {
				t.Errorf("calls = %q, want %q", calls, []string{line})
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if got != tt.want {
					t.Errorf("Info = %+v, want %+v", got, tt.want)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to start with %q", err, tt.wantErr)
			}
			if tt.stderr != "" {
				if re, ok := errors.AsType[*run.Error](err); !ok || re.Stderr != tt.stderr {
					t.Errorf("err = %v, want it to wrap a *run.Error carrying brew's stderr", err)
				}
			}
		})
	}
}

func TestRestart(t *testing.T) {
	t.Parallel()

	testServiceCommand(t, brew.Restart, "restart")
}

func TestStop(t *testing.T) {
	t.Parallel()

	testServiceCommand(t, brew.Stop, "stop")
}

// testServiceCommand checks fn, which runs brew services <verb> <service>.
func testServiceCommand(t *testing.T, fn func(context.Context, run.Runner, string, string) error, verb string) {
	t.Helper()

	tests := []struct {
		name    string
		service string
		stderr  string // brew fails with this when set
	}{
		{name: "dnsmasq", service: "dnsmasq"},
		{name: "caddy", service: "caddy"},
		{name: "brew fails", service: "dnsmasq", stderr: failure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := paths.ForTest(t.TempDir())
			line := p.Brew + " services " + verb + " " + tt.service
			f := run.NewFake()
			if tt.stderr != "" {
				f.Fail(line, tt.stderr)
			}

			err := fn(t.Context(), f, p.Brew, tt.service)
			if calls := f.Calls(); !slices.Equal(calls, []string{line}) {
				t.Errorf("calls = %q, want %q", calls, []string{line})
			}
			if tt.stderr == "" {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			want := verb + " " + tt.service + ": " + line + ": " + tt.stderr
			if err == nil || err.Error() != want {
				t.Errorf("err = %v, want %q", err, want)
			}
			if _, ok := errors.AsType[*run.Error](err); !ok {
				t.Errorf("err = %v, want it to wrap the *run.Error", err)
			}
		})
	}
}

// readTestdata returns the content of a file under testdata.
func readTestdata(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
