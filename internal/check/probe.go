package check

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sangdth/lodo/internal/store"
)

// Probe time limits. Probe often runs right after apply restarted dnsmasq or
// Caddy, so the direct and HTTP probes retry for a while before they give up.
const (
	directWindow  = 2 * time.Second
	retryEvery    = 100 * time.Millisecond
	systemTimeout = 6 * time.Second
	httpWindow    = 3 * time.Second
	localTimeout  = 500 * time.Millisecond // each dial to an app's port
)

// The ends of the problems lodo apply can't fix, each naming what does.
const (
	removeLine = ": remove that line"
	runSetup   = ": run lodo setup"
	runDev     = ": start it with lodo dev in its project"
)

// Result is what the probes found for one enabled domain.
type Result struct {
	Name    string
	Address string
	Port    int    // 0 means no HTTP probe
	Direct  bool   // dnsmasq answered with Address
	System  bool   // macOS resolved Name to Address
	HTTP    bool   // Caddy reached the app; only when Port > 0
	Secure  bool   // the domain has HTTPS on, so the HTTPS probe applies
	HTTPS   bool   // Caddy reached the app over HTTPS with a certificate macOS trusts; only when Secure
	Detail  string // why probes failed, joined with "; "; empty when all passed

	// Down means the app isn't running and nothing else failed: the lookups
	// passed, Caddy answered that nothing listens on Address and Port, nothing
	// accepts a connection there, and no app was found on Localhost instead.
	// lodo can't start an app, so doctor and apply don't count it as a
	// failure; OK is still false.
	Down bool
}

// OK reports whether every probe that applies to the domain passed.
func (r Result) OK() bool {
	return r.Direct && r.System && (r.Port == 0 || r.HTTP) && (!r.Secure || r.HTTPS)
}

// Probe checks every enabled domain, all domains at once: dnsmasq answers
// the name with its address, macOS resolves it to that address, and, for a
// domain with a port, Caddy reaches the app. Results follow store.Sort order.
// ctx bounds every probe. A name whose app Caddy found stopped is then looked
// for on Localhost, once per port: lodo can't tell whose app a listener there
// is, so every stopped name on that port fails with the same detail, which
// checkNames lists once.
func (e Env) Probe(ctx context.Context, domains []store.Domain) []Result {
	on := enabled(domains)
	hosts, _ := readFile(e.Paths.Hosts) // no hosts file means no entries
	entries := hostsAddresses(hosts)
	results := make([]Result, len(on))
	stopped := make([]bool, len(on))
	var wg sync.WaitGroup
	for i, d := range on {
		wg.Go(func() { results[i], stopped[i] = e.probe(ctx, d, entries[d.Name]) })
	}
	wg.Wait()
	elsewhere := map[int]string{} // per port, probeLocalhost's answer
	for i, d := range on {
		if !stopped[i] {
			continue
		}
		wrong, ok := elsewhere[d.Port]
		if !ok {
			wrong = e.probeLocalhost(ctx, d)
			elsewhere[d.Port] = wrong
		}
		if wrong != "" {
			results[i].Detail = strings.Replace(results[i].Detail, appDown(d), wrong, 1)
			results[i].Down = false
		}
	}
	return results
}

// probe runs one domain's probes: the lookups one after the other, then HTTP
// and HTTPS together. Each probe returns what went wrong, or "" when it
// passed. inHosts are the addresses the hosts file lists for the name: macOS
// and dnsmasq both answer from it first, so an entry with another address
// explains a failed lookup better than the lookup does. When Caddy finds the
// app down, an HTTPS probe that found the same is said once, and one dial to
// the domain's address and port tells a stopped app from one that answers
// Caddy badly. It also reports whether the app is stopped.
func (e Env) probe(ctx context.Context, d store.Domain, inHosts []string) (Result, bool) {
	direct := e.probeDirect(ctx, d.Name, d.Address)
	sys := e.probeSystem(ctx, d.Name, d.Address)
	lookups := []string{direct, sys}
	if (direct != "" || sys != "") && len(inHosts) > 0 && !slices.Contains(inHosts, d.Address) {
		lookups = []string{e.Paths.Hosts + " maps " + d.Name + " to " + strings.Join(inHosts, ", ") + removeLine}
	}
	var web, secure string
	var wg sync.WaitGroup
	if d.Port > 0 {
		wg.Go(func() { web = e.probeHTTP(ctx, d) })
	}
	if d.HTTPS {
		wg.Go(func() { secure = e.probeHTTPS(ctx, d) })
	}
	wg.Wait()
	r := Result{
		Name:    d.Name,
		Address: d.Address,
		Port:    d.Port,
		Direct:  direct == "",
		System:  sys == "",
		HTTP:    d.Port > 0 && web == "",
		Secure:  d.HTTPS,
		HTTPS:   d.HTTPS && secure == "",
	}
	stopped := web == appDown(d)
	if stopped {
		if secure == "https: "+web {
			secure = "" // the same app is behind both probes: say it once
		}
		if listens(ctx, d.Address, d.Port) {
			web, stopped = badAnswer(d), false
		} else {
			r.Down = r.Direct && r.System && secure == ""
		}
	}
	problems := slices.DeleteFunc(append(lookups, web, secure), func(s string) bool { return s == "" })
	r.Detail = strings.Join(problems, "; ")
	return r, stopped
}

