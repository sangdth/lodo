package system_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/caddy"
	"github.com/sangdth/lodo/internal/dnsmasq"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

const block = "# lodo\nconf-file=/fake/Users/tester/.config/lodo/dnsmasq.conf\nlisten-address=127.0.0.1\nport=53535\nbind-interfaces\n"

func TestRewriteSystemConf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		old  string
		want string
	}{
		{name: "empty file", old: "", want: block},
		{name: "only comments", old: "# dnsmasq\n#port=5353\n", want: "# dnsmasq\n#port=5353\n\n" + block},
		{
			name: "replaces the options lodo owns",
			old:  "no-resolv\nport=53\nlisten-address=127.0.0.1,10.0.0.1\n  conf-file = /old.conf\nbind-dynamic\nserver=1.1.1.1\n\n\n",
			want: "no-resolv\nserver=1.1.1.1\n\n" + block,
		},
		{name: "already lodo's", old: "# dnsmasq\n\n" + block, want: "# dnsmasq\n\n" + block},
		{name: "lodo's block in the middle", old: "a=1\n" + block + "b=2\n", want: "a=1\nb=2\n\n" + block},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := system.RewriteSystemConf(tt.old, fake)
			if got != tt.want {
				t.Errorf("RewriteSystemConf =\n%q\nwant\n%q", got, tt.want)
			}
			if again := system.RewriteSystemConf(got, fake); again != got {
				t.Errorf("not idempotent: second rewrite =\n%q", again)
			}
		})
	}
}

func TestRewriteSystemConf_ThisMac(t *testing.T) {
	t.Parallel()

	old, err := os.ReadFile("testdata/this-mac-dnsmasq.conf")
	if err != nil {
		t.Fatal(err)
	}
	golden.RequireEqual(t, system.RewriteSystemConf(string(old), fake))
}

func TestStripSystemConf(t *testing.T) {
	t.Parallel()

	old := "# dnsmasq\nserver=1.1.1.1\n\n" + block
	if got, want := system.StripSystemConf(old), "# dnsmasq\nserver=1.1.1.1\n"; got != want {
		t.Errorf("StripSystemConf = %q, want %q", got, want)
	}
	if got := system.StripSystemConf(block); got != "" {
		t.Errorf("StripSystemConf of lodo's block alone = %q, want empty", got)
	}
}

func TestRewriteSystemCaddyfile(t *testing.T) {
	t.Parallel()

	caddyBlock := system.CaddyBlock(fake)
	global := system.CaddyGlobalBlock
	site := "example.com {\n\trespond \"hi\"\n}\n"
	userGlobal := "# mine\n{\n\temail me@example.com\n}\n\n" + site
	tests := []struct {
		name string
		old  string
		want string
	}{
		{name: "no file", old: "", want: global + "\n" + caddyBlock},
		{name: "keeps other sites", old: site, want: global + "\n" + site + "\n" + caddyBlock},
		{name: "already lodo's", old: global + "\n" + site + "\n" + caddyBlock, want: global + "\n" + site + "\n" + caddyBlock},
		{name: "moves the import to the end", old: caddyBlock + "\n" + site, want: global + "\n" + site + "\n" + caddyBlock},
		{name: "moves the global block to the top", old: site + "\n" + global, want: global + "\n" + site + "\n" + caddyBlock},
		{name: "keeps the user's global block", old: userGlobal, want: userGlobal + "\n" + caddyBlock},
		{name: "user's global block after lodo's", old: global + "\n" + userGlobal, want: userGlobal + "\n" + caddyBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := system.RewriteSystemCaddyfile(tt.old, fake)
			if got != tt.want {
				t.Errorf("RewriteSystemCaddyfile =\n%q\nwant\n%q", got, tt.want)
			}
			if again := system.RewriteSystemCaddyfile(got, fake); again != got {
				t.Errorf("not idempotent: second rewrite =\n%q", again)
			}
		})
	}
}

