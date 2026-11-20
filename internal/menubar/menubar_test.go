package menubar_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sangdth/lodo/internal/menubar"
	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
)

func TestRunning(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	r := run.NewFake()
	printLine := run.Line(p.Launchctl, "print", "gui/501/io.lodo.menubar")
	r.Fail(printLine, "Could not find service")

	if menubar.Running(t.Context(), r, p) {
		t.Error("Running when not loaded = true, want false")
	}

	r.Set(printLine, "\tstate = not running\n")
	if menubar.Running(t.Context(), r, p) {
		t.Error("Running when loaded without pid = true, want false")
	}

	r.Set(printLine, "\tstate = running\n\tpid = 999\n")
	if !menubar.Running(t.Context(), r, p) {
		t.Error("Running when running pid 999 = false, want true")
	}
}

func TestStartAndStop(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	writeFile(t, p.Swiftc, "")
	r := run.NewFake()
	printLine := run.Line(p.Launchctl, "print", "gui/501/io.lodo.menubar")
	r.Fail(printLine, "Could not find service")

	// First start compiles via temp files and bootstraps the LaunchAgent.
	if err := menubar.Start(t.Context(), p, r); err != nil {
		t.Fatal(err)
	}
	wantFirst := []string{
		run.Line(p.Swiftc, "-O", p.MenubarBin+".tmp.swift", "-o", p.MenubarBin+".tmp"),
		printLine,
		run.Line(p.Launchctl, "bootstrap", "gui/501", p.MenubarPlist),
	}
	if got := r.Calls(); !slices.Equal(got, wantFirst) {
		t.Errorf("first Start calls = %q, want %q", got, wantFirst)
	}
	for _, temp := range []string{p.MenubarBin + ".tmp.swift", p.MenubarBin + ".tmp"} {
		if _, err := os.Stat(temp); !os.IsNotExist(err) {
			t.Errorf("temp file %s left behind: %v", temp, err)
		}
	}

	// Second start when already loaded skips swiftc and kickstarts the job.
	r = run.NewFake()
	r.Set(printLine, "\tstate = running\n\tpid = 999\n")
	if err := menubar.Start(t.Context(), p, r); err != nil {
		t.Fatal(err)
	}
	wantSecond := []string{
		printLine,
		run.Line(p.Launchctl, "kickstart", "-k", "gui/501/io.lodo.menubar"),
	}
	if got := r.Calls(); !slices.Equal(got, wantSecond) {
		t.Errorf("second Start calls = %q, want %q", got, wantSecond)
	}

	// Stop unloads the job first, then deletes the plist.
	r = run.NewFake()
	r.Set(printLine, "\tstate = running\n\tpid = 999\n")
	if err := menubar.Stop(t.Context(), p, r); err != nil {
		t.Fatal(err)
	}
	wantStop := []string{
		printLine,
		run.Line(p.Launchctl, "bootout", "gui/501/io.lodo.menubar"),
	}
	if got := r.Calls(); !slices.Equal(got, wantStop) {
		t.Errorf("Stop calls = %q, want %q", got, wantStop)
	}
	if _, err := os.Stat(p.MenubarPlist); !os.IsNotExist(err) {
		t.Errorf("MenubarPlist still exists after Stop: %v", err)
	}

	// Stop still boots out a loaded job even if the plist was already removed.
	r = run.NewFake()
	r.Set(printLine, "\tstate = running\n\tpid = 999\n")
	if err := menubar.Stop(t.Context(), p, r); err != nil {
		t.Fatal(err)
	}
	if got := r.Calls(); !slices.Equal(got, wantStop) {
		t.Errorf("Stop without plist calls = %q, want %q", got, wantStop)
	}
}

func TestStart_FailedBuildRebuildsNextTime(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	writeFile(t, p.Swiftc, "")
	writeFile(t, p.MenubarBin, "old-binary")
	writeFile(t, p.MenubarBin+".swift", "// old source\n")

	compileLine := run.Line(p.Swiftc, "-O", p.MenubarBin+".tmp.swift", "-o", p.MenubarBin+".tmp")
	r := run.NewFake()
	r.Fail(compileLine, "xcode license not accepted")
	if err := menubar.Start(t.Context(), p, r); err == nil || !strings.Contains(err.Error(), "build menubar") {
		t.Fatalf("failed Start err = %v, want build menubar error", err)
	}
	// Old binary stays intact and source stamp was not updated.
	gotBin, err := os.ReadFile(p.MenubarBin)
	if err != nil || string(gotBin) != "old-binary" {
		t.Errorf("MenubarBin after failed build = %q, %v; want old-binary untouched", gotBin, err)
	}

	// Next Start retries compilation instead of thinking the old binary is up to date.
	r2 := run.NewFake()
	printLine := run.Line(p.Launchctl, "print", "gui/501/io.lodo.menubar")
	r2.Fail(printLine, "Could not find service")
	if err := menubar.Start(t.Context(), p, r2); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r2.Calls(), compileLine) {
		t.Errorf("second Start skipped swiftc after prior failure: %q", r2.Calls())
	}
}

