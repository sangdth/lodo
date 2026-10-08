package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lodo/internal/paths"
	"github.com/sangdth/lodo/internal/run"
	"github.com/sangdth/lodo/internal/store"
)

func TestForm_Prefill(t *testing.T) {
	t.Parallel()

	m := send(ready(&fakeBackend{}, sample), "A")
	if m.mode != modeForm {
		t.Fatalf("mode = %v after a, want the form", m.mode)
	}
	assertAddress(t, m, "127.0.1.2", "the lowest free own address")

	m = typeText(m, "test.crm")
	assertAddress(t, m, "127.0.1.1", "crm.test's address; next free: 127.0.1.2")

	m = typeText(send(m, "tab"), "") // into the address field
	m = typeText(clearField(m), "127.0.1.9")
	m = send(m, "shift+tab")
	m = typeText(m, "x")
	assertAddress(t, m, "127.0.1.9", "crm.test's address; next free: 127.0.1.2")
}

func TestForm_PrefillWhenTheBlockIsFull(t *testing.T) {
	t.Parallel()

	var full []store.Domain
	for i := store.OwnFirst; i <= store.OwnLast; i++ {
		full = append(full, store.Domain{Name: "p" + string(rune('a'+i%26)) + strings.Repeat("x", i/26) + ".test", Address: store.OwnAddress(i)})
	}
	m := send(ready(&fakeBackend{}, full), "A")
	assertAddress(t, m, "127.0.0.1", "all own addresses are taken, so it shares 127.0.0.1")
}

func TestForm_Add(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := send(ready(b, sample), "A")
	m = typeText(m, "api.crm")
	m = send(m, "enter")

	want := store.Domain{Name: "api.crm.test", Address: "127.0.1.1", Enabled: true}
	if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) {
		t.Fatalf("saved %v, want it to hold %+v", b.saved, want)
	}
	if m.mode != modeList {
		t.Errorf("mode = %v after saving, want the list", m.mode)
	}
	if d, _ := m.selected(); d.Name != "api.crm.test" {
		t.Errorf("cursor on %q after adding, want the new row", d.Name)
	}
}

func TestForm_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		backend   *fakeBackend
		fields    [fieldCount]string // typed into name, address, port; "" leaves the prefill
		wantField int
		wantMsg   string
	}{
		{name: "bad name", fields: [fieldCount]string{"Web"}, wantField: fieldName, wantMsg: "name must be lowercase"},
		{
			name: "a bad label", fields: [fieldCount]string{"web_app"},
			wantField: fieldName, wantMsg: "name may use only a-z, 0-9 and '-' inside labels of 1-63 characters",
		},
		{name: "duplicate", fields: [fieldCount]string{"crm"}, wantField: fieldName, wantMsg: "crm.test is already listed"},
		{
			name: "another project's address", fields: [fieldCount]string{"web", "127.0.1.3"},
			wantField: fieldAddress, wantMsg: "127.0.1.3 belongs to flowy.test",
		},
		{
			name: "bad address", fields: [fieldCount]string{"web", "10.0.0.1"},
			wantField: fieldAddress, wantMsg: "address must be an IPv4 address in 127.0.0.0/8, like 127.0.1.3",
		},
		{name: "bad port", fields: [fieldCount]string{"web", "", "abc"}, wantField: fieldPort, wantMsg: "port must be a number"},
		{
			name: "caddy's port", fields: [fieldCount]string{"web", "", "80"},
			wantField: fieldPort, wantMsg: "port 80 is where Caddy listens; use the app's own port",
		},
		{
			name:    "a port before caddy is ready",
			backend: &fakeBackend{portsErr: errors.New("caddy is not installed: brew install caddy, then lodo setup")},
			fields:  [fieldCount]string{"web", "", "3000"}, wantField: fieldPort,
			wantMsg: "caddy is not installed: brew install caddy, then lodo setup",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := tt.backend
			if b == nil {
				b = &fakeBackend{}
			}
			m := fill(send(ready(b, sample), "A"), tt.fields)
			m = send(m, "enter")
			if m.mode != modeForm {
				t.Fatalf("the form closed on an invalid domain")
			}
			if got := m.form.errs[tt.wantField]; got != tt.wantMsg {
				t.Errorf("error under field %d = %q, want %q", tt.wantField, got, tt.wantMsg)
			}
			if m.form.focus != tt.wantField {
				t.Errorf("cursor in field %d, want %d", m.form.focus, tt.wantField)
			}
			if len(b.saved) != 0 {
				t.Errorf("saved an invalid list: %v", b.saved)
			}
		})
	}
}

