// Package caddy generates lodo's Caddyfile and validates it.
package caddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
)

// Config returns lodo's Caddyfile: one site per enabled domain with a port, in
// store.Sort order, that forwards http://<name> to <address>:<port>. The
// http:// prefix keeps Caddy on port 80, without automatic HTTPS. A domain with
// HTTPS on lists both http://<name> and https://<name> in one site with tls
// internal, so Caddy serves both, with a certificate from its local CA and no
// redirect from one to the other. Caddy listens on every address, because
// macOS lets a user listen on port 80 only that way, so each site starts by
// aborting any connection from outside this Mac. With no such domain it is the
// header line alone, which Caddy takes as a config with no sites.
// localOnly opens every site: it closes a connection that comes from outside
// this Mac without an answer. remote_ip matches the connection's own address,
// never a forwarded header.
const localOnly = "\t@outside not remote_ip 127.0.0.0/8 ::1\n\tabort @outside\n"

func Config(domains []store.Domain) string {
	var b strings.Builder
	b.WriteString(store.GeneratedHeader)
	for _, d := range store.Sort(domains) {
		switch {
		case !d.Enabled || d.Port == 0:
		case d.HTTPS:
			fmt.Fprintf(&b, "\nhttp://%s, https://%s {\n%s\ttls internal\n\treverse_proxy %s:%d\n}\n",
				d.Name, d.Name, localOnly, d.Address, d.Port)
		default:
			fmt.Fprintf(&b, "\nhttp://%s {\n%s\treverse_proxy %s:%d\n}\n", d.Name, localOnly, d.Address, d.Port)
		}
	}
	return b.String()
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
