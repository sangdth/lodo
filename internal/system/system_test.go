package system_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/oo/internal/caddy"
	"github.com/sangdth/oo/internal/dnsmasq"
	"github.com/sangdth/oo/internal/paths"
	"github.com/sangdth/oo/internal/store"
	"github.com/sangdth/oo/internal/system"
)

const block = "# oo\nconf-file=/fake/Users/tester/.config/oo/dnsmasq.conf\nlisten-address=127.0.0.1\nport=53535\nbind-interfaces\n"

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
			name: "replaces the options oo owns",
			old:  "no-resolv\nport=53\nlisten-address=127.0.0.1,10.0.0.1\n  conf-file = /old.conf\nbind-dynamic\nserver=1.1.1.1\n\n\n",
			want: "no-resolv\nserver=1.1.1.1\n\n" + block,
		},
		{name: "already oo's", old: "# dnsmasq\n\n" + block, want: "# dnsmasq\n\n" + block},
		{name: "oo's block in the middle", old: "a=1\n" + block + "b=2\n", want: "a=1\nb=2\n\n" + block},
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
		t.Errorf("StripSystemConf of oo's block alone = %q, want empty", got)
	}
}

func TestRewriteSystemCaddyfile(t *testing.T) {
	t.Parallel()

	caddyBlock := system.CaddyBlock(fake)
	site := "example.com {\n\trespond \"hi\"\n}\n"
	tests := []struct {
		name string
		old  string
		want string
	}{
		{name: "no file", old: "", want: caddyBlock},
		{name: "keeps other sites", old: site, want: site + "\n" + caddyBlock},
		{name: "already oo's", old: site + "\n" + caddyBlock, want: site + "\n" + caddyBlock},
		{name: "moves the import to the end", old: caddyBlock + "\n" + site, want: site + "\n" + caddyBlock},
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

	site := "example.com {\n\trespond \"hi\"\n}\n"
	if got := system.StripSystemCaddyfile(site+"\n"+system.CaddyBlock(fake), fake); got != site {
		t.Errorf("StripSystemCaddyfile = %q, want %q", got, site)
	}
	if got := system.StripSystemCaddyfile(system.CaddyBlock(fake), fake); got != "" {
		t.Errorf("StripSystemCaddyfile of oo's block alone = %q, want empty", got)
	}
}

var sample = []store.Domain{
	{Name: "crm.oo", Address: "127.0.1.1", Enabled: true},
	{Name: "dashboard.crm.oo", Address: "127.0.1.1", Port: 3000, Enabled: true},
	{Name: "flowy.oo", Address: "127.0.1.3", Enabled: true},
	{Name: "old.oo", Address: "127.0.0.1"},
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
		{name: "port change", domains: withPort(sample, "dashboard.crm.oo", 3001), want: system.Changes{Caddy: true}},
		{name: "toggle", domains: toggled(withPort(sample, "dashboard.crm.oo", 3001), "crm.oo"), want: system.Changes{Dnsmasq: true, Resolvers: true}},
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
// rewritten Homebrew config together with oo's generated config.
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