func TestForm_Allowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fields [fieldCount]string
		want   store.Domain
	}{
		{name: "subdomain shares its project's address", fields: [fieldCount]string{"test.crm"}, want: store.Domain{Name: "test.crm.test", Address: "127.0.1.1", Enabled: true}},
		{name: "shared localhost", fields: [fieldCount]string{"web", "127.0.0.1"}, want: store.Domain{Name: "web.test", Address: "127.0.0.1", Enabled: true}},
		{name: "a port", fields: [fieldCount]string{"web", "", "3000"}, want: store.Domain{Name: "web.test", Address: "127.0.1.2", Port: 3000, Enabled: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{}
			m := send(fill(send(ready(b, sample), "A"), tt.fields), "enter")
			if len(b.saved) != 1 || !slices.Contains(b.saved[0], tt.want) {
				t.Errorf("saved %v, want it to hold %+v; form errors %q", b.saved, tt.want, m.form.errs)
			}
		})
	}
}

func TestForm_Edit(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := ready(b, sample)
	m = send(send(m, "down"), "down") // flowy.test
	m = send(m, "e")
	if got := m.form.inputs[fieldName].Value(); got != "flowy" {
		t.Fatalf("name field = %q, want the selected name without .test", got)
	}
	m = send(send(m, "tab"), "tab")
	m = typeText(m, "3000")
	m = send(m, "enter")

	want := store.Domain{Name: "flowy.test", Address: "127.0.1.3", Port: 3000, Enabled: true}
	if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) || len(b.saved[0]) != len(sample) {
		t.Errorf("saved %v, want flowy.test with port 3000 and nothing else changed", b.saved)
	}
}

func TestForm_EditKeepsOff(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := send(send(send(send(ready(b, sample), "down"), "down"), "down"), "e") // old.test, which is off
	m = typeText(clearField(m), "older")
	m = send(m, "enter")
	want := store.Domain{Name: "older.test", Address: "127.0.0.1"}
	if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) {
		t.Errorf("saved %v, want old.test renamed, still off, at its address", b.saved)
	}
	if m.mode != modeList {
		t.Errorf("mode = %v after saving the edit, want the list", m.mode)
	}
}

func TestForm_NameSuffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		typed string
		want  string // the name field as drawn, without styles
	}{
		{name: "empty shows the placeholder", typed: "", want: "app.flowy .test"},
		{name: "labels", typed: "web", want: "web .test"},
		{name: "a suffix typed out of habit", typed: "web.test", want: "web.test .test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := typeText(send(ready(&fakeBackend{}, sample), "A"), tt.typed)
			if got := ansi.Strip(m.nameInput()); got != tt.want {
				t.Errorf("name field = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestForm_SuffixTypedOutOfHabit(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := send(typeText(send(ready(b, sample), "A"), "web.test"), "enter")
	want := store.Domain{Name: "web.test", Address: "127.0.1.2", Enabled: true}
	if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) {
		t.Errorf("saved %v, want it to hold %+v, not web.test.test; form errors %q", b.saved, want, m.form.errs)
	}
}

func TestForm_Subdomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		typed string
		want  string
	}{
		{name: "labels", typed: "api", want: "api.crm.test"},
		{name: "the parent typed out of habit", typed: "api.crm.test", want: "api.crm.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{}
			m := send(ready(b, sample), "a") // on crm.test
			assertAddress(t, m, "127.0.1.1", "crm.test's address; next free: 127.0.1.2")
			if got := ansi.Strip(m.nameInput()); got != "api .crm.test" {
				t.Errorf("empty name field = %q, want the placeholder and the parent", got)
			}
			if !strings.Contains(ansi.Strip(m.formView()), "Add a subdomain of crm.test") {
				t.Error("the title does not name the parent")
			}
			m = send(typeText(m, tt.typed), "enter")
			want := store.Domain{Name: tt.want, Address: "127.0.1.1", Enabled: true}
			if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) {
				t.Errorf("saved %v, want it to hold %+v; form errors %q", b.saved, want, m.form.errs)
			}
		})
	}
}

func TestForm_EditKeepsTheParent(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := send(send(ready(b, sample), "down"), "e") // dashboard.crm.test
	if got := ansi.Strip(m.nameInput()); got != "dashboard .crm.test" {
		t.Errorf("name field = %q, want the label and the locked parent", got)
	}
	if !strings.Contains(ansi.Strip(m.formView()), "Edit dashboard.crm.test") {
		t.Error("the title does not name the edited domain")
	}
	m = send(typeText(clearField(m), "admin"), "enter")
	want := store.Domain{Name: "admin.crm.test", Address: "127.0.1.1", Port: 3000, Enabled: true}
	if len(b.saved) != 1 || !slices.Contains(b.saved[0], want) || slices.ContainsFunc(b.saved[0], func(d store.Domain) bool { return d.Name == "dashboard.crm.test" }) {
		t.Errorf("saved %v, want dashboard.crm.test renamed to %+v; form errors %q", b.saved, want, m.form.errs)
	}
}

