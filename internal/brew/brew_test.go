package brew_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sangdth/lodo/internal/brew"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
)

// failure is what brew prints when launchctl refuses a job.
const failure = "Error: Failure while executing; `/bin/launchctl bootstrap gui/501 " +
	"/Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist` exited with 5."

// notLoaded is what launchctl print says for a job it has not loaded.
const notLoaded = "Could not find service \"sh.brew.dnsmasq\" in domain for user gui: 501"

func TestStatus_Off(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		st   brew.Status
		want bool
	}{
		{name: "running", st: brew.Status{Running: true, Registered: true}, want: false},
		{name: "crashed or unloaded by hand: still registered", st: brew.Status{Registered: true}, want: false},
		{name: "stopped by brew services stop", st: brew.Status{}, want: true},
	}
	for _, tt := range tests {
		if got := tt.st.Off(); got != tt.want {
			t.Errorf("%s: Off() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		plist string // the plist that registers the job, under the test root; empty means none
		print string // testdata file launchctl print shows; empty means the job is not loaded
		label string // the label launchctl print is asked about
		want  brew.Status
	}{
		{
			name: "running, captured", plist: "Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist",
			print: "print-running.txt", label: "sh.brew.dnsmasq",
			want: brew.Status{Running: true, User: "tester", PID: 63372, Registered: true},
		},
		{
			name: "loaded but not running", plist: "Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist",
			print: "print-stopped.txt", label: "sh.brew.dnsmasq",
			want: brew.Status{User: "tester", Registered: true},
		},
		{
			name: "registered under the old label", plist: "Users/tester/Library/LaunchAgents/homebrew.mxcl.dnsmasq.plist",
			print: "print-running.txt", label: "homebrew.mxcl.dnsmasq",
			want: brew.Status{Running: true, User: "tester", PID: 63372, Registered: true},
		},
		{
			name: "registered, not loaded", plist: "Users/tester/Library/LaunchAgents/sh.brew.dnsmasq.plist",
			label: "sh.brew.dnsmasq", want: brew.Status{User: "tester", Registered: true},
		},
		{
			name: "a root plist", plist: "Library/LaunchDaemons/sh.brew.dnsmasq.plist",
			label: "sh.brew.dnsmasq", want: brew.Status{User: "root", Registered: true},
		},
		{name: "turned off", label: "sh.brew.dnsmasq", want: brew.Status{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			p := paths.ForTest(root)
			if tt.plist != "" {
				writeFile(t, filepath.Join(root, tt.plist))
			}
			line := run.Line(p.Launchctl, "print", "gui/501/"+tt.label)
			f := run.NewFake()
			if tt.print != "" {
				f.Set(line, readTestdata(t, tt.print))
			} else {
				f.Fail(line, notLoaded)
			}

			got := brew.Info(t.Context(), f, p, "dnsmasq")
			if calls := f.Calls(); !slices.Equal(calls, []string{line}) {
				t.Errorf("calls = %q, want %q", calls, []string{line})
			}
			if got != tt.want {
				t.Errorf("Info = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRestart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		registered bool
		loaded     bool
		stderr     string // the restart fails with this when set
		want       string // the command that restarts it: kickstart or brew
	}{
		{name: "loaded: kickstart", registered: true, loaded: true, want: "kickstart"},
		{name: "kickstart fails", registered: true, loaded: true, stderr: "Boot-out failed", want: "kickstart"},
		{name: "registered, not loaded: brew", registered: true, want: "brew"},
		{name: "turned off: brew registers it", want: "brew"},
		{name: "brew fails", stderr: failure, want: "brew"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := paths.ForTest(t.TempDir())
			if tt.registered {
				writeFile(t, filepath.Join(p.LaunchAgents, "sh.brew.dnsmasq.plist"))
			}
			printLine := run.Line(p.Launchctl, "print", "gui/501/sh.brew.dnsmasq")
			restart := run.Line(p.Brew, "services", "restart", "dnsmasq")
			if tt.want == "kickstart" {
				restart = run.Line(p.Launchctl, "kickstart", "-k", "gui/501/sh.brew.dnsmasq")
			}
			f := run.NewFake()
			if !tt.loaded {
				f.Fail(printLine, notLoaded)
			}
			if tt.stderr != "" {
				f.Fail(restart, tt.stderr)
			}

			err := brew.Restart(t.Context(), f, p, "dnsmasq")
			if calls, want := f.Calls(), []string{printLine, restart}; !slices.Equal(calls, want) {
				t.Errorf("calls = %q, want %q", calls, want)
			}
			checkErr(t, err, tt.stderr, "restart dnsmasq: "+restart+": "+tt.stderr)
		})
	}
}

func TestStop(t *testing.T) {
	t.Parallel()

	for _, stderr := range []string{"", failure} {
		p := paths.ForTest(t.TempDir())
		line := run.Line(p.Brew, "services", "stop", "caddy")
		f := run.NewFake()
		if stderr != "" {
			f.Fail(line, stderr)
		}

		err := brew.Stop(t.Context(), f, p, "caddy")
		if calls := f.Calls(); !slices.Equal(calls, []string{line}) {
			t.Errorf("calls = %q, want %q", calls, []string{line})
		}
		checkErr(t, err, stderr, "stop caddy: "+line+": "+stderr)
	}
}

// checkErr checks err is nil when stderr is empty, and otherwise is want and
// wraps the *run.Error.
func checkErr(t *testing.T, err error, stderr, want string) {
	t.Helper()

	if stderr == "" {
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		return
	}
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if _, ok := errors.AsType[*run.Error](err); !ok {
		t.Errorf("err = %v, want it to wrap the *run.Error", err)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
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
