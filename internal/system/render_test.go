package system_test

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/store"
	"github.com/sangdth/oo/internal/system"
)

// fake is a fixed set of paths for golden files.
var fake = paths.ForTest("/fake")

func TestResolverFile(t *testing.T) {
	t.Parallel()

	if got, want := system.ResolverFile(), "# oo\nnameserver 127.0.0.1\nport 53535\n"; got != want {
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
	long := strings.Repeat("abc.", 63) + "oo" // matches the pattern, 254 characters
	tests := []struct {
		name       string
		list       *string           // nil: no list file
		before     map[string]string // resolver dir content before the run
		after      map[string]string // resolver dir content after the run
		wantStdout string            // checked when the script succeeds
		wantStderr string            // substring
		wantFail   bool
	}{
		{
			name:       "writes valid names",
			list:       ptr("crm.oo\ntest.crm.oo\n"),
			after:      map[string]string{"crm.oo": resolver, "test.crm.oo": resolver},
			wantStdout: "oo: 2 resolver files written, 0 removed, 0 lines skipped\n",
		},
		{
			name:       "skips invalid lines",
			list:       ptr("oo\n../x.oo\nX.OO\nx.com\n\nx.oo \n-x.oo\nflowy.oo\na/b.oo\n" + long + "\n"),
			after:      map[string]string{"flowy.oo": resolver},
			wantStdout: "oo: 1 resolver files written, 0 removed, 9 lines skipped\n",
		},
		{
			name:       "last line without newline",
			list:       ptr("crm.oo\nflowy.oo"),
			after:      map[string]string{"crm.oo": resolver, "flowy.oo": resolver},
			wantStdout: "oo: 2 resolver files written, 0 removed, 0 lines skipped\n",
		},
		{
			name:       "removes its files no longer listed",
			list:       ptr("crm.oo\n"),
			before:     map[string]string{"old.oo": resolver, "crm.oo": resolver},
			after:      map[string]string{"crm.oo": resolver},
			wantStdout: "oo: 1 resolver files written, 1 removed, 0 lines skipped\n",
		},
		{
			name:       "empty list removes all its files",
			list:       ptr(""),
			before:     map[string]string{"old.oo": resolver, "older.oo": resolver},
			after:      map[string]string{},
			wantStdout: "oo: 0 resolver files written, 2 removed, 0 lines skipped\n",
		},
		{
			name: "leaves files it did not write",
			list: ptr("crm.oo\n"),
			before: map[string]string{
				"crm.oo":   "nameserver 10.0.0.1\n",
				"other.oo": "nameserver 10.0.0.2\n",
				"local":    "nameserver 127.0.0.1\n",
			},
			after: map[string]string{
				"crm.oo":   "nameserver 10.0.0.1\n",
				"other.oo": "nameserver 10.0.0.2\n",
				"local":    "nameserver 127.0.0.1\n",
			},
			wantStdout: "oo: 0 resolver files written, 0 removed, 0 lines skipped\n",
			wantStderr: "crm.oo alone: oo did not write it",
		},
		{
			name:       "missing list",
			list:       nil,
			before:     map[string]string{"old.oo": resolver},
			after:      map[string]string{"old.oo": resolver},
			wantFail:   true,
			wantStderr: "is missing or not a regular file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := paths.ForTest(t.TempDir())
			writeDir(t, p.ResolverDir, tt.before)
			if tt.list != nil {
				writeFile(t, p.Resolvers, *tt.list)
			}
			stdout, stderr, err := runScript(t, p)
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
			if _, err := os.Stat(filepath.Join(filepath.Dir(p.ResolverDir), "x.oo")); !os.IsNotExist(err) {
				t.Error("../x.oo escaped the resolver dir")
			}
		})
	}
}

