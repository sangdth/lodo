package check_test

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sangdth/lcd/internal/check"
	"github.com/sangdth/lcd/internal/paths"
	"github.com/sangdth/lcd/internal/run"
	"github.com/sangdth/lcd/internal/store"
)

// crm has no port; dashboard has one and the address the test's stand-in for
// Caddy listens on.
var (
	crm       = store.Domain{Name: "crm.lcd", Address: "127.0.1.1", Enabled: true}
	dashboard = store.Domain{Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Enabled: true}
)

func TestResult_OK(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result check.Result
		want   bool
	}{
		{name: "both lookups pass, no port", result: check.Result{Direct: true, System: true}, want: true},
		{name: "dnsmasq failed", result: check.Result{System: true}, want: false},
		{name: "macOS failed", result: check.Result{Direct: true}, want: false},
		{name: "port, caddy reached the app", result: check.Result{Port: 3000, Direct: true, System: true, HTTP: true}, want: true},
		{name: "port, caddy did not reach the app", result: check.Result{Port: 3000, Direct: true, System: true}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.result.OK(); got != tt.want {
				t.Errorf("OK() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnv_Probe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		domains   []store.Domain    // nil means crm and dashboard
		dns       map[string]string // dnsmasq's answers that differ from the domains; "" means it does not know the name
		dnsDown   bool              // nothing listens on dnsmasq's port
		macOS     map[string]string // dscacheutil output that differs from the domains
		caddy     http.HandlerFunc  // the stand-in for Caddy; nil means it reached the app
		caddyDown bool              // nothing listens on the HTTP port
		hosts     string            // the hosts file's content
		timeout   time.Duration     // ends Probe early, so a failing direct probe stops retrying
		want      []check.Result    // {port} in a Detail stands for the HTTP port
	}{
		{
			name: "every probe passes, in store order, without disabled domains",
			domains: []store.Domain{
				{Name: "flowy.lcd", Address: "127.0.1.3", Enabled: true},
				dashboard,
				{Name: "old.lcd", Address: "127.0.1.2"},
				crm,
			},
			want: []check.Result{
				{Name: "crm.lcd", Address: "127.0.1.1", Direct: true, System: true},
				{Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Direct: true, System: true, HTTP: true},
				{Name: "flowy.lcd", Address: "127.0.1.3", Direct: true, System: true},
			},
		},
		{
			name:    "dnsmasq answers another address",
			domains: []store.Domain{crm},
			dns:     map[string]string{"crm.lcd": "127.0.1.9"},
			timeout: 300 * time.Millisecond,
			want: []check.Result{
				{Name: "crm.lcd", Address: "127.0.1.1", System: true, Detail: "dnsmasq: answered 127.0.1.9, want 127.0.1.1"},
			},
		},
		{
			name:    "dnsmasq does not know the name",
			domains: []store.Domain{crm},
			dns:     map[string]string{"crm.lcd": ""},
			timeout: 300 * time.Millisecond,
			want:    []check.Result{{Name: "crm.lcd", Address: "127.0.1.1", System: true, Detail: "dnsmasq: no answer"}},
		},
		{
			name:    "dnsmasq is down",
			domains: []store.Domain{crm},
			dnsDown: true,
			timeout: 300 * time.Millisecond,
			want:    []check.Result{{Name: "crm.lcd", Address: "127.0.1.1", System: true, Detail: "dnsmasq: no answer"}},
		},
		{
			name:    "macOS has no address",
			domains: []store.Domain{crm},
			macOS:   map[string]string{"crm.lcd": ""},
			want:    []check.Result{{Name: "crm.lcd", Address: "127.0.1.1", Direct: true, Detail: "macOS: no address"}},
		},
		{
			name:    "macOS resolves to other addresses",
			domains: []store.Domain{crm},
			macOS:   map[string]string{"crm.lcd": macOSOutput("crm.lcd", "127.0.1.9", "127.0.1.8")},
			want: []check.Result{
				{Name: "crm.lcd", Address: "127.0.1.1", Direct: true, Detail: "macOS: 127.0.1.9, 127.0.1.8, want 127.0.1.1"},
			},
		},
		{
			name:    "the hosts file maps the name elsewhere",
			domains: []store.Domain{crm},
			hosts:   "127.0.0.1\tlocalhost\n# old setup\n127.0.0.1 crm.lcd api.crm.lcd\n",
			dns:     map[string]string{"crm.lcd": "127.0.0.1"},
			macOS:   map[string]string{"crm.lcd": macOSOutput("crm.lcd", "127.0.0.1")},
			timeout: 300 * time.Millisecond,
			want: []check.Result{
				{Name: "crm.lcd", Address: "127.0.1.1", Detail: "{hosts} maps crm.lcd to 127.0.0.1: remove that line"},
			},
		},
		{
			name:    "a hosts entry that agrees changes nothing",
			domains: []store.Domain{crm},
			hosts:   "127.0.1.1 crm.lcd\n",
			macOS:   map[string]string{"crm.lcd": ""},
			want:    []check.Result{{Name: "crm.lcd", Address: "127.0.1.1", Direct: true, Detail: "macOS: no address"}},
		},
		{
			name:    "a commented-out hosts entry is ignored",
			domains: []store.Domain{crm},
			hosts:   "# 127.0.0.1 crm.lcd\n",
			macOS:   map[string]string{"crm.lcd": ""},
			want:    []check.Result{{Name: "crm.lcd", Address: "127.0.1.1", Direct: true, Detail: "macOS: no address"}},
		},
		{
			name:    "macOS lists the address among others",
			domains: []store.Domain{crm},
			macOS:   map[string]string{"crm.lcd": "name: crm.lcd\nipv6_address: ::1\nip_address: 127.0.1.9\nip_address: 127.0.1.1\n\n"},
			want:    []check.Result{{Name: "crm.lcd", Address: "127.0.1.1", Direct: true, System: true}},
		},
		{
			name:    "caddy reached an app that answered 500",
			domains: []store.Domain{dashboard},
			caddy:   respond(http.StatusInternalServerError, "Server", "Caddy", "Via", "1.1 Caddy"),
			want:    []check.Result{{Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Direct: true, System: true, HTTP: true}},
		},
		{
			name:    "app down",
			domains: []store.Domain{dashboard},
			caddy:   respond(http.StatusBadGateway, "Server", "Caddy"),
			want: []check.Result{{
				Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Direct: true, System: true,
				Detail: "app down: nothing answers on 127.0.0.1:3000",
			}},
		},
		{
			name:    "caddy has no site for the name",
			domains: []store.Domain{dashboard},
			caddy:   respond(http.StatusOK, "Server", "Caddy"),
			want: []check.Result{{
				Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Direct: true, System: true,
				Detail: "caddy has no site for dashboard.crm.lcd: run lcd apply",
			}},
		},
		{
			name:    "a redirect to https is not followed",
			domains: []store.Domain{dashboard},
			caddy:   respond(http.StatusPermanentRedirect, "Server", "Caddy", "Location", "https://dashboard.crm.lcd/"),
			want: []check.Result{{
				Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Direct: true, System: true,
				Detail: "caddy has no site for dashboard.crm.lcd: run lcd apply",
			}},
		},
		{
			name:    "another server has the port",
			domains: []store.Domain{dashboard},
			caddy:   respond(http.StatusOK, "Server", "nginx/1.27.0"),
			want: []check.Result{{
				Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Direct: true, System: true,
				Detail: "port {port} on 127.0.0.1 answered by nginx/1.27.0, not Caddy",
			}},
		},
		{
			name:    "a server that does not name itself",
			domains: []store.Domain{dashboard},
			caddy:   respond(http.StatusNotFound),
			want: []check.Result{{
				Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Direct: true, System: true,
				Detail: "port {port} on 127.0.0.1 answered by something, not Caddy",
			}},
		},
		{
			name:      "nothing listens on the HTTP port",
			domains:   []store.Domain{dashboard},
			caddyDown: true,
			timeout:   300 * time.Millisecond,
			want: []check.Result{{
				Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000, Direct: true, System: true,
				Detail: "nothing answers on 127.0.0.1:{port}: is Caddy running?",
			}},
		},
		{
			name:      "every probe fails",
			domains:   []store.Domain{dashboard},
			dns:       map[string]string{"dashboard.crm.lcd": "127.0.1.9"},
			macOS:     map[string]string{"dashboard.crm.lcd": ""},
			caddyDown: true,
			timeout:   300 * time.Millisecond,
			want: []check.Result{{
				Name: "dashboard.crm.lcd", Address: "127.0.0.1", Port: 3000,
				Detail: "dnsmasq: answered 127.0.1.9, want 127.0.0.1; macOS: no address; nothing answers on 127.0.0.1:{port}: is Caddy running?",
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			domains := tt.domains
			if domains == nil {
				domains = []store.Domain{crm, dashboard}
			}
			answers := map[string]string{}
			e, fake := newProbeEnv(t)
			if tt.hosts != "" {
				writeFile(t, e.Paths.Hosts, tt.hosts)
			}
			for _, d := range domains {
				answers[d.Name] = d.Address
				fake.Set(dscacheutil(e.Paths, d.Name), macOSOutput(d.Name, d.Address))
			}
			for name, addr := range tt.dns {
				answers[name] = addr
			}
			for name, out := range tt.macOS {
				fake.Set(dscacheutil(e.Paths, name), out)
			}
			if !tt.dnsDown {
				e.DNS = startDNS(t, fromMap(answers))
			}
			if !tt.caddyDown {
				h := tt.caddy
				if h == nil {
					h = respond(http.StatusOK, "Server", "Caddy", "Via", "1.1 Caddy")
				}
				e.HTTPPort = startHTTP(t, h)
			}

			ctx := t.Context()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}
			got := e.Probe(ctx, domains)
			want := slices.Clone(tt.want)
			for i := range want {
				want[i].Detail = strings.NewReplacer("{port}", strconv.Itoa(e.HTTPPort), "{hosts}", e.Paths.Hosts).Replace(want[i].Detail)
			}
			if !slices.Equal(got, want) {
				t.Errorf("Probe =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// TestEnv_Probe_HTTPRequest checks the request a browser would send: GET / with
// the name as Host, to the HTTP port on the domain's address, whatever port
// the URL implies, and no second request for a redirect.
func TestEnv_Probe_HTTPRequest(t *testing.T) {
	t.Parallel()

	e, fake := newProbeEnv(t)
	e.DNS = startDNS(t, fromMap(map[string]string{dashboard.Name: dashboard.Address}))
	fake.Set(dscacheutil(e.Paths, dashboard.Name), macOSOutput(dashboard.Name, dashboard.Address))
	var mu sync.Mutex
	var requests []string
	e.HTTPPort = startHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.Host+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Server", "Caddy")
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	})

	got := e.Probe(t.Context(), []store.Domain{dashboard})
	if len(got) != 1 || got[0].Detail != "caddy has no site for dashboard.crm.lcd: run lcd apply" {
		t.Errorf("Probe = %+v, want caddy's own redirect read as no site", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"GET dashboard.crm.lcd /"}; !slices.Equal(requests, want) {
		t.Errorf("requests = %q, want %q", requests, want)
	}
}

func TestEnv_Probe_RetriesWhileDnsmasqStarts(t *testing.T) {
	t.Parallel()

	e, fake := newProbeEnv(t)
	ready := time.Now().Add(300 * time.Millisecond)
	e.DNS = startDNS(t, func(name string) (string, bool) {
		if time.Now().Before(ready) {
			return "", false
		}
		return crm.Address, name == crm.Name
	})
	fake.Set(dscacheutil(e.Paths, crm.Name), macOSOutput(crm.Name, crm.Address))

	got := e.Probe(t.Context(), []store.Domain{crm})
	if len(got) != 1 || !got[0].Direct {
		t.Errorf("Probe = %+v, want dnsmasq's late answer accepted", got)
	}
}

// TestEnv_Probe_RetriesWhileCaddyStarts starts the stand-in for Caddy only
// after the probe's first connection was refused, as after a Caddy restart.
func TestEnv_Probe_RetriesWhileCaddyStarts(t *testing.T) {
	t.Parallel()

	e, fake := newProbeEnv(t)
	e.DNS = startDNS(t, fromMap(map[string]string{dashboard.Name: dashboard.Address}))
	fake.Set(dscacheutil(e.Paths, dashboard.Name), macOSOutput(dashboard.Name, dashboard.Address))
	port := closedTCP(t)
	e.HTTPPort = port
	started := make(chan struct{})
	go func() {
		defer close(started)
		time.Sleep(300 * time.Millisecond)
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Errorf("listen on the reserved port: %v", err)
			return
		}
		srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: respond(http.StatusOK, "Via", "1.1 Caddy")}}
		srv.Start()
		t.Cleanup(srv.Close)
	}()

	got := e.Probe(t.Context(), []store.Domain{dashboard})
	<-started
	if len(got) != 1 || !got[0].HTTP {
		t.Errorf("Probe = %+v, want Caddy's late answer accepted", got)
	}
}

// TestEnv_Probe_DomainsAtOnce holds every HTTP request until all of them
// arrived, which only happens when the domains are probed concurrently.
func TestEnv_Probe_DomainsAtOnce(t *testing.T) {
	t.Parallel()

	domains := []store.Domain{
		{Name: "a.lcd", Address: "127.0.0.1", Port: 3000, Enabled: true},
		{Name: "b.lcd", Address: "127.0.0.1", Port: 3001, Enabled: true},
		{Name: "c.lcd", Address: "127.0.0.1", Port: 3002, Enabled: true},
	}
	const n = 3
	e, fake := newProbeEnv(t)
	answers := map[string]string{}
	for _, d := range domains {
		answers[d.Name] = d.Address
		fake.Set(dscacheutil(e.Paths, d.Name), macOSOutput(d.Name, d.Address))
	}
	e.DNS = startDNS(t, fromMap(answers))
	var arrived atomic.Int32
	all := make(chan struct{})
	e.HTTPPort = startHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == n {
			close(all)
		}
		select {
		case <-all:
			w.Header().Set("Via", "1.1 Caddy")
		case <-time.After(time.Second): // the others never came
		case <-r.Context().Done():
		}
	})

	for _, r := range e.Probe(t.Context(), domains) {
		if !r.HTTP {
			t.Errorf("%s: %s; want all %d requests in flight at once", r.Name, r.Detail, n)
		}
	}
}

func TestEnv_Probe_StopsWhenContextEnds(t *testing.T) {
	t.Parallel()

	e, fake := newProbeEnv(t)
	e.DNS = startDNS(t, fromMap(map[string]string{crm.Name: crm.Address}))
	fake.Set(dscacheutil(e.Paths, crm.Name), macOSOutput(crm.Name, crm.Address))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	got := e.Probe(ctx, []store.Domain{crm})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Probe took %v with an ended context, want it to stop at once", elapsed)
	}
	if len(got) != 1 || got[0].Direct || got[0].Detail != "dnsmasq: no answer" {
		t.Errorf("Probe = %+v, want the direct probe to fail with no answer", got)
	}
}

// newProbeEnv returns an Env whose commands go to a fake, and whose dnsmasq
// and Caddy are down until the test starts stand-ins for them.
func newProbeEnv(t *testing.T) (check.Env, *run.Fake) {
	t.Helper()
	fake := run.NewFake()
	e := check.NewEnv(paths.ForTest(t.TempDir()), fake)
	e.DNS = closedUDP(t)
	e.HTTPPort = closedTCP(t)
	return e, fake
}

func dscacheutil(p paths.Paths, name string) string {
	return run.Line(p.Dscacheutil, "-q", "host", "-a", "name", name)
}

// macOSOutput is dscacheutil -q host output for name with addrs.
func macOSOutput(name string, addrs ...string) string {
	var b strings.Builder
	b.WriteString("name: " + name + "\n")
	for _, a := range addrs {
		b.WriteString("ip_address: " + a + "\n")
	}
	return b.String() + "\n"
}

// startDNS serves A records on a local UDP port, as dnsmasq does, and returns
// the server's address. answer gives a name's address, or false for NXDOMAIN.
func startDNS(t *testing.T, answer func(name string) (string, bool)) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // ends the serving goroutine
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := dnsReply(buf[:n], answer); reply != nil {
				_, _ = conn.WriteTo(reply, from) // a lost reply is a lost UDP packet
			}
		}
	}()
	return conn.LocalAddr().String()
}

