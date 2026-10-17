// Package caddy generates lodo's Caddyfile and validates it.
package caddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
)

// localOnly opens every site: it closes a connection that comes from outside
// this Mac without an answer. remote_ip matches the connection's own address,
// never a forwarded header.
const localOnly = "\t@outside not remote_ip 127.0.0.0/8 ::1\n\tabort @outside\n"

// Config returns lodo's Caddyfile: one site per enabled domain with a port, in
// store.Sort order, that forwards http://<name> to <address>:<port> (see
// proxy). The http:// prefix keeps Caddy on port 80, without automatic HTTPS.
// A domain with HTTPS on lists both http://<name> and https://<name> in one
// site with tls internal, so Caddy serves both, with a certificate from its
// local CA and no redirect from one to the other. Caddy listens on every
// address, because macOS lets a user listen on port 80 only that way, so each
// site starts by aborting any connection from outside this Mac. With no such
// domain it is the header line alone, which Caddy takes as a config with no
// sites.
func Config(domains []store.Domain) string {
	var b strings.Builder
	b.WriteString(store.GeneratedHeader)
	for _, d := range store.Sort(domains) {
		switch {
		case !d.HasSite():
		case d.HTTPS:
			fmt.Fprintf(&b, "\nhttp://%s, https://%s {\n%s\ttls internal\n%s}\n", d.Name, d.Name, localOnly, proxy(d))
		default:
			fmt.Fprintf(&b, "\nhttp://%s {\n%s%s}\n", d.Name, localOnly, proxy(d))
		}
	}
	return b.String()
}

// proxy returns a site's lines that forward it to its app. Host stays the
// .test name, so Mailpit's websocket, SvelteKit's and Better Auth's origin
// checks and Next's Server Actions all see the name; Vite allows the name
// because lodo dev's hook sets __VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS. Only
// Origin changes, to http://localhost:<port>, which Next 16's dev check allows
// with no allowedDevOrigins: only on Next's dev paths, under any basePath or
// assetPrefix, and only when it is the site's own origin, so another page's
// Origin still fails Next's check and a Server Action keeps the name. The
// prototype that settled these lines is in docs/plan.md, Phase 11.
func proxy(d store.Domain) string {
	scheme := "http"
	if d.HTTPS {
		scheme = "https?"
	}
	return fmt.Sprintf("\t@own_dev path */_next/* */__nextjs*\n"+
		"\trequest_header @own_dev Origin \"^%s://%s$\" http://localhost:%d\n"+
		"\treverse_proxy %s:%d\n",
		scheme, regexp.QuoteMeta(d.Name), d.Port, d.Address, d.Port)
}

// Validate runs caddy validate on the Caddyfile at path. When caddy rejects
// it, the error holds caddy's own message, without its log lines.
func Validate(ctx context.Context, r run.Runner, caddyBin, path string) error {
	_, err := r.Run(ctx, caddyBin, "validate", "--config", path, "--adapter", "caddyfile")
	if err == nil {
		return nil
	}
	if re, ok := errors.AsType[*run.Error](err); ok {
		if msg := lastError(re.Stderr); msg != "" {
			return fmt.Errorf("caddy validate: %s", msg)
		}
	}
	return fmt.Errorf("caddy validate: %w", err)
}

// logTail is how much of the end of Caddy's log LogError reads.
const logTail = 64 << 10

// LogError returns the last error message in the last logTail bytes of
// Caddy's log at path, or "" when there is none, the log can't be read, or
// Caddy has not written to it since since.
func LogError(path string, since time.Time) string {
	// Stat first: opening a FIFO blocks.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(since) {
		return ""
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is paths.CaddyLog, Homebrew's own log
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }() // read only: a close error loses nothing
	if _, err := f.Seek(max(0, info.Size()-logTail), io.SeekStart); err != nil {
		return ""
	}
	tail, err := io.ReadAll(io.LimitReader(f, logTail))
	if err != nil {
		return ""
	}
	return lastError(string(tail))
}

// lastError returns the last error message in caddy's stderr, or "". Caddy
// 2.11 logs a failed command as a JSON line at level "error" with the message
// in msg; a plain "Error: ..." line, cobra's default format, counts too.
func lastError(stderr string) string {
	var msg string
	for line := range strings.Lines(stderr) {
		line = strings.TrimSpace(line)
		if text, ok := strings.CutPrefix(line, "Error:"); ok {
			msg = strings.TrimSpace(text)
			continue
		}
		var entry struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Level == "error" && entry.Msg != "" {
			msg = entry.Msg
		}
	}
	return msg
}
