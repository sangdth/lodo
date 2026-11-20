package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

const (
	e2eDnsmasqBin = "/opt/homebrew/opt/dnsmasq/sbin/dnsmasq"
	e2eCaddyBin   = "/opt/homebrew/bin/caddy"
	e2eSwiftcBin  = "/usr/bin/swiftc"
)

// e2eRunner wraps run.Fake so system.Apply runs the real apply-resolvers.sh
// script in the temp dir, swiftc compiles the real menu bar binary when
// swiftcBin is set, and lodo dev's Replace spawns the real Node dev server
// with the generated environment.
type e2eRunner struct {
	*run.Fake
	t         *testing.T
	paths     paths.Paths
	nodeBin   string
	swiftcBin string
	stopNode  func()
}

func (r *e2eRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if r.swiftcBin != "" && name == r.paths.Swiftc {
		cmd := exec.CommandContext(ctx, r.swiftcBin, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("swiftc: %w: %s", err, out)
		}
		return string(out), nil
	}
	return r.Fake.Run(ctx, name, args...)
}

func (r *e2eRunner) RunInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	if name == r.paths.Sudo && slices.Equal(args, []string{"-n", r.paths.Script}) {
		cmd := exec.CommandContext(ctx, "/bin/sh", r.paths.Script)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("apply-resolvers.sh: %w: %s", err, out)
		}
		return string(out), nil
	}
	return r.Fake.RunInput(ctx, stdin, name, args...)
}

func (r *e2eRunner) Replace(dir, name string, args, env []string) error {
	if err := r.Fake.Replace(dir, name, args, env); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.t.Context())
	cmd := exec.CommandContext(ctx, r.nodeBin, "server.js")
	cmd.Dir = dir
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	stop := func() {
		cancel()
		_ = cmd.Wait()
	}
	r.stopNode = stop
	r.t.Cleanup(stop)
	return nil
}

