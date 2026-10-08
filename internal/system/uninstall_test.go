package system_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

func uninstallCalls(p paths.Paths, withScript, withCaddy bool) []string {
	calls := []string{run.Line(p.Sudo, "-v")}
	if withScript {
		calls = append(calls, run.Line(p.Sudo, "-n", p.Script))
	}
	calls = append(calls, run.Line(p.Brew, "services", "stop", "dnsmasq"))
	if withCaddy {
		calls = append(calls, run.Line(p.Brew, "services", "stop", "caddy"))
	}
	calls = append(calls,
		run.Line(p.Sudo, "-n", p.Launchctl, "bootout", "system/io.lodo.loopback"),
		run.Line(p.Sudo, "-n", p.Rm, "-f", p.LoopbackPlist),
	)
	for i := store.OwnFirst; i <= store.OwnLast; i++ {
		calls = append(calls, run.Line(p.Sudo, "-n", p.Ifconfig, "lo0", "-alias", store.OwnAddress(i)))
	}
	return append(calls,
		run.Line(p.Sudo, "-n", p.Rm, "-f", p.Sudoers, p.Script),
		run.Line(p.Sudo, "-n", p.Rmdir, filepath.Dir(p.Script)),
	)
}

func TestUninstall(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, true)
	writeFile(t, p.SystemCaddyfile, "example.com {\n\trespond \"hi\"\n}\n")
	ctx := context.Background()
	if err := system.Setup(ctx, p, r, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	domains := []store.Domain{{Name: "crm.test", Address: "127.0.1.1", Enabled: true}}
	if err := store.Save(p.DomainsJSON, domains); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.Script, "#!/bin/sh\n") // the fake runner installed nothing

	r = run.NewFake()
	var out strings.Builder
	if err := system.Uninstall(ctx, p, r, &out); err != nil {
		t.Fatalf("Uninstall: %v\n%s", err, out.String())
	}
	assertCalls(t, r.Calls(), uninstallCalls(p, true, true))
	assertFile(t, p.SystemConf, "# dnsmasq\n#port=5353\n")
	assertFile(t, p.SystemCaddyfile, "example.com {\n\trespond \"hi\"\n}\n")
	for _, gone := range []string{p.SystemConfBackup, p.SystemCaddyfileBackup} {
		if exists(gone) {
			t.Errorf("%s still exists", gone)
		}
	}
	if got, err := store.Load(p.DomainsJSON); err != nil || len(got) != 1 {
		t.Errorf("domains.json after uninstall: %v, %v; want it kept", got, err)
	}
	assertFile(t, p.Resolvers, "")
	if !strings.Contains(out.String(), "sudo brew services start dnsmasq") {
		t.Errorf("output lacks the hint to restore the root job:\n%s", out.String())
	}
}

func TestUninstall_WithoutBackups(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, true)
	writeFile(t, p.SystemConf, "server=1.1.1.1\n\n"+system.ConfBlock(p))
	writeFile(t, p.SystemCaddyfile, system.CaddyBlock(p))
	if err := system.Uninstall(context.Background(), p, r, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, p.SystemConf, "server=1.1.1.1\n")
	if exists(p.SystemCaddyfile) {
		t.Error("Caddyfile holding only lodo's import was not removed")
	}
	assertCalls(t, r.Calls(), uninstallCalls(p, false, true))
}

func TestUninstall_NothingInstalled(t *testing.T) {
	t.Parallel()

	p, r := newMac(t, false)
	if err := system.Uninstall(context.Background(), p, r, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, p.SystemConf, "# dnsmasq\n#port=5353\n")
	assertCalls(t, r.Calls(), uninstallCalls(p, false, false))
	if _, err := os.Stat(p.DomainsJSON); !os.IsNotExist(err) {
		t.Errorf("uninstall created domains.json: %v", err)
	}
}
