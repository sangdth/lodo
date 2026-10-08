package check

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sangdth/lcd/internal/store"
)

// Probe time limits. Probe often runs right after apply restarted dnsmasq, so
// the direct probe retries for a while before it gives up.
const (
	directWindow  = 2 * time.Second
	directRetry   = 100 * time.Millisecond
	systemTimeout = 6 * time.Second
	httpTimeout   = 3 * time.Second
)

// Result is what the probes found for one enabled domain.
type Result struct {
	Name    string
	Address string
	Port    int    // 0 means no HTTP probe
	Direct  bool   // dnsmasq answered with Address
	System  bool   // macOS resolved Name to Address
	HTTP    bool   // Caddy reached the app; only when Port > 0
	Detail  string // why probes failed, joined with "; "; empty when all passed
}

// OK reports whether every probe that applies to the domain passed.
func (r Result) OK() bool {
	return r.Direct && r.System && (r.Port == 0 || r.HTTP)
}

// Probe checks every enabled domain, all domains at once: dnsmasq answers
// the name with its address, macOS resolves it to that address, and, for a
// domain with a port, Caddy reaches the app. Results follow store.Sort order.
// ctx bounds every probe.
func (e Env) Probe(ctx context.Context, domains []store.Domain) []Result {
	on := enabled(domains)
	hosts, _ := readFile(e.Paths.Hosts) // no hosts file means no entries
	entries := hostsAddresses(hosts)
	results := make([]Result, len(on))
	var wg sync.WaitGroup
	for i, d := range on {
		wg.Go(func() { results[i] = e.probe(ctx, d, entries[d.Name]) })
	}
	wg.Wait()
	return results
}

// probe runs one domain's probes, one after another. Each probe returns what
// went wrong, or "" when it passed. inHosts are the addresses the hosts file
// lists for the name: macOS and dnsmasq both answer from it first, so an entry
// with another address explains a failed lookup better than the lookup does.
func (e Env) probe(ctx context.Context, d store.Domain, inHosts []string) Result {
	direct := e.probeDirect(ctx, d.Name, d.Address)
	sys := e.probeSystem(ctx, d.Name, d.Address)
	lookups := []string{direct, sys}
	if (direct != "" || sys != "") && len(inHosts) > 0 && !slices.Contains(inHosts, d.Address) {
		lookups = []string{e.Paths.Hosts + " maps " + d.Name + " to " + strings.Join(inHosts, ", ") + ": remove that line"}
	}
	var web string
	if d.Port > 0 {
		web = e.probeHTTP(ctx, d)
	}
	problems := slices.DeleteFunc(append(lookups, web), func(s string) bool { return s == "" })
	return Result{
		Name:    d.Name,
		Address: d.Address,
		Port:    d.Port,
		Direct:  direct == "",
		System:  sys == "",
		HTTP:    d.Port > 0 && web == "",
		Detail:  strings.Join(problems, "; "),
	}
}

// probeDirect asks dnsmasq for name's IPv4 address, skipping the system
// resolver, and retries every directRetry for up to directWindow while the
// answer is missing or wrong.
func (e Env) probeDirect(ctx context.Context, name, want string) string {
	ctx, cancel := context.WithTimeout(ctx, directWindow)
	defer cancel()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", e.DNS)
		},
	}
	retry := time.NewTicker(directRetry)
	defer retry.Stop()
	var problem string
	for {
		addrs, err := resolver.LookupNetIP(ctx, "ip4", name)
		if err == nil && slices.ContainsFunc(addrs, func(a netip.Addr) bool { return a.Unmap().String() == want }) {
			return ""
		}
		// An attempt that ctx cut short tells nothing new: keep the last answer.
		if problem == "" || ctx.Err() == nil {
			problem = directProblem(addrs, err, want)
		}
		select {
		case <-ctx.Done():
			return problem
		case <-retry.C:
		}
	}
}

// directProblem describes a failed direct lookup.
func directProblem(addrs []netip.Addr, err error, want string) string {
	if err != nil || len(addrs) == 0 {
		return "dnsmasq: no answer"
	}
	got := make([]string, len(addrs))
	for i, a := range addrs {
		got[i] = a.Unmap().String()
	}
	return "dnsmasq: answered " + strings.Join(got, ", ") + ", want " + want
}

// probeSystem resolves name the way apps do, through macOS's resolver and
// its /etc/resolver files.
func (e Env) probeSystem(ctx context.Context, name, want string) string {
	ctx, cancel := context.WithTimeout(ctx, systemTimeout)
	defer cancel()
	out, _ := e.Runner.Run(ctx, e.Paths.Dscacheutil, "-q", "host", "-a", "name", name) // a failed lookup lists no address
	addrs := ipAddresses(out)
	switch {
	case slices.Contains(addrs, want):
		return ""
	case len(addrs) == 0:
		return "macOS: no address"
	}
	return "macOS: " + strings.Join(addrs, ", ") + ", want " + want
}

// hostsAddresses maps every name in a hosts file to the addresses listed for
// it, in file order. Comments and malformed lines are skipped.
func hostsAddresses(content string) map[string][]string {
	entries := map[string][]string{}
	for line := range strings.Lines(content) {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		for _, name := range f[1:] {
			name = strings.ToLower(name)
			entries[name] = append(entries[name], f[0])
		}
	}
	return entries
}

// ipAddresses returns the values of the ip_address lines in dscacheutil
// output.
func ipAddresses(out string) []string {
	var addrs []string
	for line := range strings.Lines(out) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ip_address:"); ok {
			addrs = append(addrs, strings.TrimSpace(v))
		}
	}
	return addrs
}

// probeHTTP asks for http://<name>/ on the domain's address and HTTP port,
// as a browser would after resolving the name, and reads from the answer
// whether Caddy reached the app.
func (e Env) probeHTTP(ctx context.Context, d store.Domain) string {
	caddyAddr := net.JoinHostPort(d.Address, strconv.Itoa(e.HTTPPort))
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", caddyAddr)
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       httpTimeout,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.Name+"/", nil)
	if err != nil {
		return oneLine(err.Error())
	}
	resp, err := client.Do(req)
	if err != nil {
		return "nothing answers on " + caddyAddr + ": is Caddy running?"
	}
	defer func() { _ = resp.Body.Close() }() // only the status and headers matter
	return httpProblem(resp.StatusCode, resp.Header, d, e.HTTPPort)
}

// httpProblem reads who answered on the HTTP port. Caddy 2.11 adds a Via
// header naming itself when it reached the app, and answers with only its
// Server header when it did not.
func httpProblem(status int, h http.Header, d store.Domain, httpPort int) string {
	if strings.Contains(strings.Join(h.Values("Via"), ", "), "Caddy") {
		return ""
	}
	server := h.Get("Server")
	switch {
	case server == "Caddy" && status == http.StatusBadGateway:
		return "app down: nothing answers on " + net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
	case server == "Caddy":
		return "caddy has no site for " + d.Name + ": run lcd apply"
	}
	return fmt.Sprintf("port %d on %s answered by %s, not Caddy", httpPort, d.Address, cmp.Or(server, "something"))
}
