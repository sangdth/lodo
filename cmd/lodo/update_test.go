package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
)

// newUpdateApp returns an app on temp paths that runs go from /opt/bin, reports
// cur as its version, and lives in bin, where a release installs, so nothing
// shadows it. It returns that folder too.
func newUpdateApp(t *testing.T, cur string) (a app, p paths.Paths, r *run.Fake, out *strings.Builder, bin string) {
	t.Helper()
	p, r, out = paths.ForTest(t.TempDir()), run.NewFake(), &strings.Builder{}
	bin = filepath.Join(t.TempDir(), "bin")
	a = newApp(p, r, out, out)
	a.lookPath = func(file string) (string, error) { return "/opt/bin/" + file, nil }
	a.version = func() string { return cur }
	a.execPath = func() (string, error) { return filepath.Join(bin, "lodo"), nil }
	r.Set(goEnv(), bin+"\n"+filepath.Join(p.Home, "go"))
	return a, p, r, out, bin
}

// goRun is the command line of go, run from the folder update reads the
// toolchain's settings in.
func goRun(args ...string) string {
	return run.Line("/opt/bin/go", append([]string{"-C", os.TempDir()}, args...)...)
}

// listLodo is the command line that reads the latest published version.
func listLodo() string { return goRun("list", "-m", "-f", "{{.Version}}", latestTarget()) }

// installLodo is the command line that builds version v.
func installLodo(v string) string { return goRun("install", installTarget(v)) }

// goEnv is the command line that reads where go install writes: GOBIN, then
// GOPATH.
func goEnv() string { return goRun("env", "GOBIN", "GOPATH") }

func TestApp_Update(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cur         string // what lodo reports as its version
		latest      string // what go list prints
		noGo        bool   // go isn't on PATH
		listErr     string // go list fails with this
		instErr     string // go install fails with this
		doctorErr   bool   // the new lodo's doctor can't run
		setup       bool   // lodo setup has run
		wantCode    int
		wantOut     []string
		wantErr     string
		wantDoctor  bool // the new lodo's doctor must run
		wantInstall bool
	}{
		{
			name: "up to date", cur: "v0.1.0", latest: "v0.1.0\n",
			wantOut: []string{"lodo v0.1.0 is up to date\n"},
		},
		{
			name: "installs the latest", cur: "v0.1.0", latest: "v0.2.0\n",
			wantOut: []string{"updated v0.1.0 -> v0.2.0\n"}, wantInstall: true,
		},
		{
			name: "a dev build updates", cur: "dev", latest: "v0.2.0\n",
			wantOut: []string{"updated dev -> v0.2.0\n"}, wantInstall: true,
		},
		{
			name: "a set-up Mac is checked", cur: "v0.1.0", latest: "v0.2.0\n", setup: true,
			wantOut:     []string{"updated v0.1.0 -> v0.2.0\n", "\n✓ 1 dnsmasq  running\n"},
			wantInstall: true, wantDoctor: true,
		},
		{
			name: "the new lodo's doctor can't run", cur: "v0.1.0", latest: "v0.2.0\n", setup: true, doctorErr: true,
			wantOut: []string{"updated v0.1.0 -> v0.2.0\n"}, wantErr: "warn  ",
			wantInstall: true, wantDoctor: true,
		},
		{
			name: "refuses a dirty build", cur: "v0.1.0+dirty", latest: "v0.2.0\n", wantCode: 1,
			wantErr: "built from a modified checkout: go install ./cmd/lodo from the repo",
		},
		{
			name: "go list prints nothing", cur: "v0.1.0", latest: "\n", wantCode: 1,
			wantErr: "read the latest version: go printed none",
		},
		{
			name: "without go", cur: "v0.1.0", noGo: true, wantCode: 1,
			wantErr: "brew install go, then lodo update",
		},
		{
			name: "go list fails", cur: "v0.1.0", listErr: "dial tcp: no such host", wantCode: 1,
			wantErr: "read the latest version: ",
		},
		{
			name: "go install fails", cur: "v0.1.0", latest: "v0.2.0\n",
			instErr: "build constraints exclude all Go files", wantCode: 1,
			wantErr: "install lodo v0.2.0: ", wantInstall: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, p, r, out, bin := newUpdateApp(t, tt.cur)
			if tt.noGo {
				a.lookPath = func(string) (string, error) {
					return "", errors.New(`exec: "go": executable file not found in $PATH`)
				}
			}
			if tt.setup {
				setUpFakeMac(t, p, r)
			}
			doctor := run.Line(filepath.Join(bin, "lodo"), "doctor")
			r.Set(listLodo(), tt.latest)
			r.Set(doctor, "✓ 1 dnsmasq  running\n")
			if tt.listErr != "" {
				r.Fail(listLodo(), tt.listErr)
			}
			if tt.instErr != "" {
				r.Fail(installLodo("v0.2.0"), tt.instErr)
			}
			if tt.doctorErr {
				r.Fail(doctor, "")
			}

			if code := a.command(t.Context(), "update", options{}); code != tt.wantCode {
				t.Errorf("exit code = %d, want %d:\n%s", code, tt.wantCode, out.String())
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
			if tt.wantErr != "" && !strings.Contains(out.String(), tt.wantErr) {
				t.Errorf("output lacks %q:\n%s", tt.wantErr, out.String())
			}
			if got := slices.Contains(r.Calls(), installLodo("v0.2.0")); got != tt.wantInstall {
				t.Errorf("go install built the reported version = %v, want %v; calls: %q", got, tt.wantInstall, r.Calls())
			}
			if got := slices.Contains(r.Calls(), doctor); got != tt.wantDoctor {
				t.Errorf("the new lodo's doctor ran = %v, want %v; calls: %q", got, tt.wantDoctor, r.Calls())
			}
		})
	}
}

