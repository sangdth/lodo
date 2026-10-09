package system_test

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

// fake is a fixed set of paths for golden files.
var fake = paths.ForTest("/fake")

func TestResolverFile(t *testing.T) {
	t.Parallel()

	if got, want := system.ResolverFile(), "# lodo\nnameserver 127.0.0.1\nport 53535\n"; got != want {
		t.Errorf("ResolverFile = %q, want %q", got, want)
	}
}

func TestScript(t *testing.T) {
	t.Parallel()

	golden.RequireEqual(t, system.Script(fake))
}

func TestScript_Run(t *testing.T) {
	t.Parallel()

	resolver := system.ResolverFile()
	long := strings.Repeat("abc.", 61) + "abcde.test" // matches the pattern, 254 characters
	tests := []struct {
		name       string
		list       string            // the script's standard input
		before     map[string]string // resolver dir content before the run
		after      map[string]string // resolver dir content after the run
		wantStdout string            // checked when the script succeeds
		wantStderr string            // substring
		wantFail   bool
	}{
		{
			name:       "writes valid names",
			list:       "crm.test\ntest.crm.test\n",
			after:      map[string]string{"crm.test": resolver, "test.crm.test": resolver},
			wantStdout: "lodo: 2 resolver files written, 0 removed, 0 lines skipped\n",
		},
		{
			name:       "skips invalid lines",
			list:       "test\n../x.test\nX.TEST\nx.com\n\nx.test \n-x.test\nflowy.test\na/b.test\n" + long + "\n",
			after:      map[string]string{"flowy.test": resolver},
			wantStdout: "lodo: 1 resolver files written, 0 removed, 9 lines skipped\n",
		},
		{
			name:       "last line without newline",
			list:       "crm.test\nflowy.test",
			after:      map[string]string{"crm.test": resolver, "flowy.test": resolver},
			wantStdout: "lodo: 2 resolver files written, 0 removed, 0 lines skipped\n",
		},
		{
			name:       "removes its files no longer listed",
			list:       "crm.test\n",
			before:     map[string]string{"old.test": resolver, "crm.test": resolver},
			after:      map[string]string{"crm.test": resolver},
			wantStdout: "lodo: 1 resolver files written, 1 removed, 0 lines skipped\n",
		},
		{
			name:       "empty list removes all its files",
			list:       "",
			before:     map[string]string{"old.test": resolver, "older.test": resolver},
			after:      map[string]string{},
			wantStdout: "lodo: 0 resolver files written, 2 removed, 0 lines skipped\n",
		},
		{
			name: "leaves files it did not write",
			list: "crm.test\n",
			before: map[string]string{
				"crm.test":   "nameserver 10.0.0.1\n",
				"other.test": "nameserver 10.0.0.2\n",
				"local":      "nameserver 127.0.0.1\n",
			},
			after: map[string]string{
				"crm.test":   "nameserver 10.0.0.1\n",
				"other.test": "nameserver 10.0.0.2\n",
				"local":      "nameserver 127.0.0.1\n",
			},
			wantStdout: "lodo: 0 resolver files written, 0 removed, 0 lines skipped\n",
			wantStderr: "crm.test alone: lodo did not write it",
		},
		{
			name:       "list as long as the cap",
			list:       "crm.test\n" + strings.Repeat("x", system.MaxScriptInput-len("crm.test\n")),
			after:      map[string]string{"crm.test": resolver},
			wantStdout: "lodo: 1 resolver files written, 0 removed, 1 lines skipped\n",
		},
		{
			name:       "list longer than the cap",
			list:       "crm.test\n" + strings.Repeat("x", system.MaxScriptInput-len("crm.test\n")+1),
			before:     map[string]string{"old.test": resolver},
			after:      map[string]string{"old.test": resolver},
			wantFail:   true,
			wantStderr: "is longer than",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := paths.ForTest(t.TempDir())
			writeDir(t, p.ResolverDir, tt.before)
			stdout, stderr, err := runScript(t, p, tt.list)
			if tt.wantFail != (err != nil) {
				t.Fatalf("err = %v, wantFail %v; stderr:\n%s", err, tt.wantFail, stderr)
			}
			if !tt.wantFail && stdout != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
			if got := readDir(t, p.ResolverDir); !maps.Equal(got, tt.after) {
				t.Errorf("resolver dir = %v, want %v", got, tt.after)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(p.ResolverDir), "x.test")); !os.IsNotExist(err) {
				t.Error("../x.test escaped the resolver dir")
			}
		})
	}
}

func TestScript_RunFileModes(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	if _, stderr, err := runScript(t, p, "crm.test\n"); err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	info, err := os.Stat(filepath.Join(p.ResolverDir, "crm.test"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 so mDNSResponder can read it", info.Mode().Perm())
	}
}

// TestScript_RunNeverOpensInput feeds the script paths to FIFOs with no
// writer, by absolute path and relative to its working directory. Opening one
// would block until the timeout, so a quick run shows it read neither.
func TestScript_RunNeverOpensInput(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	cwd := t.TempDir()
	abs := filepath.Join(t.TempDir(), "list.test")
	for _, fifo := range []string{abs, filepath.Join(cwd, "crm.test")} {
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	script := filepath.Join(t.TempDir(), "apply-resolvers.sh")
	writeFile(t, script, system.Script(p))
	var out, errOut strings.Builder
	cmd := exec.CommandContext(ctx, "/bin/sh", script)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(abs + "\n/dev/stdin\ncrm.test\n")
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, errOut.String())
	}
	if want := "lodo: 1 resolver files written, 0 removed, 2 lines skipped\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	want := map[string]string{"crm.test": system.ResolverFile()}
	if got := readDir(t, p.ResolverDir); !maps.Equal(got, want) {
		t.Errorf("resolver dir = %v, want %v", got, want)
	}
}