func TestStripSystemCaddyfile(t *testing.T) {
	t.Parallel()

	caddyBlock := system.CaddyBlock(fake)
	global := system.CaddyGlobalBlock
	site := "example.com {\n\trespond \"hi\"\n}\n"
	userGlobal := "{\n\temail me@example.com\n}\n"
	tests := []struct {
		name string
		old  string
		want string
	}{
		{name: "import block", old: site + "\n" + caddyBlock, want: site},
		{name: "lodo's blocks alone", old: global + "\n" + caddyBlock, want: ""},
		{name: "both blocks around a site", old: global + "\n" + site + "\n" + caddyBlock, want: site},
		{name: "keeps the user's global block", old: userGlobal + "\n" + site + "\n" + caddyBlock, want: userGlobal + "\n" + site},
		{name: "marker before a site keeps the site", old: "# lodo\n" + site, want: site},
		{name: "unclosed block after a marker stays", old: "# lodo\n{\n\tadmin off\n", want: "{\n\tadmin off\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := system.StripSystemCaddyfile(tt.old, fake); got != tt.want {
				t.Errorf("StripSystemCaddyfile =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// TestSystemCaddyfile_CaddyAccepts runs caddy adapt on rewritten Caddyfiles,
// which checks lodo's global block parses and comes first. HOME and the XDG
// directories point at a temp dir, so caddy writes nothing real.
func TestSystemCaddyfile_CaddyAccepts(t *testing.T) {
	t.Parallel()

	const caddyBin = "/opt/homebrew/bin/caddy"
	if _, err := exec.LookPath(caddyBin); err != nil {
		t.Skipf("caddy is not installed: %v", err)
	}
	for _, old := range []string{"", "example.com {\n\trespond \"hi\"\n}\n", "{\n\temail me@example.com\n}\n"} {
		dir := t.TempDir()
		p := paths.ForTest(dir)
		writeFile(t, p.Caddyfile, "")
		writeFile(t, p.SystemCaddyfile, system.RewriteSystemCaddyfile(old, p))
		cmd := exec.CommandContext(t.Context(), caddyBin, "adapt", "--config", p.SystemCaddyfile, "--adapter", "caddyfile")
		cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_DATA_HOME="+dir, "XDG_CONFIG_HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("caddy adapt of %q: %v\n%s", old, err, out)
		}
		if old == "" && !strings.Contains(string(out), `"admin":{"disabled":true}`) {
			t.Errorf("caddy adapt of lodo's Caddyfile does not turn admin off:\n%s", out)
		}
	}
}

var sample = []store.Domain{
	{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
	{Name: "dashboard.crm.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
	{Name: "flowy.test", Address: "127.0.1.3", Enabled: true},
	{Name: "old.test", Address: "127.0.0.1"},
}

func TestWriteFiles(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	c, err := system.WriteFiles(p, sample)
	if err != nil {
		t.Fatal(err)
	}
	if c != (system.Changes{Dnsmasq: true, Resolvers: true, Caddy: true}) {
		t.Errorf("first write: changes = %+v, want all", c)
	}
	for path, want := range map[string]string{
		p.DnsmasqConf: dnsmasq.Config(sample, p.Log),
		p.Resolvers:   dnsmasq.ResolverList(sample),
		p.Caddyfile:   caddy.Config(sample),
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s =\n%s\nwant\n%s", path, got, want)
		}
	}

	steps := []struct {
		name    string
		domains []store.Domain
		want    system.Changes
	}{
		{name: "same list", domains: sample, want: system.Changes{}},
		{name: "port change", domains: withPort(sample, "dashboard.crm.test", 3001), want: system.Changes{Caddy: true}},
		{name: "toggle", domains: toggled(withPort(sample, "dashboard.crm.test", 3001), "crm.test"), want: system.Changes{Dnsmasq: true, Resolvers: true}},
	}
	for _, s := range steps {
		c, err := system.WriteFiles(p, s.domains)
		if err != nil {
			t.Fatal(err)
		}
		if c != s.want {
			t.Errorf("%s: changes = %+v, want %+v", s.name, c, s.want)
		}
	}
}

// TestSystemConf_DnsmasqAccepts runs the real dnsmasq syntax check on the
// rewritten Homebrew config together with lodo's generated config.
func TestSystemConf_DnsmasqAccepts(t *testing.T) {
	t.Parallel()

	const bin = "/opt/homebrew/opt/dnsmasq/sbin/dnsmasq"
	if _, err := os.Stat(bin); err != nil {
		t.Skip("dnsmasq not installed")
	}
	p := paths.ForTest(t.TempDir())
	if _, err := system.WriteFiles(p, sample); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile("testdata/this-mac-dnsmasq.conf")
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "dnsmasq.conf")
	writeFile(t, conf, system.RewriteSystemConf(string(old), p))
	if out, err := exec.Command(bin, "--test", "--conf-file="+conf).CombinedOutput(); err != nil {
		t.Errorf("dnsmasq --test: %v: %s", err, out)
	}
}

func withPort(ds []store.Domain, name string, port int) []store.Domain {
	out := append([]store.Domain(nil), ds...)
	for i := range out {
		if out[i].Name == name {
			out[i].Port = port
		}
	}
	return out
}

func toggled(ds []store.Domain, name string) []store.Domain {
	out, err := store.Toggle(ds, name)
	if err != nil {
		panic(err)
	}
	return out
}
