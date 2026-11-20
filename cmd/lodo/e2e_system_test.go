//go:build e2e

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sangdth/lodo/internal/menubar"
	"github.com/sangdth/lodo/internal/paths"
)

// TestSystemE2E builds the real lodo binary and runs the full macOS lifecycle:
// setup -> dev -y (through macOS /etc/resolver, dnsmasq, Caddy, and bind.cjs)
// -> doctor -> apply -> exec -> prune -> uninstall.
//
// Because it modifies /etc/resolver, /Library/LaunchDaemons, lo0, and Homebrew
// services, it only runs when LODO_E2E_DESTRUCTIVE=1 is set (such as on a
// disposable GitHub Actions macos-latest runner).
func TestSystemE2E(t *testing.T) {
	if os.Getenv("LODO_E2E_DESTRUCTIVE") != "1" {
		t.Skip("skipping destructive system e2e test: set LODO_E2E_DESTRUCTIVE=1 on a disposable macOS VM")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	p, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(t.TempDir(), "lodo")
	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		_ = exec.CommandContext(cleanCtx, bin, "uninstall").Run()
	})

	// 1. lodo setup
	setupOut := runLodo(t, ctx, "", bin, "setup")
	for _, want := range []string{
		"✓ resolver script installed",
		"✓ sudoers rule installed",
		"✓ sudo runs the resolver script without a password",
		"✓ 1 dnsmasq",
	} {
		if !strings.Contains(setupOut, want) {
			t.Errorf("lodo setup output lacks %q:\n%s", want, setupOut)
		}
	}

	// 1b. lodo menubar compiles the helper and starts its LaunchAgent.
	menubarTarget := fmt.Sprintf("gui/%d/%s", p.UID, menubar.Label)
	if out := runLodo(t, ctx, "", bin, "menubar"); strings.TrimSpace(out) != "menubar running" {
		t.Errorf("lodo menubar output = %q, want menubar running", out)
	}
	if _, err := os.Stat(p.MenubarPlist); err != nil {
		t.Errorf("MenubarPlist missing after lodo menubar: %v", err)
	}
	if out, err := exec.CommandContext(ctx, p.Launchctl, "print", menubarTarget).CombinedOutput(); err != nil {
		t.Errorf("launchctl print %s after lodo menubar: %v\n%s", menubarTarget, err, out)
	}

	// 2. lodo dev -y in a real Node project, verifying macOS DNS + Caddy + bind.cjs + doctor + apply + menubar
	appPort := e2eFreeTCPPort(t, "127.0.0.1")
	resultFile := filepath.Join(t.TempDir(), "result.json")
	serverJS := fmt.Sprintf(`const http = require("node:http");
const fs = require("node:fs");
const { execFile } = require("node:child_process");
const { promisify } = require("node:util");

const execFileAsync = promisify(execFile);
const bin = %q;
const menubarBin = %q;
const resultFile = %q;

const srv = http.createServer((req, res) => {
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify({
    host: req.headers.host,
    localAddress: req.socket.localAddress,
    dockerHostIP: process.env.DOCKER_HOST_IP,
    apiURL: process.env.API_URL,
    databaseURL: process.env.DATABASE_URL,
    viteHosts: process.env.__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS,
  }));
});

srv.listen(%d, "127.0.0.1", async () => {
  let lastErr = "";
  for (let i = 0; i < 30; i++) {
    try {
      const resp = await fetch("http://web.shop.lodo/");
      if (resp.ok) {
        const body = await resp.json();
        const { stdout: doctorOut } = await execFileAsync(bin, ["doctor"]);
        const { stdout: applyOut } = await execFileAsync(bin, ["apply"]);
        const { stdout: menubarDump } = await execFileAsync(menubarBin, ["--dump"]);
        fs.writeFileSync(resultFile, JSON.stringify({
          status: resp.status,
          via: resp.headers.get("via") || "",
          ...body,
          doctorOut,
          applyOut,
          menubarDump,
        }));
        srv.close(() => process.exit(0));
        return;
      }
      lastErr = "HTTP " + resp.status;
    } catch (err) {
      lastErr = [err && err.message, err && err.cause && err.cause.message, err && err.stdout, err && err.stderr].filter(Boolean).join("\n");
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  console.error("server.js failed:", lastErr);
  process.exit(1);
});
`, bin, p.MenubarBin, resultFile, appPort)

	root := writeTree(t, map[string]string{
		".git/HEAD":             "ref: refs/heads/master\n",
		"package.json":          `{"workspaces": ["apps/*"], "scripts": {"dev": "node server.js"}}`,
		"apps/web/package.json": fmt.Sprintf(`{"scripts": {"dev": "next dev -p %d"}}`, appPort),
		"apps/web/.env":         fmt.Sprintf("API_URL=http://localhost:%d\nDATABASE_URL=postgres://u:hunter2@localhost:5432/shop\n", appPort),
		"compose.yaml":          `services: {db: {image: postgres, ports: ["${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"]}}`,
		"server.js":             serverJS,
	})

	// /usr/bin/script gives Bubble Tea a real macOS PTY in headless CI.
	devCmd := exec.CommandContext(ctx, "/usr/bin/script", "-q", "/dev/null", bin, "dev", "-y")
	devCmd.Dir = root
	if out, err := devCmd.CombinedOutput(); err != nil {
		t.Fatalf("lodo dev -y: %v\n%s", err, out)
	}

	rawResult, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var got struct {
		Status       int    `json:"status"`
		Via          string `json:"via"`
		Host         string `json:"host"`
		LocalAddress string `json:"localAddress"`
		DockerHostIP string `json:"dockerHostIP"`
		APIURL       string `json:"apiURL"`
		DatabaseURL  string `json:"databaseURL"`
		ViteHosts    string `json:"viteHosts"`
		DoctorOut    string `json:"doctorOut"`
		ApplyOut     string `json:"applyOut"`
		MenubarDump  string `json:"menubarDump"`
	}
	if err := json.Unmarshal(rawResult, &got); err != nil {
		t.Fatalf("unmarshal result: %v\n%s", err, rawResult)
	}
	if got.Status != 200 || !strings.Contains(got.Via, "Caddy") {
		t.Errorf("status = %d, via = %q; want 200 through Caddy", got.Status, got.Via)
	}
	if got.Host != "web.shop.lodo" {
		t.Errorf("Host = %q, want web.shop.lodo", got.Host)
	}
	if got.LocalAddress != "127.0.1.1" {
		t.Errorf("localAddress = %q, want 127.0.1.1", got.LocalAddress)
	}
	if got.DockerHostIP != "127.0.1.1" {
		t.Errorf("DOCKER_HOST_IP = %q, want 127.0.1.1", got.DockerHostIP)
	}
	if got.APIURL != "http://web.shop.lodo" {
		t.Errorf("API_URL = %q, want http://web.shop.lodo", got.APIURL)
	}
	if got.DatabaseURL != "postgres://u:hunter2@shop.lodo:5432/shop" {
		t.Errorf("DATABASE_URL = %q, want postgres://u:hunter2@shop.lodo:5432/shop", got.DatabaseURL)
	}
	if got.ViteHosts != ".shop.lodo" {
		t.Errorf("viteHosts = %q, want .shop.lodo", got.ViteHosts)
	}
	for _, want := range []string{"✓ 7 names resolve", "✓ 8 caddy"} {
		if !strings.Contains(got.DoctorOut, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, got.DoctorOut)
		}
	}
	for _, want := range []string{"✓ shop.lodo", "✓ web.shop.lodo"} {
		if !strings.Contains(got.ApplyOut, want) {
			t.Errorf("apply output lacks %q:\n%s", want, got.ApplyOut)
		}
	}
	wantMenubarRow := "● web.shop.lodo  (http :" + strconv.Itoa(appPort) + ")"
	if !strings.Contains(got.MenubarDump, "shop.lodo · 127.0.1.1") || !strings.Contains(got.MenubarDump, wantMenubarRow) {
		t.Errorf("menubar --dump while running =\n%s\nwant shop.lodo header and %q", got.MenubarDump, wantMenubarRow)
	}
	if out, err := exec.CommandContext(ctx, p.MenubarBin, "--dump").CombinedOutput(); err != nil || string(out) != "No running services\n" {
		t.Errorf("menubar --dump after stop = %q, %v; want No running services", out, err)
	}

	// 2b. lodo menubar --stop unloads the agent and removes its plist; start it
	// again so step 5 verifies lodo uninstall stops it too.
	if out := runLodo(t, ctx, "", bin, "menubar", "--stop"); strings.TrimSpace(out) != "menubar stopped" {
		t.Errorf("lodo menubar --stop output = %q", out)
	}
	if _, err := os.Stat(p.MenubarPlist); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("MenubarPlist still exists after --stop: %v", err)
	}
	if out, err := exec.CommandContext(ctx, p.Launchctl, "print", menubarTarget).CombinedOutput(); err == nil {
		t.Errorf("launchctl print %s still loaded after --stop:\n%s", menubarTarget, out)
	}
	runLodo(t, ctx, "", bin, "menubar")

	// 3. lodo exec exports the project's variables without the listen hook.
	execOut := runLodo(t, ctx, root, bin, "exec", "node", "-e",
		`console.log(JSON.stringify({ip: process.env.DOCKER_HOST_IP, api: process.env.API_URL, opts: process.env.NODE_OPTIONS || ""}))`)
	var execGot struct {
		IP   string `json:"ip"`
		API  string `json:"api"`
		Opts string `json:"opts"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(execOut)), &execGot); err != nil {
		t.Fatalf("unmarshal lodo exec stdout %q: %v", execOut, err)
	}
	if execGot.IP != "127.0.1.1" || execGot.API != "http://web.shop.lodo" {
		t.Errorf("lodo exec env = %+v, want 127.0.1.1 and http://web.shop.lodo", execGot)
	}
	if strings.Contains(execGot.Opts, "bind.cjs") || strings.Contains(execGot.Opts, "shop.lodo.cjs") {
		t.Errorf("lodo exec set NODE_OPTIONS hook: %q", execGot.Opts)
	}

	// 4. lodo prune removes names when the project folder is gone.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	pruneOut := runLodo(t, ctx, "", bin, "prune")
	if !strings.Contains(pruneOut, "shop.lodo") || !strings.Contains(pruneOut, "web.shop.lodo") {
		t.Errorf("lodo prune output = %q, want shop.lodo and web.shop.lodo removed", pruneOut)
	}
	if _, err := os.Stat(filepath.Join(p.ResolverDir, "shop.lodo")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("/etc/resolver/shop.lodo still exists after prune: %v", err)
	}

	// 5. lodo uninstall removes all system parts, and doctor reports the Mac uninstalled.
	uninstallOut := runLodo(t, ctx, "", bin, "uninstall")
	for _, want := range []string{
		"✓ menu bar icon stopped",
		"✓ sudoers rule and resolver script removed",
	} {
		if !strings.Contains(uninstallOut, want) {
			t.Errorf("lodo uninstall output lacks %q:\n%s", want, uninstallOut)
		}
	}
	for _, gone := range []string{p.Script, p.Sudoers, p.LoopbackPlist, p.MenubarPlist} {
		if _, err := os.Stat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still exists after uninstall: %v", gone, err)
		}
	}
	if out, err := exec.CommandContext(ctx, p.Launchctl, "print", menubarTarget).CombinedOutput(); err == nil {
		t.Errorf("launchctl print %s still loaded after uninstall:\n%s", menubarTarget, out)
	}
	doctorAfter := exec.CommandContext(ctx, bin, "doctor")
	if out, err := doctorAfter.CombinedOutput(); err == nil {
		t.Errorf("lodo doctor succeeded after uninstall:\n%s", out)
	}
}

func runLodo(t *testing.T, ctx context.Context, dir, bin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("lodo %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}