func TestScript_RunQuotedPath(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(filepath.Join(t.TempDir(), "it's here"))
	if _, stderr, err := runScript(t, p, "crm.test\n"); err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	if got := readDir(t, p.ResolverDir); len(got) != 1 {
		t.Errorf("resolver dir = %v, want crm.test", got)
	}
}

func TestScript_RunTwice(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	for range 2 {
		if _, stderr, err := runScript(t, p, "crm.test\nflowy.test\n"); err != nil {
			t.Fatalf("%v: %s", err, stderr)
		}
	}
	want := map[string]string{"crm.test": system.ResolverFile(), "flowy.test": system.ResolverFile()}
	if got := readDir(t, p.ResolverDir); !maps.Equal(got, want) {
		t.Errorf("resolver dir = %v, want %v", got, want)
	}
}

// TestScript_AgreesWithStore checks the script writes exactly the names
// store.ValidateName accepts.
func TestScript_AgreesWithStore(t *testing.T) {
	t.Parallel()

	names := []string{
		"a.test", "1.test", "a--b.test", "xn--bcher-kva.test", "a.b.c.d.test", "a.local.test",
		strings.Repeat("a", 63) + ".test",
		strings.Repeat("a", 64) + ".test",
		strings.Repeat("abc.", 61) + "a.test",     // 250 characters
		strings.Repeat("abc.", 61) + "abcde.test", // 254 characters
		"test", "a-.test", "-a.test", "TEST", "a.TEST", "a.test.", "a..test", ".a.test", "flowy.local",
		"a.tests", "localhost", "a.test\r", "a\tb.test", "a b.test", "é.test", "a/b.test", "..test",
	}
	p := paths.ForTest(t.TempDir())
	if _, stderr, err := runScript(t, p, strings.Join(names, "\n")+"\n"); err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	written := readDir(t, p.ResolverDir)
	for _, n := range names {
		_, gotWritten := written[n]
		wantWritten := store.ValidateName(n) == nil
		if gotWritten != wantWritten {
			t.Errorf("%q: written by script = %v, accepted by store = %v", n, gotWritten, wantWritten)
		}
	}
}

func TestLoopbackPlist(t *testing.T) {
	t.Parallel()

	plist := system.LoopbackPlist()
	golden.RequireEqual(t, plist)

	if _, err := os.Stat("/usr/bin/plutil"); err != nil {
		t.Skip("plutil not available")
	}
	path := filepath.Join(t.TempDir(), "io.lodo.loopback.plist")
	writeFile(t, path, plist)
	if out, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Errorf("plutil -lint: %v: %s", err, out)
	}
}

func TestSudoers(t *testing.T) {
	t.Parallel()

	rule, err := system.Sudoers(fake)
	if err != nil {
		t.Fatal(err)
	}
	golden.RequireEqual(t, rule)

	if _, err := os.Stat("/usr/sbin/visudo"); err != nil {
		t.Skip("visudo not available")
	}
	path := filepath.Join(t.TempDir(), "lodo")
	writeFile(t, path, rule)
	if out, err := exec.Command("/usr/sbin/visudo", "-c", "-f", path).CombinedOutput(); err != nil {
		t.Errorf("visudo -c: %v: %s", err, out)
	}
}

func TestSudoers_RefusesOddUser(t *testing.T) {
	t.Parallel()

	for _, user := range []string{"", "a b", "root,ALL", "x:y", "a\nALL ALL=(ALL) ALL"} {
		p := fake
		p.User = user
		if _, err := system.Sudoers(p); err == nil {
			t.Errorf("Sudoers with user %q: err = nil", user)
		}
	}
}

func TestConfBlock(t *testing.T) {
	t.Parallel()

	want := "# lodo\nconf-file=/fake/Users/tester/.config/lodo/dnsmasq.conf\nlisten-address=127.0.0.1\nport=53535\nbind-interfaces\n"
	if got := system.ConfBlock(fake); got != want {
		t.Errorf("ConfBlock = %q, want %q", got, want)
	}
}

func TestCaddyBlock(t *testing.T) {
	t.Parallel()

	want := "# lodo\nimport /fake/Users/tester/.config/lodo/Caddyfile\n"
	if got := system.CaddyBlock(fake); got != want {
		t.Errorf("CaddyBlock = %q, want %q", got, want)
	}
}

// runScript renders the script for p into a temp file and runs it with /bin/sh.
func runScript(t *testing.T, p paths.Paths, stdin string) (stdout, stderr string, err error) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "apply-resolvers.sh")
	writeFile(t, script, system.Script(p))
	var out, errOut strings.Builder
	cmd := exec.Command("/bin/sh", script)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
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

func writeDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		writeFile(t, filepath.Join(dir, name), content)
	}
}

// readDir returns every entry of dir with its content; a missing dir is empty.
func readDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return got
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got[e.Name()] = string(data)
	}
	return got
}
