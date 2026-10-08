package caddy_test

import (
	"cmp"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/caddy"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
)

// caddyBin is Homebrew's caddy. The test that runs it skips when it is
// missing.
const caddyBin = "/opt/homebrew/bin/caddy"

// lists are the domain lists the Config tests render. The sample is out of
// order on purpose; only dashboard.crm.test and service.crm.test are enabled
// with a port.
var lists = []struct {
	name    string
	domains []store.Domain
}{
	{
		name: "sample",
		domains: []store.Domain{
			{Name: "flowy.test", Address: "127.0.1.3", Enabled: true},
			{Name: "service.crm.test", Address: "127.0.1.1", Port: 3002, Enabled: true},
			{Name: "dashboard.crm.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
			{Name: "old.test", Address: "127.0.0.1"},
			{Name: "docs.crm.test", Address: "127.0.1.1", Port: 3003},
			{Name: "a.api.crm.test", Address: "127.0.1.1", Enabled: true},
			{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
			{Name: "test.crm.test", Address: "127.0.1.4", Enabled: true},
			{Name: "api.crm.test", Address: "127.0.1.1", Enabled: true},
		},
	},
	{
		name: "no ports",
		domains: []store.Domain{
			{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
			{Name: "flowy.test", Address: "127.0.1.3", Enabled: true},
		},
	},
}

func TestConfig(t *testing.T) {
	t.Parallel()

	for _, tt := range lists {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			golden.RequireEqual(t, caddy.Config(tt.domains))
		})
	}
}

// TestConfig_CaddyAccepts runs the real caddy validate on each generated
// config. HOME and the XDG directories point at a temp dir, so caddy writes
// nothing real.
func TestConfig_CaddyAccepts(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath(caddyBin); err != nil {
		t.Skipf("caddy is not installed: %v", err)
	}
	for _, tt := range lists {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "Caddyfile")
			config := caddy.Config(tt.domains)
			if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), caddyBin, "validate", "--config", path, "--adapter", "caddyfile")
			cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_DATA_HOME="+dir, "XDG_CONFIG_HOME="+dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("caddy rejects the config: %v\n%s\nconfig:\n%s", err, out, config)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fail    bool   // caddy exits 1
		stderr  string // what caddy prints when it fails
		wantErr string // the error when stderr holds caddy's message; else the runner's error, wrapped
	}{
		{name: "valid"},
		{
			name: "caddy 2.11 logs the error as JSON", fail: true,
			stderr: `{"level":"info","ts":1791468283.708788,"msg":"using config from file",` +
				`"file":"/Users/tester/.config/lodo/Caddyfile"}` + "\n" +
				`{"level":"error","ts":1791468283.709107,"msg":"adapting config using caddyfile: ` +
				`/Users/tester/.config/lodo/Caddyfile:4: unrecognized directive: revers_proxy"}`,
			wantErr: "caddy validate: adapting config using caddyfile: " +
				"/Users/tester/.config/lodo/Caddyfile:4: unrecognized directive: revers_proxy",
		},
		{
			name: "plain Error line after JSON logs", fail: true,
			stderr: `{"level":"info","ts":1791468324.516309,"msg":"using config from file",` +
				`"file":"/Users/tester/.config/lodo/Caddyfile"}` + "\n" +
				`{"level":"warn","ts":1791468324.516472,"msg":"no handler"}` + "\n" +
				"Error: adapting config using caddyfile: ambiguous site definition: http://crm.test",
			wantErr: "caddy validate: adapting config using caddyfile: ambiguous site definition: http://crm.test",
		},
		{
			name: "no error message in stderr", fail: true,
			stderr: `{"level":"info","ts":1791468324.645348,"msg":"using config from file"}`,
		},
		{name: "empty stderr", fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := paths.ForTest(t.TempDir())
			line := p.Caddy + " validate --config " + p.Caddyfile + " --adapter caddyfile"
			f := run.NewFake()
			if tt.fail {
				f.Fail(line, tt.stderr)
			}

			err := caddy.Validate(t.Context(), f, p.Caddy, p.Caddyfile)
			if calls := f.Calls(); !slices.Equal(calls, []string{line}) {
				t.Errorf("calls = %q, want %q", calls, []string{line})
			}
			if !tt.fail {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			want := tt.wantErr
			if want == "" {
				want = "caddy validate: " + line + ": " + cmp.Or(tt.stderr, "exit status 1")
				if _, ok := errors.AsType[*run.Error](err); !ok {
					t.Errorf("err = %v, want it to wrap the *run.Error", err)
				}
			}
			if err == nil || err.Error() != want {
				t.Errorf("err = %v, want %q", err, want)
			}
		})
	}
}