// probeDirect asks dnsmasq for name's IPv4 address, skipping the system
// resolver, and retries every retryEvery for up to directWindow while the
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
	retry := time.NewTicker(retryEvery)
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
	return e.probeWeb(ctx, d, "http", e.HTTPPort, nil)
}

// probeHTTPS asks for https://<name>/ on the domain's address and HTTPS port,
// verifying Caddy's certificate against the CAs macOS trusts, and reads the
// answer as probeHTTP does.
func (e Env) probeHTTPS(ctx context.Context, d store.Domain) string {
	tlsConfig := &tls.Config{RootCAs: e.RootCAs, MinVersion: tls.VersionTLS12}
	if p := e.probeWeb(ctx, d, "https", e.HTTPSPort, tlsConfig); p != "" {
		return "https: " + p
	}
	return ""
}

// probeWeb asks for <scheme>://<name>/ on the domain's address and port, and
// reads from the answer whether Caddy reached the app. While the connection
// or handshake fails it retries every retryEvery for up to httpWindow: apply
// may have just restarted Caddy, which needs a moment to listen again. An
// untrusted certificate ends it at once.
func (e Env) probeWeb(ctx context.Context, d store.Domain, scheme string, port int, tlsConfig *tls.Config) string {
	ctx, cancel := context.WithTimeout(ctx, httpWindow)
	defer cancel()
	caddyAddr := net.JoinHostPort(d.Address, strconv.Itoa(port))
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", caddyAddr)
			},
			TLSClientConfig:   tlsConfig,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	retry := time.NewTicker(retryEvery)
	defer retry.Stop()
	problem := "nothing answers on " + caddyAddr + ": is Caddy running?"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+d.Name+"/", nil)
		if err != nil {
			return oneLine(err.Error())
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close() // only the status and headers matter
			return httpProblem(resp.StatusCode, resp.Header, d, port)
		}
		if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
			return "Caddy's certificate isn't trusted" + runSetup
		}
		// A refused connection keeps the default problem. Another failure, such
		// as a handshake Caddy ends because it has no certificate for the name,
		// is reported as is, unless ctx cut the attempt short.
		if !isDialError(err) && ctx.Err() == nil {
			problem = oneLine(err.Error())
		}
		select {
		case <-ctx.Done():
			return problem
		case <-retry.C:
		}
	}
}

// isDialError reports whether err is a failure to connect, not one after.
func isDialError(err error) bool {
	op, ok := errors.AsType[*net.OpError](err)
	return ok && op.Op == "dial"
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
		return appDown(d)
	case server == "Caddy":
		return "caddy has no site for " + d.Name + ": run lodo apply"
	}
	return fmt.Sprintf("port %d on %s answered by %s, not Caddy", httpPort, d.Address, cmp.Or(server, "something"))
}

// appDown is the problem httpProblem reports when Caddy answered for the
// site but nothing listens on the domain's address and port.
func appDown(d store.Domain) string {
	return "app down: nothing answers on " + net.JoinHostPort(d.Address, strconv.Itoa(d.Port))
}

// badAnswer is the problem for an app that accepts a connection on the
// domain's address and port, yet Caddy got no HTTP answer from it.
func badAnswer(d store.Domain) string {
	return "app on " + net.JoinHostPort(d.Address, strconv.Itoa(d.Port)) + " gave Caddy no HTTP answer: does it serve HTTPS itself?"
}

// probeLocalhost dials each Localhost address but the domain's own on the
// domain's port, after nothing answered on the domain's address: a dev server
// started without a host often listens there instead. It returns that problem
// for the first address that answers, or "" when none does.
func (e Env) probeLocalhost(ctx context.Context, d store.Domain) string {
	for _, host := range e.Localhost {
		if host != d.Address && listens(ctx, host, d.Port) {
			return wrongHost(host, d.Port)
		}
	}
	return ""
}

// listens reports whether something accepts a TCP connection on host and
// port, dialed once within localTimeout.
func listens(ctx context.Context, host string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, localTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close() // only the connection mattered
	return true
}

// wrongHost is the problem for an app that listens on host and port instead
// of its name's address. It leaves the address out, so every stopped name on
// the port gets the same text.
func wrongHost(host string, port int) string {
	return "an app listens on " + net.JoinHostPort(host, strconv.Itoa(port)) + ", not on the name's address" + runDev
}

// needsApply reports whether lodo apply can fix one of a failed result's
// problems. It can't start an app or fix one that answers badly, and the
// problems that end with another fix need that one.
func needsApply(r Result) bool {
	d := store.Domain{Address: r.Address, Port: r.Port}
	for p := range strings.SplitSeq(r.Detail, "; ") {
		p = strings.TrimPrefix(p, "https: ")
		if p != appDown(d) && p != badAnswer(d) &&
			!strings.HasSuffix(p, removeLine) && !strings.HasSuffix(p, runSetup) && !strings.HasSuffix(p, runDev) {
			return true
		}
	}
	return false
}
