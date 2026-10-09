package system_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
	"github.com/sangdth/lodo/internal/system"
)

const rootPEM = "-----BEGIN CERTIFICATE-----\nstand-in\n-----END CERTIFICATE-----\n"

var secure = store.Domain{Name: "secure.test", Address: "127.0.1.1", Port: 3000, Enabled: true, HTTPS: true}

// trustCall is the command that trusts lodo's copy of Caddy's root.
func trustCall(p paths.Paths) string {
	return run.Line(p.Sudo, p.Security, "add-trusted-cert", "-d", "-r", "trustRoot", "-k", p.SystemKeychain, p.TrustedCA)
}

// untrustCall is the command that untrusts lodo's copy of Caddy's root.
func untrustCall(p paths.Paths) string {
	return run.Line(p.Sudo, p.Security, "remove-trusted-cert", "-d", p.TrustedCA)
}

func saveDomains(t *testing.T, p paths.Paths, domains []store.Domain) {
	t.Helper()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(p.DomainsJSON, domains); err != nil {
		t.Fatal(err)
	}
}

// TestSetup_TrustsCaddy checks setup trusts a copy of Caddy's root right after
// Caddy restarts, and only when an enabled domain has HTTPS on and Caddy is
// installed.
func TestSetup_TrustsCaddy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		domains   []store.Domain
		withCaddy bool
		trust     bool   // the root is trusted
		wantOut   string // a line the output holds
	}{
		{
			name: "enabled https domain", domains: []store.Domain{secure}, withCaddy: true, trust: true,
			wantOut: "✓ Caddy's local CA trusted for HTTPS names\n",
		},
		{
			name: "https domain disabled", withCaddy: true,
			domains: []store.Domain{{Name: "secure.test", Address: "127.0.1.1", Port: 3000, HTTPS: true}},
			wantOut: "– Caddy's local CA trusted for HTTPS names: no enabled name has HTTPS on\n",
		},
		{
			name: "no domains.json yet", withCaddy: true,
			wantOut: "– Caddy's local CA trusted for HTTPS names: no enabled name has HTTPS on\n",
		},
		{
			name: "caddy not installed", domains: []store.Domain{secure},
			wantOut: "– Caddy's local CA trusted for HTTPS names: Caddy is not installed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, r := newMac(t, tt.withCaddy)
			writeFile(t, p.CaddyRoot, rootPEM)
			if tt.domains != nil {
				saveDomains(t, p, tt.domains)
			}
			var out strings.Builder
			if err := system.Setup(context.Background(), p, r, &out); err != nil {
				t.Fatalf("Setup: %v\n%s", err, out.String())
			}
			want := setupCalls(p, tt.withCaddy)
			if tt.trust {
				// The trust comes right after the restart, before the last call.
				last := len(want) - 1
				want = append(want[:last:last], trustCall(p), want[last])
			}
			assertCalls(t, r.Calls(), want)
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output lacks %q:\n%s", tt.wantOut, out.String())
			}
			if !tt.trust {
				if exists(p.TrustedCA) {
					t.Errorf("%s written though nothing was trusted", p.TrustedCA)
				}
				return
			}
			assertFile(t, p.TrustedCA, rootPEM)
			if info, err := os.Stat(p.TrustedCA); err != nil || info.Mode().Perm() != 0o644 {
				t.Errorf("copy: %v, %v; want mode 0644", info, err)
			}
		})
	}
}

// TestSetup_TrustFails checks the trust step stops setup when Caddy's root is
// missing or not a plain file, trusting nothing, or when security fails.
func TestSetup_TrustFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		prepare   func(t *testing.T, p paths.Paths, r *run.Fake)
		wantErr   string
		wantTrust bool // add-trusted-cert ran
	}{
		{
			name:    "caddy never writes its root",
			prepare: func(*testing.T, paths.Paths, *run.Fake) {},
			wantErr: "caddy has not made its root",
		},
		{
			name: "root is a symlink",
			prepare: func(t *testing.T, p paths.Paths, _ *run.Fake) {
				t.Helper()
				other := filepath.Join(filepath.Dir(p.CaddyRoot), "other.crt")
				writeFile(t, other, rootPEM)
				if err := os.Symlink(other, p.CaddyRoot); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "not a regular file",
		},
		{
			name: "root is a directory",
			prepare: func(t *testing.T, p paths.Paths, _ *run.Fake) {
				t.Helper()
				if err := os.MkdirAll(p.CaddyRoot, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "not a regular file",
		},
		{
			name: "security fails",
			prepare: func(t *testing.T, p paths.Paths, r *run.Fake) {
				t.Helper()
				writeFile(t, p.CaddyRoot, rootPEM)
				r.Fail(trustCall(p), "authorization denied")
			},
			wantErr: "authorization denied", wantTrust: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, r := newMac(t, true)
			saveDomains(t, p, []store.Domain{secure})
			tt.prepare(t, p, r)
			err := system.Setup(context.Background(), p, r, &strings.Builder{})
			se, ok := errors.AsType[*system.StepError](err)
			if !ok || se.Step != "Caddy's local CA trusted for HTTPS names" || se.Fix != "lodo setup" {
				t.Fatalf("err = %#v, want the trust step's StepError", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to hold %q", err, tt.wantErr)
			}
			if got := slices.Contains(r.Calls(), trustCall(p)); got != tt.wantTrust {
				t.Errorf("add-trusted-cert ran: %v, want %v", got, tt.wantTrust)
			}
			if exists(p.TrustedCA) {
				t.Errorf("%s written though nothing was trusted", p.TrustedCA)
			}
		})
	}
}

// TestUninstall_Untrust checks uninstall untrusts the copy setup trusted and
// removes it, and when security fails, warns, keeps the copy and goes on.
func TestUninstall_Untrust(t *testing.T) {
	t.Parallel()

	for _, fails := range []bool{false, true} {
		name := "untrusts"
		if fails {
			name = "security fails"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, r := newMac(t, true)
			writeFile(t, p.SystemCaddyfile, system.CaddyBlock(p))
			writeFile(t, p.TrustedCA, rootPEM)
			if fails {
				r.Fail(untrustCall(p), "The specified item could not be found in the keychain.")
			}
			var out strings.Builder
			if err := system.Uninstall(context.Background(), p, r, &out); err != nil {
				t.Fatalf("Uninstall: %v\n%s", err, out.String())
			}
			want := uninstallCalls(p, false, true)
			i := slices.Index(want, run.Line(p.Brew, "services", "stop", "caddy"))
			want = slices.Insert(want, i, untrustCall(p))
			assertCalls(t, r.Calls(), want)
			if !fails {
				if exists(p.TrustedCA) {
					t.Errorf("%s kept after untrust", p.TrustedCA)
				}
				if line := "✓ Caddy's local CA no longer trusted\n"; !strings.Contains(out.String(), line) {
					t.Errorf("output lacks %q:\n%s", line, out.String())
				}
				return
			}
			assertFile(t, p.TrustedCA, rootPEM)
			line := "! Caddy's local CA no longer trusted: "
			hand := "remove it by hand: sudo security remove-trusted-cert -d " + p.TrustedCA + "\n"
			if !strings.Contains(out.String(), line) || !strings.Contains(out.String(), hand) {
				t.Errorf("output lacks the warning %q ... %q:\n%s", line, hand, out.String())
			}
		})
	}
}