func TestRefresh(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	writeFile(t, p.Swiftc, "")
	r := run.NewFake()

	// Disabled (no plist): no-op.
	if err := menubar.Refresh(t.Context(), p, r); err != nil {
		t.Fatal(err)
	}
	if len(r.Calls()) != 0 {
		t.Errorf("Refresh when disabled ran commands: %q", r.Calls())
	}

	// Enabled and up to date: no-op (no flicker).
	writeFile(t, p.MenubarPlist, menubar.Plist(p))
	writeFile(t, p.MenubarBin, "bin")
	writeFile(t, p.MenubarBin+".swift", string(menubar.Source()))
	if err := menubar.Refresh(t.Context(), p, r); err != nil {
		t.Fatal(err)
	}
	if len(r.Calls()) != 0 {
		t.Errorf("Refresh when up to date ran commands: %q", r.Calls())
	}

	// Enabled with stale source: rebuilds and kickstarts.
	writeFile(t, p.MenubarBin+".swift", "// stale\n")
	printLine := run.Line(p.Launchctl, "print", "gui/501/io.lodo.menubar")
	r.Set(printLine, "\tstate = running\n\tpid = 999\n")
	if err := menubar.Refresh(t.Context(), p, r); err != nil {
		t.Fatal(err)
	}
	want := []string{
		run.Line(p.Swiftc, "-O", p.MenubarBin+".tmp.swift", "-o", p.MenubarBin+".tmp"),
		printLine,
		run.Line(p.Launchctl, "kickstart", "-k", "gui/501/io.lodo.menubar"),
	}
	if got := r.Calls(); !slices.Equal(got, want) {
		t.Errorf("Refresh when stale calls = %q, want %q", got, want)
	}
}

func TestStart_WithoutSwiftc(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	r := run.NewFake()
	err := menubar.Start(t.Context(), p, r)
	if err == nil || !strings.Contains(err.Error(), "swiftc is not installed") {
		t.Errorf("err = %v, want swiftc not installed", err)
	}
}

func TestPlist_PlutilAccepts(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(filepath.Join(t.TempDir(), "a & b"))
	content := menubar.Plist(p)
	if !strings.Contains(content, "<key>SuccessfulExit</key>\n\t\t<false/>") {
		t.Errorf("Plist lacks KeepAlive SuccessfulExit=false:\n%s", content)
	}
	if _, err := os.Stat("/usr/bin/plutil"); err != nil {
		t.Skip("plutil not available")
	}
	plist := filepath.Join(t.TempDir(), "io.lodo.menubar.plist")
	writeFile(t, plist, content)
	if out, err := exec.Command("/usr/bin/plutil", "-lint", plist).CombinedOutput(); err != nil {
		t.Errorf("plutil -lint: %v: %s", err, out)
	}
}

func TestSwiftSelfTests(t *testing.T) {
	t.Parallel()

	swiftc, err := exec.LookPath("/usr/bin/swiftc")
	if err != nil {
		t.Skipf("swiftc not installed: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	src := filepath.Join(dir, "menubar.swift")
	bin := filepath.Join(dir, "lodo-menubar")
	writeFile(t, src, string(menubar.Source()))
	if out, err := exec.CommandContext(ctx, swiftc, src, "-o", bin).CombinedOutput(); err != nil {
		t.Fatalf("swiftc compile: %v\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, bin, "--test").CombinedOutput(); err != nil {
		t.Fatalf("lodo-menubar --test: %v\n%s", err, out)
	}
	cmd := exec.CommandContext(ctx, bin, "--dump")
	cmd.Env = append(os.Environ(), "LODO_DOMAINS_JSON="+filepath.Join(dir, "missing.json"))
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "No running services\n" {
		t.Fatalf("lodo-menubar --dump (empty) = %q, %v; want No running services", out, err)
	}
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
