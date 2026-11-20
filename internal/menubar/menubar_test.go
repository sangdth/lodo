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

func TestInfo(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	r := run.NewFake()
	printLine := run.Line(p.Launchctl, "print", "gui/501/io.lodo.menubar")
	r.Fail(printLine, "Could not find service")

	if st := menubar.Info(t.Context(), r, p); !st.Off() || st.Running || st.Registered {
		t.Errorf("Info when off = %+v, want Off", st)
	}

	writeFile(t, p.MenubarPlist, menubar.Plist(p))
	if st := menubar.Info(t.Context(), r, p); st.Off() || st.Running || !st.Registered {
		t.Errorf("Info when registered but stopped = %+v", st)
	}

	r.Set(printLine, "\tstate = running\n\tpid = 999\n")
	if st := menubar.Info(t.Context(), r, p); st.Off() || !st.Running || st.PID != 999 || !st.Registered {
		t.Errorf("Info when running = %+v, want running pid 999", st)
	}
}

func TestStartAndStop(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	writeFile(t, p.Swiftc, "")
	r := run.NewFake()
	printLine := run.Line(p.Launchctl, "print", "gui/501/io.lodo.menubar")
	r.Fail(printLine, "Could not find service")

	// First start compiles the Swift source and bootstraps the LaunchAgent.
	if err := menubar.Start(t.Context(), p, r); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.MenubarBin, "") // simulate swiftc output
	wantFirst := []string{
		run.Line(p.Swiftc, "-O", p.MenubarBin+".swift", "-o", p.MenubarBin),
		printLine,
		run.Line(p.Launchctl, "bootstrap", "gui/501", p.MenubarPlist),
	}
	if got := r.Calls(); !slices.Equal(got, wantFirst) {
		t.Errorf("first Start calls = %q, want %q", got, wantFirst)
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

	// Stop unloads the job and deletes the plist.
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

	if _, err := os.Stat("/usr/bin/plutil"); err != nil {
		t.Skip("plutil not available")
	}
	p := paths.ForTest(filepath.Join(t.TempDir(), "a & b"))
	plist := filepath.Join(t.TempDir(), "io.lodo.menubar.plist")
	writeFile(t, plist, menubar.Plist(p))
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
