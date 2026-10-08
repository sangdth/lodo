package store_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sangdth/lcd/internal/store"
)

func TestValidateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantMsg string // empty means valid
	}{
		{name: "project", in: "flowy.lcd"},
		{name: "hyphen and subdomain", in: "a-b.dev.lcd"},
		{name: "deep subdomain", in: "a.test.crm.lcd"},
		{name: "digits", in: "web2.lcd"},
		{name: "63-char label", in: strings.Repeat("a", 63) + ".lcd"},
		{name: "empty", in: "", wantMsg: "name is empty"},
		{name: "bare tld", in: "lcd", wantMsg: "name must end in .lcd"},
		{name: "dot lcd", in: ".lcd", wantMsg: "name needs a label before .lcd"},
		{name: "uppercase", in: "X.LCD", wantMsg: "name must be lowercase"},
		{name: "other tld", in: "x.com", wantMsg: "name must end in .lcd"},
		{name: "bonjour's tld", in: "flowy.local", wantMsg: "name must end in .lcd"},
		{name: "path traversal", in: "../x.lcd", wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters"},
		{name: "embedded newline", in: "x.lcd\nfoo", wantMsg: "name must end in .lcd"},
		{name: "newline before suffix", in: "x\n.lcd", wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters"},
		{name: "leading hyphen", in: "-x.lcd", wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters"},
		{name: "trailing hyphen", in: "x-.lcd", wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters"},
		{name: "empty label", in: "a..lcd", wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters"},
		{name: "64-char label", in: strings.Repeat("a", 64) + ".lcd", wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters"},
		{name: "space", in: "a b.lcd", wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters"},
		{name: "underscore", in: "a_b.lcd", wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters"},
		{name: "too long", in: strings.Repeat("abcdefgh.", 28) + "lcd", wantMsg: "name is longer than 253 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertFieldErr(t, store.ValidateName(tt.in), store.FieldName, tt.wantMsg)
		})
	}
}

func TestValidateAddress(t *testing.T) {
	t.Parallel()

	const msg = "address must be an IPv4 address in 127.0.0.0/8, like 127.0.1.3"
	tests := []struct {
		name    string
		in      string
		wantMsg string
	}{
		{name: "localhost", in: "127.0.0.1"},
		{name: "own block", in: "127.0.1.3"},
		{name: "top of loopback", in: "127.255.255.254"},
		{name: "private network", in: "10.0.0.1", wantMsg: msg},
		{name: "ipv6 loopback", in: "::1", wantMsg: msg},
		{name: "three octets", in: "127.0.1", wantMsg: msg},
		{name: "leading zero", in: "127.000.0.1", wantMsg: msg},
		{name: "text", in: "abc", wantMsg: msg},
		{name: "empty", in: "", wantMsg: msg},
		{name: "ipv4-mapped ipv6", in: "::ffff:127.0.0.1", wantMsg: msg},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertFieldErr(t, store.ValidateAddress(tt.in), store.FieldAddress, tt.wantMsg)
		})
	}
}

func TestValidatePort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      int
		wantMsg string
	}{
		{name: "none", in: 0},
		{name: "lowest", in: 1},
		{name: "next dev", in: 3000},
		{name: "highest", in: 65535},
		{name: "negative", in: -1, wantMsg: "port must be between 1 and 65535"},
		{name: "too high", in: 70000, wantMsg: "port must be between 1 and 65535"},
		{name: "caddy's own", in: 80, wantMsg: "port 80 is where Caddy listens; use the app's own port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertFieldErr(t, store.ValidatePort(tt.in), store.FieldPort, tt.wantMsg)
		})
	}
}

func TestParsePort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    int
		wantMsg string
	}{
		{name: "empty means none", in: "", want: 0},
		{name: "spaces mean none", in: "  ", want: 0},
		{name: "one", in: "1", want: 1},
		{name: "next dev", in: "3000", want: 3000},
		{name: "highest", in: "65535", want: 65535},
		{name: "zero", in: "0", wantMsg: "port must be between 1 and 65535"},
		{name: "too high", in: "70000", wantMsg: "port must be between 1 and 65535"},
		{name: "text", in: "abc", wantMsg: "port must be a number"},
		{name: "caddy's own", in: "80", wantMsg: "port 80 is where Caddy listens; use the app's own port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := store.ParsePort(tt.in)
			assertFieldErr(t, err, store.FieldPort, tt.wantMsg)
			if got != tt.want {
				t.Errorf("ParsePort(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsOwn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "first", in: "127.0.1.1", want: true},
		{name: "last", in: "127.0.1.50", want: true},
		{name: "past the block", in: "127.0.1.51", want: false},
		{name: "zero", in: "127.0.1.0", want: false},
		{name: "localhost", in: "127.0.0.1", want: false},
		{name: "leading zero", in: "127.0.1.01", want: false},
		{name: "other subnet", in: "127.0.10.1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := store.IsOwn(tt.in); got != tt.want {
				t.Errorf("IsOwn(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "project itself", in: "crm.lcd", want: "crm.lcd"},
		{name: "subdomain", in: "test.crm.lcd", want: "crm.lcd"},
		{name: "deep subdomain", in: "a.b.crm.lcd", want: "crm.lcd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := store.Project(tt.in); got != tt.want {
				t.Errorf("Project(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		listed   []string
		in       string
		want     string
		wantOkay bool
	}{
		{name: "nearest listed suffix", listed: []string{"crm.lcd", "test.crm.lcd"}, in: "a.test.crm.lcd", want: "test.crm.lcd", wantOkay: true},
		{name: "skips a gap", listed: []string{"crm.lcd"}, in: "a.test.crm.lcd", want: "crm.lcd", wantOkay: true},
		{name: "none listed", listed: []string{"flowy.lcd"}, in: "a.test.crm.lcd", wantOkay: false},
		{name: "not a label boundary", listed: []string{"rm.lcd"}, in: "crm.lcd", wantOkay: false},
		{name: "itself is not its parent", listed: []string{"crm.lcd"}, in: "crm.lcd", wantOkay: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var domains []store.Domain
			for _, n := range tt.listed {
				domains = append(domains, store.Domain{Name: n, Address: "127.0.0.1", Enabled: true})
			}
			got, ok := store.Parent(domains, tt.in)
			if ok != tt.wantOkay || got.Name != tt.want {
				t.Errorf("Parent(%q) = %q, %v; want %q, %v", tt.in, got.Name, ok, tt.want, tt.wantOkay)
			}
		})
	}
}

func TestNextFree(t *testing.T) {
	t.Parallel()

	full := make([]store.Domain, 0, store.OwnLast)
	for i := store.OwnFirst; i <= store.OwnLast; i++ {
		full = append(full, store.Domain{Name: fmt.Sprintf("p%d.lcd", i), Address: store.OwnAddress(i)})
	}
	tests := []struct {
		name    string
		used    []string
		in      []store.Domain
		want    string
		wantErr error
	}{
		{name: "empty list", want: "127.0.1.1"},
		{name: "after two", used: []string{"127.0.1.1", "127.0.1.2"}, want: "127.0.1.3"},
		{name: "fills a gap", used: []string{"127.0.1.1", "127.0.1.3"}, want: "127.0.1.2"},
		{name: "shared localhost ignored", used: []string{"127.0.0.1"}, want: "127.0.1.1"},
		{name: "block full", in: full, wantErr: store.ErrBlockFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			domains := tt.in
			for i, a := range tt.used {
				domains = append(domains, store.Domain{Name: fmt.Sprintf("d%d.lcd", i), Address: a})
			}
			got, err := store.NextFree(domains)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NextFree = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSort(t *testing.T) {
	t.Parallel()

	in := domains("flowy.lcd", "test.crm.lcd", "a.api.crm.lcd", "crm.lcd", "api.crm.lcd", "b.lcd")
	want := []string{"b.lcd", "crm.lcd", "api.crm.lcd", "a.api.crm.lcd", "test.crm.lcd", "flowy.lcd"}
	got := names(store.Sort(in))
	if !slices.Equal(got, want) {
		t.Errorf("Sort = %q, want %q", got, want)
	}
	if names(in)[0] != "flowy.lcd" {
		t.Error("Sort changed its input")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []store.Domain
		wantErr string
	}{
		{name: "empty", in: nil},
		{name: "project and subdomains share", in: []store.Domain{
			{Name: "crm.lcd", Address: "127.0.1.1"},
			{Name: "dashboard.crm.lcd", Address: "127.0.1.1", Port: 3000},
		}},
		{name: "bad entry named", in: []store.Domain{{Name: "X.lcd", Address: "127.0.0.1"}}, wantErr: `"X.lcd": name must be lowercase`},
		{name: "duplicate", in: []store.Domain{
			{Name: "crm.lcd", Address: "127.0.1.1"},
			{Name: "crm.lcd", Address: "127.0.1.2"},
		}, wantErr: `"crm.lcd": crm.lcd is already listed`},
		{name: "own address across projects", in: []store.Domain{
			{Name: "crm.lcd", Address: "127.0.1.1"},
			{Name: "flowy.lcd", Address: "127.0.1.1"},
		}, wantErr: `"flowy.lcd": 127.0.1.1 belongs to crm.lcd`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := store.Validate(tt.in)
			if got := errString(err); got != tt.wantErr {
				t.Errorf("Validate = %q, want %q", got, tt.wantErr)
			}
		})
	}
}

func TestAdd(t *testing.T) {
	t.Parallel()

	base := []store.Domain{
		{Name: "crm.lcd", Address: "127.0.1.1", Enabled: true},
		{Name: "flowy.lcd", Address: "127.0.1.3", Enabled: true},
		{Name: "old.lcd", Address: "127.0.0.1"},
	}
	tests := []struct {
		name      string
		add       store.Domain
		wantField string
		wantMsg   string
	}{
		{name: "subdomain shares its project's address", add: store.Domain{Name: "test.crm.lcd", Address: "127.0.1.1"}},
		{name: "subdomain takes its own address", add: store.Domain{Name: "test.crm.lcd", Address: "127.0.1.4"}},
		{name: "two share localhost", add: store.Domain{Name: "new.lcd", Address: "127.0.0.1"}},
		{name: "subdomain with a port", add: store.Domain{Name: "dashboard.crm.lcd", Address: "127.0.1.1", Port: 3000}},
		{name: "duplicate name", add: store.Domain{Name: "crm.lcd", Address: "127.0.1.9"}, wantField: store.FieldName, wantMsg: "crm.lcd is already listed"},
		{name: "another project's address", add: store.Domain{Name: "web.lcd", Address: "127.0.1.3"}, wantField: store.FieldAddress, wantMsg: "127.0.1.3 belongs to flowy.lcd"},
		{name: "subdomain of another project", add: store.Domain{Name: "api.web.lcd", Address: "127.0.1.1"}, wantField: store.FieldAddress, wantMsg: "127.0.1.1 belongs to crm.lcd"},
		{name: "bad port", add: store.Domain{Name: "api.crm.lcd", Address: "127.0.1.1", Port: 80}, wantField: store.FieldPort, wantMsg: "port 80 is where Caddy listens; use the app's own port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := store.Add(base, tt.add)
			assertFieldErr(t, err, tt.wantField, tt.wantMsg)
			if tt.wantMsg != "" {
				return
			}
			if len(got) != len(base)+1 || !slices.Contains(got, tt.add) {
				t.Errorf("Add = %v, want base plus %v", got, tt.add)
			}
			if !slices.Equal(names(got), names(store.Sort(got))) {
				t.Errorf("Add result not sorted: %q", names(got))
			}
		})
	}
	if len(base) != 3 {
		t.Error("Add changed its input")
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()

	base := []store.Domain{
		{Name: "crm.lcd", Address: "127.0.1.1", Enabled: true},
		{Name: "test.crm.lcd", Address: "127.0.1.1", Enabled: true},
		{Name: "flowy.lcd", Address: "127.0.1.3", Enabled: true},
	}

	t.Run("moving a parent leaves subdomains", func(t *testing.T) {
		t.Parallel()
		got, err := store.Update(base, "crm.lcd", store.Domain{Name: "crm.lcd", Address: "127.0.1.5", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if a := find(got, "test.crm.lcd").Address; a != "127.0.1.1" {
			t.Errorf("test.crm.lcd moved to %s, want it left at 127.0.1.1", a)
		}
		if a := find(got, "crm.lcd").Address; a != "127.0.1.5" {
			t.Errorf("crm.lcd at %s, want 127.0.1.5", a)
		}
	})
	t.Run("rename keeps the row count", func(t *testing.T) {
		t.Parallel()
		got, err := store.Update(base, "flowy.lcd", store.Domain{Name: "flow.lcd", Address: "127.0.1.3"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || find(got, "flow.lcd").Name == "" || find(got, "flowy.lcd").Name != "" {
			t.Errorf("Update = %q", names(got))
		}
	})
	t.Run("keeping its own name is allowed", func(t *testing.T) {
		t.Parallel()
		if _, err := store.Update(base, "flowy.lcd", store.Domain{Name: "flowy.lcd", Address: "127.0.1.3", Port: 3000}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("renaming onto another row", func(t *testing.T) {
		t.Parallel()
		_, err := store.Update(base, "flowy.lcd", store.Domain{Name: "crm.lcd", Address: "127.0.1.3"})
		assertFieldErr(t, err, store.FieldName, "crm.lcd is already listed")
	})
	t.Run("missing row", func(t *testing.T) {
		t.Parallel()
		if _, err := store.Update(base, "nope.lcd", store.Domain{Name: "nope.lcd", Address: "127.0.0.1"}); errString(err) != "nope.lcd is not listed" {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRemove(t *testing.T) {
	t.Parallel()

	base := []store.Domain{
		{Name: "crm.lcd", Address: "127.0.1.1"},
		{Name: "flowy.lcd", Address: "127.0.1.2"},
	}
	got, err := store.Remove(base, "crm.lcd")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got), []string{"flowy.lcd"}) {
		t.Errorf("Remove = %q", names(got))
	}
	if free, _ := store.NextFree(got); free != "127.0.1.1" {
		t.Errorf("NextFree after Remove = %s, want the freed 127.0.1.1", free)
	}
	if len(base) != 2 || base[0].Name != "crm.lcd" {
		t.Error("Remove changed its input")
	}
	if _, err := store.Remove(base, "nope.lcd"); err == nil {
		t.Error("Remove of a missing row: err = nil")
	}
}

func TestToggle(t *testing.T) {
	t.Parallel()

	base := []store.Domain{{Name: "crm.lcd", Address: "127.0.1.1", Enabled: true}}
	got, err := store.Toggle(base, "crm.lcd")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Enabled || !base[0].Enabled {
		t.Errorf("Toggle: got enabled=%v, input enabled=%v; want false, true", got[0].Enabled, base[0].Enabled)
	}
	if _, err := store.Toggle(base, "nope.lcd"); err == nil {
		t.Error("Toggle of a missing row: err = nil")
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		content  *string
		want     []string
		wantErrs string // substring
	}{
		{name: "missing file is empty", content: nil, want: nil},
		{name: "sorted on load", content: ptr(`{"version":1,"domains":[{"name":"flowy.lcd","address":"127.0.1.3","enabled":true},{"name":"crm.lcd","address":"127.0.1.1","enabled":false}]}`), want: []string{"crm.lcd", "flowy.lcd"}},
		{name: "bad json", content: ptr(`{`), wantErrs: "parse "},
		{name: "unknown field", content: ptr(`{"version":1,"domains":[{"name":"crm.lcd","addr":"127.0.1.1"}]}`), wantErrs: `unknown field "addr"`},
		{name: "wrong version", content: ptr(`{"version":2,"domains":[]}`), wantErrs: "format version 2, this lcd reads version 1"},
		{name: "invalid domain", content: ptr(`{"version":1,"domains":[{"name":"crm.com","address":"127.0.1.1","enabled":true}]}`), wantErrs: `"crm.com": name must end in .lcd`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "domains.json")
			if tt.content != nil {
				if err := os.WriteFile(path, []byte(*tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := store.Load(path)
			if tt.wantErrs != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrs) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErrs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(names(got), tt.want) {
				t.Errorf("Load = %q, want %q", names(got), tt.want)
			}
		})
	}
}

func TestSave(t *testing.T) {
	t.Parallel()

	t.Run("round trip", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nested", "domains.json")
		in := []store.Domain{
			{Name: "test.crm.lcd", Address: "127.0.1.1", Port: 3000, Enabled: true},
			{Name: "crm.lcd", Address: "127.0.1.1", Enabled: true},
			{Name: "old.lcd", Address: "127.0.0.1"},
		}
		if err := store.Save(path, in); err != nil {
			t.Fatal(err)
		}
		got, err := store.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, store.Sort(in)) {
			t.Errorf("Load after Save = %v, want %v", got, store.Sort(in))
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(raw), `"port"`) != 1 {
			t.Errorf("port written for rows without one:\n%s", raw)
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("dir holds %d entries, want only domains.json", len(entries))
		}
	})
	t.Run("empty list writes an empty array", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "domains.json")
		if err := store.Save(path, nil); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := "{\n  \"version\": 1,\n  \"domains\": []\n}\n"; string(raw) != want {
			t.Errorf("file = %q, want %q", raw, want)
		}
	})
	t.Run("invalid list is not written", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "domains.json")
		err := store.Save(path, []store.Domain{{Name: "bad", Address: "127.0.0.1"}})
		if err == nil {
			t.Fatal("err = nil, want a validation error")
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("file exists after a failed Save: %v", statErr)
		}
	})
}

// assertFieldErr checks err is a *store.FieldError for field with msg, or nil
// when msg is empty.
func assertFieldErr(t *testing.T, err error, field, msg string) {
	t.Helper()
	if msg == "" {
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		return
	}
	fe, ok := errors.AsType[*store.FieldError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want *store.FieldError", err, err)
	}
	if fe.Field != field || fe.Msg != msg {
		t.Errorf("err = {%s %q}, want {%s %q}", fe.Field, fe.Msg, field, msg)
	}
}

func domains(names ...string) []store.Domain {
	ds := make([]store.Domain, 0, len(names))
	for _, n := range names {
		ds = append(ds, store.Domain{Name: n, Address: "127.0.0.1"})
	}
	return ds
}

func names(ds []store.Domain) []string {
	var ns []string
	for _, d := range ds {
		ns = append(ns, d.Name)
	}
	return ns
}

func find(ds []store.Domain, name string) store.Domain {
	for _, d := range ds {
		if d.Name == name {
			return d
		}
	}
	return store.Domain{}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func ptr(s string) *string { return &s }