func TestScript_RunFileModes(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	writeFile(t, p.Resolvers, "crm.oo\n")
	if _, stderr, err := runScript(t, p); err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	info, err := os.Stat(filepath.Join(p.ResolverDir, "crm.oo"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 so mDNSResponder can read it", info.Mode().Perm())
	}
}

func TestScript_RunSymlinkedList(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	target := filepath.Join(t.TempDir(), "secret")
	writeFile(t, target, "crm.oo\n")
	if err := os.MkdirAll(filepath.Dir(p.Resolvers), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p.Resolvers); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runScript(t, p)
	if err == nil {
		t.Fatal("err = nil, want the script to refuse a symlinked list")
	}
	if !strings.Contains(stderr, "is missing or not a regular file") {
		t.Errorf("stderr = %q", stderr)
	}
	if got := readDir(t, p.ResolverDir); len(got) != 0 {
		t.Errorf("resolver dir = %v, want nothing written", got)
	}
}

func TestScript_RunQuotedPath(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(filepath.Join(t.TempDir(), "it's here"))
	writeFile(t, p.Resolvers, "crm.oo\n")
	if _, stderr, err := runScript(t, p); err != nil {
		t.Fatalf("%v: %s", err, stderr)
	}
	if got := readDir(t, p.ResolverDir); len(got) != 1 {
		t.Errorf("resolver dir = %v, want crm.oo", got)
	}
}

func TestScript_RunTwice(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	writeFile(t, p.Resolvers, "crm.oo\nflowy.oo\n")
	for range 2 {
		if _, stderr, err := runScript(t, p); err != nil {
			t.Fatalf("%v: %s", err, stderr)
		}
	}
	want := map[string]string{"crm.oo": system.ResolverFile(), "flowy.oo": system.ResolverFile()}
	if got := readDir(t, p.ResolverDir); !maps.Equal(got, want) {
		t.Errorf("resolver dir = %v, want %v", got, want)
	}
}

// TestScript_AgreesWithStore checks the script writes exactly the names
// store.ValidateName accepts.
func TestScript_AgreesWithStore(t *testing.T) {
	t.Parallel()

	names := []string{
		"a.oo", "1.oo", "a--b.oo", "xn--bcher-kva.oo", "a.b.c.d.oo", "a.local.oo",
		strings.Repeat("a", 63) + ".oo",
		strings.Repeat("a", 64) + ".oo",
		strings.Repeat("abc.", 62) + "oo", // 250 characters
		strings.Repeat("abc.", 63) + "oo", // 254 characters
		"oo", "a-.oo", "-a.oo", "OO", "a.OO", "a.oo.", "a..oo", ".a.oo", "flowy.local",
		"a.oos", "localhost", "a.oo\r", "a\tb.oo", "a b.oo", "é.oo", "a/b.oo", "..oo",
	}
	p := paths.ForTest(t.TempDir())
	writeFile(t, p.Resolvers, strings.Join(names, "\n")+"\n")
	if _, stderr, err := runScript(t, p); err != nil {
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
	path := filepath.Join(t.TempDir(), "io.oo.loopback.plist")
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
	path := filepath.Join(t.TempDir(), "oo")
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

	want := "# oo\nconf-file=/fake/Users/tester/.config/oo/dnsmasq.conf\nlisten-address=127.0.0.1\nport=53535\nbind-interfaces\n"
	if got := system.ConfBlock(fake); got != want {
		t.Errorf("ConfBlock = %q, want %q", got, want)
	}
}

func TestCaddyBlock(t *testing.T) {
	t.Parallel()

	want := "# oo\nimport /fake/Users/tester/.config/oo/Caddyfile\n"
	if got := system.CaddyBlock(fake); got != want {
		t.Errorf("CaddyBlock = %q, want %q", got, want)
	}
}

// runScript renders the script for p into a temp file and runs it with /bin/sh.
func runScript(t *testing.T, p paths.Paths) (stdout, stderr string, err error) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "apply-resolvers.sh")
	writeFile(t, script, system.Script(p))
	var out, errOut strings.Builder
	cmd := exec.Command("/bin/sh", script)
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

func ptr(s string) *string { return &s }