// TestApp_UpdateInstallDir checks that an install somewhere other than where
// lodo runs says so, and that the folder comes from GOBIN, else GOPATH's first
// entry, else the home folder.
func TestApp_UpdateInstallDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		gobin  string // go env GOBIN
		gopath string // go env GOPATH
		self   string // where the running lodo is
		bin    string // where the new lodo is; empty means GOPATH's first entry, or the home folder
		want   string // the warning the output must hold; empty for none
	}{
		{
			name: "the same folder", gobin: "/opt/go/bin", bin: "/opt/go/bin", self: "/opt/go/bin/lodo",
		},
		{
			name: "another copy before it on PATH", gobin: "/opt/go/bin", bin: "/opt/go/bin", self: "/usr/local/bin/lodo",
			want: "warn  lodo runs from /usr/local/bin/lodo, not the new /opt/go/bin/lodo: put /opt/go/bin on PATH before it\n",
		},
		{
			name: "GOBIN unset, GOPATH's first entry", gopath: "/opt/gopath:/other", bin: "/opt/gopath/bin",
			self: "/opt/gopath/bin/lodo",
		},
		{
			name: "GOBIN and GOPATH unset", bin: "HOME/go/bin", self: "HOME/go/bin/lodo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, p, r, out, _ := newUpdateApp(t, "v0.1.0")
			bin := strings.ReplaceAll(tt.bin, "HOME", p.Home)
			self := strings.ReplaceAll(tt.self, "HOME", p.Home)
			a.execPath = func() (string, error) { return self, nil }
			r.Set(listLodo(), "v0.2.0\n")
			r.Set(goEnv(), tt.gobin+"\n"+tt.gopath)

			if code := a.update(t.Context()); code != 0 {
				t.Fatalf("exit code = %d, want 0:\n%s", code, out.String())
			}
			want := strings.ReplaceAll(tt.want, "PATHDIR", bin)
			if want == "" {
				if strings.Contains(out.String(), "warn") {
					t.Errorf("warned although lodo runs from the new copy:\n%s", out.String())
				}
				return
			}
			if !strings.Contains(out.String(), want) {
				t.Errorf("output lacks %q:\n%s", want, out.String())
			}
		})
	}
}