// TestE2E_DataPlane exercises the full rootless pipeline in a temp dir:
// scanning a project, saving via the setup form (lodo dev -y), generating
// dnsmasq/Caddy/hook files, running apply-resolvers.sh, starting real dnsmasq,
// Caddy, and Node processes with bind.cjs, and probing DNS + HTTP end-to-end.
func TestE2E_DataPlane(t *testing.T) {
	t.Parallel()

	for _, bin := range []string{e2eDnsmasqBin, e2eCaddyBin} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed: %v", bin, err)
		}
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is not installed: %v", err)
	}

	addr := "127.0.0.1"
	if e2eCanListen("127.0.1.1") {
		addr = "127.0.1.1"
	}
	appPort := e2eFreeTCPPort(t, addr)
	caddyPort := e2eFreeTCPPort(t, "127.0.0.1")
	dnsPort := e2eFreeUDPPort(t)

	serverJS := fmt.Sprintf(`const http = require("node:http");
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
srv.listen(%d, "localhost");
`, appPort)

	root := writeTree(t, map[string]string{
		".git/HEAD":             "ref: refs/heads/master\n",
		".env":                  "DOCKER_HOST_IP=" + addr + "\n",
		"package.json":          `{"scripts": {"dev": "node server.js"}}`,
		"pnpm-lock.yaml":        "",
		"pnpm-workspace.yaml":   "packages: [apps/*]\n",
		"apps/web/package.json": fmt.Sprintf(`{"scripts": {"dev": "next dev -p %d"}}`, appPort),
		"apps/web/.env":         fmt.Sprintf("API_URL=http://localhost:%d\nDATABASE_URL=postgres://u:hunter2@localhost:5432/shop\n", appPort),
		"compose.yaml":          `services: {db: {image: postgres, ports: ["${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"]}}`,
		"server.js":             serverJS,
	})

	p := paths.ForTest(t.TempDir())
	fake := run.NewFake()
	setUpFakeMac(t, p, fake)
	for path, content := range map[string]string{
		p.Caddy:           "",
		p.SystemCaddyfile: system.RewriteSystemCaddyfile("", p),
		filepath.Join(p.LaunchAgents, "sh.brew.caddy.plist"): "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake.Set(run.Line(p.Launchctl, "print", "gui/501/sh.brew.caddy"), "\tstate = running\n\tpid = 43\n")

	runner := &e2eRunner{Fake: fake, t: t, paths: p, nodeBin: nodeBin}
	out := &strings.Builder{}
	a := newApp(p, runner, out, out)
	a.lookPath = func(file string) (string, error) { return "/opt/bin/" + file, nil }
	a.teaOpts = []tea.ProgramOption{
		tea.WithInput(&bytes.Buffer{}),
		tea.WithOutput(&bytes.Buffer{}),
		tea.WithoutSignals(),
		tea.WithWindowSize(80, 24),
	}
	a.form = a.setupForm

	if err := a.startDev(t.Context(), root, nil, options{auto: true}); err != nil {
		t.Fatalf("startDev(-y): %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Errorf("lodo dev leaked password in stdout:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "postgres://***@shop.lodo:5432/shop") {
		t.Errorf("lodo dev output lacks masked DATABASE_URL:\n%s", out.String())
	}

	// Verify apply-resolvers.sh wrote both resolver files.
	for _, name := range []string{"shop.lodo", "web.shop.lodo"} {
		got, err := os.ReadFile(filepath.Join(p.ResolverDir, name))
		if err != nil {
			t.Fatalf("resolver file %s missing: %v", name, err)
		}
		if string(got) != system.ResolverFile() {
			t.Errorf("resolver %s = %q, want %q", name, got, system.ResolverFile())
		}
	}

	// Start real dnsmasq with the generated config.
	dnsCtx, dnsCancel := context.WithCancel(t.Context())
	dnsCmd := exec.CommandContext(dnsCtx, e2eDnsmasqBin,
		"--keep-in-foreground",
		"--no-resolv",
		"--no-hosts",
		"--listen-address=127.0.0.1",
		"--port="+strconv.Itoa(dnsPort),
		"--conf-file="+p.DnsmasqConf,
	)
	if err := dnsCmd.Start(); err != nil {
		dnsCancel()
		t.Fatalf("start dnsmasq: %v", err)
	}
	t.Cleanup(func() {
		dnsCancel()
		_ = dnsCmd.Wait()
	})

	// Start real Caddy importing lodo's generated Caddyfile.
	caddyHome := t.TempDir()
	mainCF := filepath.Join(caddyHome, "Caddyfile")
	cfContent := fmt.Sprintf("{\n\tadmin off\n\thttp_port %d\n}\nimport %q\n", caddyPort, p.Caddyfile)
	if err := os.WriteFile(mainCF, []byte(cfContent), 0o600); err != nil {
		t.Fatal(err)
	}
	caddyCtx, caddyCancel := context.WithCancel(t.Context())
	caddyCmd := exec.CommandContext(caddyCtx, e2eCaddyBin, "run", "--config", mainCF, "--adapter", "caddyfile")
	caddyCmd.Env = append(os.Environ(), "HOME="+caddyHome, "XDG_DATA_HOME="+caddyHome, "XDG_CONFIG_HOME="+caddyHome)
	if err := caddyCmd.Start(); err != nil {
		caddyCancel()
		t.Fatalf("start caddy: %v", err)
	}
	t.Cleanup(func() {
		caddyCancel()
		_ = caddyCmd.Wait()
	})

	// Run check.Env.Probe against the live dnsmasq, Caddy, and Node server.
	saved, err := store.Load(p.DomainsJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range saved {
		fake.Set(run.Line(p.Dscacheutil, "-q", "host", "-a", "name", d.Name), "name: "+d.Name+"\nip_address: "+d.Address+"\n\n")
	}
	a.env.DNS = net.JoinHostPort("127.0.0.1", strconv.Itoa(dnsPort))
	a.env.HTTPPort = caddyPort

	// Wait for the Node server (started by Replace) and Caddy to accept requests.
	client := &http.Client{Timeout: 2 * time.Second}
	var body struct {
		Host         string `json:"host"`
		LocalAddress string `json:"localAddress"`
		DockerHostIP string `json:"dockerHostIP"`
		APIURL       string `json:"apiURL"`
		DatabaseURL  string `json:"databaseURL"`
		ViteHosts    string `json:"viteHosts"`
	}
	var viaHeader string
	for range 40 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://%s:%d/", addr, caddyPort), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "web.shop.lodo"
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			viaHeader = resp.Header.Get("Via")
			decErr := json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if decErr == nil {
				break
			}
		} else if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if body.Host == "" {
		t.Fatal("caddy -> node request never succeeded")
	}
	if !strings.Contains(viaHeader, "Caddy") {
		t.Errorf("Via header = %q, want Caddy", viaHeader)
	}
	if body.Host != "web.shop.lodo" {
		t.Errorf("Host = %q, want web.shop.lodo", body.Host)
	}
	if body.LocalAddress != addr {
		t.Errorf("Node localAddress = %q, want %q (bound by bind.cjs)", body.LocalAddress, addr)
	}
	if body.DockerHostIP != addr {
		t.Errorf("DOCKER_HOST_IP = %q, want %q", body.DockerHostIP, addr)
	}
	if body.APIURL != "http://web.shop.lodo" {
		t.Errorf("API_URL = %q, want http://web.shop.lodo", body.APIURL)
	}
	if body.DatabaseURL != "postgres://u:hunter2@shop.lodo:5432/shop" {
		t.Errorf("DATABASE_URL = %q, want postgres://u:hunter2@shop.lodo:5432/shop", body.DatabaseURL)
	}
	if body.ViteHosts != ".shop.lodo" {
		t.Errorf("__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS = %q, want .shop.lodo", body.ViteHosts)
	}

	results := a.env.Probe(t.Context(), saved)
	if len(results) != 2 {
		t.Fatalf("Probe returned %d results, want 2: %+v", len(results), results)
	}
	for _, res := range results {
		if !res.OK() {
			t.Errorf("probe %s failed: %+v", res.Name, res)
		}
	}
}

// TestE2E_Menubar exercises the menu bar end-to-end in a temp dir: setting up
// a project with lodo dev -y, compiling the real Swift helper via lodo menubar,
// validating its LaunchAgent plist, verifying --dump reports only the live
// bind.cjs-bound Node service (and hides it once stopped), and stopping via
// lodo menubar --stop.
func TestE2E_Menubar(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath(e2eSwiftcBin); err != nil {
		t.Skipf("%s is not installed: %v", e2eSwiftcBin, err)
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is not installed: %v", err)
	}

	addr := "127.0.0.1"
	if e2eCanListen("127.0.1.1") {
		addr = "127.0.1.1"
	}
	webPort := e2eFreeTCPPort(t, addr)
	apiPort := e2eFreeTCPPort(t, addr) // listed in domains.json, never started

	serverJS := fmt.Sprintf(`const http = require("node:http");
http.createServer((_, res) => res.end("ok")).listen(%d, "localhost");
`, webPort)

	root := writeTree(t, map[string]string{
		".git/HEAD":             "ref: refs/heads/master\n",
		".env":                  "DOCKER_HOST_IP=" + addr + "\n",
		"package.json":          `{"scripts": {"dev": "node server.js"}}`,
		"pnpm-lock.yaml":        "",
		"pnpm-workspace.yaml":   "packages: [apps/*]\n",
		"apps/web/package.json": fmt.Sprintf(`{"scripts": {"dev": "next dev -p %d"}}`, webPort),
		"apps/api/package.json": fmt.Sprintf(`{"scripts": {"dev": "vite --port %d"}}`, apiPort),
		"server.js":             serverJS,
	})

	p := paths.ForTest(t.TempDir())
	fake := run.NewFake()
	setUpFakeMac(t, p, fake)
	for path, content := range map[string]string{
		p.Caddy:           "",
		p.Swiftc:          "",
		p.SystemCaddyfile: system.RewriteSystemCaddyfile("", p),
		filepath.Join(p.LaunchAgents, "sh.brew.caddy.plist"): "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake.Set(run.Line(p.Launchctl, "print", "gui/501/sh.brew.caddy"), "\tstate = running\n\tpid = 43\n")
	menubarPrint := run.Line(p.Launchctl, "print", "gui/501/io.lodo.menubar")
	fake.Fail(menubarPrint, "Could not find service")

	runner := &e2eRunner{Fake: fake, t: t, paths: p, nodeBin: nodeBin, swiftcBin: e2eSwiftcBin}
	var stdout, stderr strings.Builder
	a := newApp(p, runner, &stdout, &stderr)
	a.lookPath = func(file string) (string, error) { return "/opt/bin/" + file, nil }
	a.teaOpts = []tea.ProgramOption{
		tea.WithInput(&bytes.Buffer{}),
		tea.WithOutput(&bytes.Buffer{}),
		tea.WithoutSignals(),
		tea.WithWindowSize(80, 24),
	}
	a.form = a.setupForm

	if err := a.startDev(t.Context(), root, nil, options{auto: true}); err != nil {
		t.Fatalf("startDev(-y): %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	// Start menubar: compiles real Swift helper, writes plist, bootstraps job.
	stdout.Reset()
	stderr.Reset()
	if code := a.command(t.Context(), "menubar", options{}); code != 0 || stdout.String() != "menubar running\n" {
		t.Fatalf("lodo menubar: code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(p.MenubarBin); err != nil {
		t.Fatalf("compiled menubar binary missing: %v", err)
	}
	if _, err := os.Stat("/usr/bin/plutil"); err == nil {
		if out, err := exec.CommandContext(t.Context(), "/usr/bin/plutil", "-lint", p.MenubarPlist).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint %s: %v\n%s", p.MenubarPlist, err, out)
		}
	}

	dump := func() string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), p.MenubarBin, "--dump")
		cmd.Env = append(os.Environ(), "LODO_DOMAINS_JSON="+p.DomainsJSON)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("lodo-menubar --dump: %v\n%s", err, out)
		}
		return string(out)
	}

	wantHeader := fmt.Sprintf("shop.lodo · %s", addr)
	wantRow := fmt.Sprintf("● web.shop.lodo  (http :%d)", webPort)
	var got string
	for range 40 {
		got = dump()
		if strings.Contains(got, wantRow) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(got, wantHeader) || !strings.Contains(got, wantRow) {
		t.Fatalf("menubar --dump while running =\n%s\nwant header %q and row %q", got, wantHeader, wantRow)
	}
	if strings.Contains(got, "api.shop.lodo") {
		t.Errorf("menubar --dump listed stopped service api.shop.lodo:\n%s", got)
	}

	// Stop the Node server and confirm the menu updates to empty.
	runner.stopNode()
	if got := dump(); got != "No running services\n" {
		t.Errorf("menubar --dump after stop = %q, want No running services", got)
	}

	// Stop menubar: boots out job and removes plist.
	fake.Set(menubarPrint, "\tstate = running\n\tpid = 777\n")
	stdout.Reset()
	stderr.Reset()
	if code := a.command(t.Context(), "menubar", options{stop: true}); code != 0 || stdout.String() != "menubar stopped\n" {
		t.Fatalf("lodo menubar --stop: code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(p.MenubarPlist); !os.IsNotExist(err) {
		t.Errorf("MenubarPlist still exists after --stop: %v", err)
	}
	if !slices.Contains(fake.Calls(), run.Line(p.Launchctl, "bootout", "gui/501/io.lodo.menubar")) {
		t.Errorf("lodo menubar --stop did not bootout LaunchAgent: %q", fake.Calls())
	}
}

func e2eCanListen(addr string) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

func e2eFreeTCPPort(t *testing.T, addr string) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func e2eFreeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).Port
}