func TestForm_EscCancels(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := typeText(send(ready(b, sample), "A"), "web")
	m = send(m, "esc")
	if m.mode != modeList || len(b.saved) != 0 {
		t.Errorf("mode %v, saved %v after esc", m.mode, b.saved)
	}
}

func TestForm_Tab(t *testing.T) {
	t.Parallel()

	m := send(ready(&fakeBackend{}, sample), "A")
	for _, step := range []struct {
		key  string
		want int
	}{
		{"tab", fieldAddress}, {"tab", fieldPort}, {"tab", fieldCompose}, {"tab", fieldName},
		{"shift+tab", fieldCompose}, {"up", fieldPort}, {"down", fieldCompose},
	} {
		m = send(m, step.key)
		if m.form.focus != step.want {
			t.Errorf("after %s the cursor is in field %d, want %d", step.key, m.form.focus, step.want)
		}
	}
}

func TestForm_TypingQDoesNotQuit(t *testing.T) {
	t.Parallel()

	m := send(ready(&fakeBackend{}, sample), "A")
	next, cmd := m.Update(press("q"))
	if cmd != nil && isQuit(cmd()) {
		t.Fatal("q quit while typing in the form")
	}
	if got := next.(Model).form.inputs[fieldName].Value(); got != "q" {
		t.Errorf("name field = %q, want q typed", got)
	}
}

func TestForm_View(t *testing.T) {
	t.Parallel()

	m := typeText(send(ready(&fakeBackend{}, sample), "A"), "test.crm")
	m = typeText(send(send(m, "tab"), "tab"), "3001")
	golden.RequireEqual(t, m.View().Content)
	if h := strings.Count(m.View().Content, "\n") + 1; h != defaultHeight {
		t.Errorf("the form view is %d lines, want the terminal's %d", h, defaultHeight)
	}
}

func TestConfirm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		key      string
		wantGone bool
	}{
		{name: "y deletes", key: "y", wantGone: true},
		{name: "n keeps", key: "n"},
		{name: "esc keeps", key: "esc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{}
			m := send(send(send(send(ready(b, sample), "down"), "down"), "down"), "d") // old.test
			if got := strings.TrimSpace(ansi.Strip(m.statusLine())); got != "delete old.test? y/N" {
				t.Errorf("status line = %q, want the question", got)
			}
			m = send(m, tt.key)
			if m.mode != modeList {
				t.Errorf("mode = %v after answering, want the list", m.mode)
			}
			gone := len(b.saved) == 1 && !slices.ContainsFunc(b.saved[0], func(d store.Domain) bool { return d.Name == "old.test" })
			if gone != tt.wantGone || (!tt.wantGone && len(b.saved) != 0) {
				t.Errorf("saved %v, want deleted = %v", b.saved, tt.wantGone)
			}
		})
	}
}

func TestBackend_PortsReady(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	b := NewBackend(p, run.NewFake())
	if err := b.PortsReady(); err == nil || !strings.Contains(err.Error(), "brew install caddy") {
		t.Errorf("without caddy: %v", err)
	}
	writeTestFile(t, p.Caddy, "")
	if err := b.PortsReady(); err == nil || !strings.Contains(err.Error(), "lodo setup") {
		t.Errorf("caddy not set up: %v", err)
	}
	writeTestFile(t, p.SystemCaddyfile, "# lodo\nimport "+p.Caddyfile+"\n")
	if err := b.PortsReady(); err != nil {
		t.Errorf("caddy set up: %v", err)
	}
}

// fill types into the open form's fields; an empty value keeps the prefill.
func fill(m Model, fields [fieldCount]string) Model {
	for i, v := range fields {
		if v == "" {
			continue
		}
		for m.form.focus != i {
			m = send(m, "tab")
		}
		if i != fieldName {
			m = clearField(m)
		}
		m = typeText(m, v)
	}
	return m
}

func assertAddress(t *testing.T, m Model, address, hint string) {
	t.Helper()
	if got := m.form.inputs[fieldAddress].Value(); got != address {
		t.Errorf("address = %q, want %q", got, address)
	}
	if m.form.hint != hint {
		t.Errorf("hint = %q, want %q", m.form.hint, hint)
	}
}