func fromMap(answers map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		addr, ok := answers[name]
		return addr, ok && addr != ""
	}
}

// dnsReply builds the answer to a DNS query by hand. It echoes the ID and the
// question, sets QR and RA, and adds one A record (name pointer 0xC00C, TTL 0)
// for a known name, or sets NXDOMAIN. It returns nil for a packet it cannot
// read.
func dnsReply(query []byte, answer func(string) (string, bool)) []byte {
	const headerLen = 12
	if len(query) < headerLen {
		return nil
	}
	var labels []string
	i := headerLen
	for i < len(query) && query[i] != 0 {
		n := int(query[i])
		if i+1+n > len(query) {
			return nil
		}
		labels = append(labels, string(query[i+1:i+1+n]))
		i += 1 + n
	}
	end := i + 5 // the root label, QTYPE and QCLASS
	if end > len(query) {
		return nil
	}
	isA := binary.BigEndian.Uint16(query[i+1:]) == 1
	addr, known := answer(strings.ToLower(strings.Join(labels, ".")))

	reply := make([]byte, headerLen, end+16)
	copy(reply, query[:2])
	flags, answers := uint16(0x8080), uint16(0) // QR, RA
	switch {
	case !known:
		flags |= 3 // NXDOMAIN
	case isA:
		answers = 1
	}
	binary.BigEndian.PutUint16(reply[2:], flags)
	binary.BigEndian.PutUint16(reply[4:], 1) // QDCOUNT
	binary.BigEndian.PutUint16(reply[6:], answers)
	reply = append(reply, query[headerLen:end]...)
	if answers == 1 {
		ip := netip.MustParseAddr(addr).As4()
		reply = append(reply, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4)
		reply = append(reply, ip[:]...)
	}
	return reply
}

// startHTTP serves h on a local port, as a stand-in for Caddy, and returns
// the port.
func startHTTP(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

// respond returns a handler that answers with status and the header pairs.
func respond(status int, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
	}
}

// closedUDP returns a local UDP address where nothing listens.
func closedUDP(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := conn.LocalAddr().String()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// closedTCP returns a local TCP port where nothing listens.
func closedTCP(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}
