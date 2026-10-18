package dev_test

import (
	"context"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/dev"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/store"
)

const withDev = `{"scripts":{"dev":"next dev"}}`

func TestPackageManager(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		files   map[string]string
		want    string
		wantErr string
	}{
		{
			name:  "packageManager wins over the lock file",
			files: map[string]string{"package.json": `{"packageManager":"pnpm@9.1.0","scripts":{"dev":"x"}}`, "yarn.lock": ""},
			want:  "pnpm",
		},
		{
			name:  "packageManager with a hash",
			files: map[string]string{"package.json": `{"packageManager":"yarn@4.1.0+sha512.abc","scripts":{"dev":"x"}}`},
			want:  "yarn",
		},
		{
			name:    "packageManager lodo doesn't run",
			files:   map[string]string{"package.json": `{"packageManager":"deno@2.0.0","scripts":{"dev":"x"}}`},
			wantErr: `packageManager "deno@2.0.0" is none of pnpm, yarn, bun, npm`,
		},
		{name: "pnpm lock", files: map[string]string{"package.json": withDev, "pnpm-lock.yaml": ""}, want: "pnpm"},
		{name: "yarn lock", files: map[string]string{"package.json": withDev, "yarn.lock": ""}, want: "yarn"},
		{name: "bun.lock", files: map[string]string{"package.json": withDev, "bun.lock": ""}, want: "bun"},
		{name: "bun.lockb", files: map[string]string{"package.json": withDev, "bun.lockb": ""}, want: "bun"},
		{name: "npm lock", files: map[string]string{"package.json": withDev, "package-lock.json": ""}, want: "npm"},
		{
			name:  "pnpm lock before npm's",
			files: map[string]string{"package.json": withDev, "package-lock.json": "", "pnpm-lock.yaml": ""},
			want:  "pnpm",
		},
		{name: "no lock file", files: map[string]string{"package.json": withDev}, want: "npm"},
		{name: "no package.json", files: map[string]string{"yarn.lock": ""}, wantErr: "has no package.json"},
		{
			name:    "no dev script",
			files:   map[string]string{"package.json": `{"scripts":{"lint":"x","build":"y"}}`},
			wantErr: "has no dev script, only build, lint",
		},
		{name: "no scripts", files: map[string]string{"package.json": `{}`}, wantErr: "has no dev script and no other scripts"},
		{name: "broken package.json", files: map[string]string{"package.json": `{`}, wantErr: "unexpected end of JSON input"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			for name, content := range tt.files {
				writeFile(t, filepath.Join(root, name), content)
			}
			got, err := dev.PackageManager(root)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("PackageManager = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPorts(t *testing.T) {
	t.Parallel()

	d := func(name, addr string, port int, on bool) store.Domain {
		return store.Domain{Name: name, Address: addr, Port: port, Enabled: on}
	}
	flowy := []store.Domain{
		d("flowy.test", "127.0.1.2", 3000, true),
		d("web.flowy.test", "127.0.1.2", 3000, true), // same port and address: one entry
		d("api.flowy.test", "127.0.1.2", 4000, true),
		d("v1.api.flowy.test", "127.0.1.2", 4001, true),
		d("mail.flowy.test", "127.0.1.2", 8025, false),
		d("db.flowy.test", "127.0.1.2", 0, true),
		d("other.test", "127.0.1.3", 5000, true),
		d("notflowy.test", "127.0.1.4", 6000, true),
	}
	tests := []struct {
		name    string
		domains []store.Domain
		project string
		want    map[int]string
		wantErr string
	}{
		{
			name: "the project's names that are on, with a port", domains: flowy, project: "flowy.test",
			want: map[int]string{3000: "127.0.1.2", 4000: "127.0.1.2", 4001: "127.0.1.2"},
		},
		{name: "a project lodo doesn't list", domains: flowy, project: "blog.test", want: map[int]string{}},
		{
			name:    "one port, two addresses",
			domains: append(flowy, d("admin.flowy.test", "127.0.0.1", 4000, true)),
			project: "flowy.test",
			wantErr: "api.flowy.test (127.0.1.2) and admin.flowy.test (127.0.0.1) both use port 4000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := dev.Ports(tt.domains, tt.project)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, tt.want) {
				t.Errorf("Ports = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHook(t *testing.T) {
	t.Parallel()

	ports := map[int]string{3000: "127.0.1.2", 10000: "127.0.1.2", 5173: "127.0.1.2"}
	golden.RequireEqual(t, dev.Hook("/Users/tester/.config/lodo/bind.cjs", "flowy.test", ports))
}

func TestNodeOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing string
		hook     string
		want     string
	}{
		{name: "none before", hook: "/a/flowy.test.cjs", want: `--require "/a/flowy.test.cjs"`},
		{
			name: "existing kept", existing: "--max-old-space-size=4096", hook: "/a/flowy.test.cjs",
			want: `--max-old-space-size=4096 --require "/a/flowy.test.cjs"`,
		},
		{
			name: "spaces, quotes and backslashes", hook: `/Users/a b/"x"\y.cjs`,
			want: `--require "/Users/a b/\"x\"\\y.cjs"`,
		},
		{
			name: "already there", existing: `--inspect --require "/a/flowy.test.cjs"`, hook: "/a/flowy.test.cjs",
			want: `--inspect --require "/a/flowy.test.cjs"`,
		},
		{
			name: "another require kept", existing: `--require "/a/other.cjs"`, hook: "/a/flowy.test.cjs",
			want: `--require "/a/other.cjs" --require "/a/flowy.test.cjs"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := dev.NodeOptions(tt.existing, tt.hook); got != tt.want {
				t.Errorf("NodeOptions = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteHooks(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	ports := map[int]string{3000: "127.0.1.2"}
	for range 2 { // the second run finds both files unchanged
		hook, err := dev.WriteHooks(p, "flowy.test", ports)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(p.DevDir, "flowy.test.cjs"); hook != want {
			t.Errorf("hook = %q, want %q", hook, want)
		}
		if got, want := readFile(t, hook), string(dev.Hook(p.BindHook, "flowy.test", ports)); got != want {
			t.Errorf("hook file = %q, want %q", got, want)
		}
		if readFile(t, p.BindHook) != readFile(t, "bind.cjs") {
			t.Error("BindHook differs from the embedded bind.cjs")
		}
	}

	if _, err := dev.WriteHooks(p, "../flowy.test", ports); err == nil {
		t.Error("WriteHooks of ../flowy.test: err nil, want the project name refused")
	}
}

// TestBind runs testdata/bind.test.cjs under node with the hook files
// WriteHooks writes, required through NodeOptions.
func TestBind(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is not installed: %v", err)
	}
	// 127.0.1.x answers on Linux and on a Mac after lodo setup; else the listed
	// port binds 127.0.0.1, which only a listen with no host or :: shows.
	addr, other := "127.0.0.1", ""
	if canListen("127.0.1.1") && canListen("127.0.1.2") {
		addr, other = "127.0.1.1", "127.0.1.2"
	}
	listed, unlisted := freePort(t), freePort(t)
	// A home with a space, quotes and a backslash checks NodeOptions quotes the
	// way Node splits.
	p := paths.ForTest(filepath.Join(t.TempDir(), `a "b" \c`))
	hook, err := dev.WriteHooks(p, "flowy.test", map[int]string{listed: addr})
	if err != nil {
		t.Fatal(err)
	}
	test, err := filepath.Abs("testdata/bind.test.cjs")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", test)
	cmd.Env = append(os.Environ(),
		"NODE_OPTIONS="+dev.NodeOptions("--no-deprecation", hook),
		"LODO_LISTED="+strconv.Itoa(listed), "LODO_UNLISTED="+strconv.Itoa(unlisted),
		"LODO_ADDR="+addr, "LODO_OTHER="+other, "LODO_BIND="+p.BindHook,
		"__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS=", // the hook starts from an empty value
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}

func canListen(addr string) bool {
	l, err := net.Listen("tcp", addr+":0")
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// freePort returns a port nothing listens on right now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
