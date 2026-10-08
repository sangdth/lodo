package dnsmasq_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lcd/internal/dnsmasq"
	"github.com/sangdth/lcd/internal/store"
)

// dnsmasqBin is Homebrew's dnsmasq. The test that runs it skips when it is
// missing.
const dnsmasqBin = "/opt/homebrew/opt/dnsmasq/sbin/dnsmasq"

// logPath is fixed so the goldens stay the same from run to run.
const logPath = "/Users/tester/.config/lcd/dnsmasq.log"

// lists are the domain lists every test here renders. The sample is out of
// order on purpose; sorted and without the disabled old.lcd it is crm.lcd,
// api.crm.lcd, a.api.crm.lcd, dashboard.crm.lcd, test.crm.lcd,
// flowy.lcd.
var lists = []struct {
	name    string
	domains []store.Domain
}{
	{
		name: "sample",
		domains: []store.Domain{
			{Name: "flowy.lcd", Address: "127.0.1.3", Enabled: true},
			{Name: "dashboard.crm.lcd", Address: "127.0.1.1", Port: 3000, Enabled: true},
			{Name: "old.lcd", Address: "127.0.0.1"},
			{Name: "a.api.crm.lcd", Address: "127.0.1.1", Enabled: true},
			{Name: "crm.lcd", Address: "127.0.1.1", Enabled: true},
			{Name: "test.crm.lcd", Address: "127.0.1.4", Enabled: true},
			{Name: "api.crm.lcd", Address: "127.0.1.1", Enabled: true},
		},
	},
	{name: "empty"},
}

func TestConfig(t *testing.T) {
	t.Parallel()

	for _, tt := range lists {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			golden.RequireEqual(t, dnsmasq.Config(tt.domains, logPath))
		})
	}
}

// TestConfig_DnsmasqAccepts runs the real dnsmasq's syntax check on each
// generated config.
func TestConfig_DnsmasqAccepts(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath(dnsmasqBin); err != nil {
		t.Skipf("dnsmasq is not installed: %v", err)
	}
	for _, tt := range lists {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			conf := filepath.Join(dir, "dnsmasq.conf")
			config := dnsmasq.Config(tt.domains, filepath.Join(dir, "dnsmasq.log"))
			if err := os.WriteFile(conf, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), dnsmasqBin, "--test", "--conf-file="+conf)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("dnsmasq rejects the config: %v\n%s\nconfig:\n%s", err, out, config)
			}
		})
	}
}

func TestResolverList(t *testing.T) {
	t.Parallel()

	for _, tt := range lists {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			golden.RequireEqual(t, dnsmasq.ResolverList(tt.domains))
		})
	}
}
