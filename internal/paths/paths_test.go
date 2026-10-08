package paths_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sangdth/lcd/internal/paths"
)

func TestDefault(t *testing.T) {
	t.Parallel()

	p, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	if p.User == "" || p.Home == "" {
		t.Fatalf("user %q, home %q: want both set", p.User, p.Home)
	}
	want := map[string]string{
		"DomainsJSON":   filepath.Join(p.Home, ".config/lcd/domains.json"),
		"SystemConf":    "/opt/homebrew/etc/dnsmasq.conf",
		"Script":        "/Library/Application Support/lcd/apply-resolvers.sh",
		"Sudoers":       "/etc/sudoers.d/lcd",
		"ResolverDir":   "/etc/resolver",
		"LoopbackPlist": "/Library/LaunchDaemons/io.lcd.loopback.plist",
		"Sudo":          "/usr/bin/sudo",
	}
	v := reflect.ValueOf(p)
	for field, path := range want {
		if got := v.FieldByName(field).String(); got != path {
			t.Errorf("%s = %q, want %q", field, got, path)
		}
	}
}

func TestForTest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	p := paths.ForTest(root)
	v := reflect.ValueOf(p)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		f := v.Field(i)
		switch {
		case name == "User":
			if f.String() != "tester" {
				t.Errorf("User = %q, want tester", f.String())
			}
		case f.Kind() == reflect.String:
			if !strings.HasPrefix(f.String(), root+string(filepath.Separator)) {
				t.Errorf("%s = %q, want it under %s", name, f.String(), root)
			}
		case f.Kind() == reflect.Slice:
			for j := range f.Len() {
				if s := f.Index(j).String(); !strings.HasPrefix(s, root+string(filepath.Separator)) {
					t.Errorf("%s[%d] = %q, want it under %s", name, j, s, root)
				}
			}
		}
	}
}
