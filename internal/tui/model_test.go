package tui

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/sangdth/lodo/internal/store"
)

func TestPress(t *testing.T) {
	t.Parallel()

	for _, k := range []string{"space", "down", "up", "enter", "esc", "tab", "shift+tab", "backspace", "ctrl+c", "q", "r"} {
		if got := press(k).String(); got != k {
			t.Errorf("press(%q).String() = %q", k, got)
		}
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := New(t.Context(), b, sample, Start{})
	if !m.busy {
		t.Error("not busy before the first report")
	}
	if got := m.rows()[0][6]; got != "…" {
		t.Errorf("check cell before the first report = %q, want …", got)
	}
	spinning := m.spinner.View() + " crm.test"
	if got := m.table.Rows()[0][0]; got != spinning {
		t.Errorf("enabled row before the first report = %q, want %q", got, spinning)
	}
	if got := m.table.Rows()[3][0]; got != "○ old.test" {
		t.Errorf("disabled row before the first report = %q, want ○ old.test", got)
	}
	if got := strings.TrimSpace(m.statusLine()); got != "" {
		t.Errorf("status line while busy = %q, want empty", got)
	}
	m = settle(m, m.Init())
	if m.busy || b.reports != 1 || len(m.checks) != 8 || len(m.results) != 3 {
		t.Errorf("after Init: busy %v, %d reports, %d checks, %d results", m.busy, b.reports, len(m.checks), len(m.results))
	}
	if got := m.table.Rows()[0][0]; got != "● crm.test" {
		t.Errorf("enabled row after the first report = %q, want ● crm.test", got)
	}
}

func TestModel_Toggle(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := ready(b, sample)
	m = send(m, "down") // dashboard.crm.test
	m = send(m, "space")

	want, err := store.Toggle(sample, "dashboard.crm.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.saved) != 1 || !slices.Equal(b.saved[0], want) {
		t.Errorf("saved %v, want %v", b.saved, want)
	}
	if len(b.applied) != 1 || !slices.Equal(b.applied[0], want) {
		t.Errorf("applied %v, want %v", b.applied, want)
	}
	if m.domains[1].Enabled || m.busy || m.err != nil {
		t.Errorf("after the change: %+v, busy %v, err %v", m.domains[1], m.busy, m.err)
	}
	if got := m.rows()[1][6]; got != "–" {
		t.Errorf("check cell of a disabled name = %q, want –", got)
	}
}

func TestModel_SpinnerOnTheChangedRow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		keys  []string // keys that move to the row; the last key starts the change
		row   int
		label string
	}{
		{name: "turn a name off", keys: []string{"space"}, row: 0, label: "crm.test"},
		{name: "turn a name on", keys: []string{"down", "down", "down", "space"}, row: 3, label: "old.test"},
		{name: "delete a name", keys: []string{"down", "down", "d", "y"}, row: 2, label: "flowy.test"},
		{name: "reload", keys: []string{"r"}, row: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := ready(&fakeBackend{}, sample)
			last := len(tt.keys) - 1
			for _, k := range tt.keys[:last] {
				m = send(m, k)
			}
			next, _ := m.Update(press(tt.keys[last])) // the change has started, not landed
			m = next.(Model)
			spinner := m.spinner.View()
			for i, d := range m.domains {
				row := m.table.Rows()[i]
				spinning := strings.HasPrefix(strings.TrimLeft(row[0], " "), spinner)
				want := i == tt.row || (tt.row < 0 && d.Enabled)
				if spinning != want {
					t.Errorf("row %d %q spins: %v, want %v", i, row[0], spinning, want)
				}
			}
			if tt.row >= 0 && !strings.HasSuffix(m.table.Rows()[tt.row][0], tt.label) {
				t.Errorf("row %d = %q, want %s", tt.row, m.table.Rows()[tt.row][0], tt.label)
			}
		})
	}
}

func TestModel_Services(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := send(ready(b, sample), "tab")
	if !m.onServices || m.table.Focused() {
		t.Fatalf("after tab: onServices %v, table focused %v", m.onServices, m.table.Focused())
	}
	if got := ansi.Strip(m.keys()); got != servicesHelp {
		t.Errorf("keys = %q, want the services' keys", got)
	}
	m = send(m, "left") // already on the first
	m = send(m, "space")
	m = send(m, "right")
	m = send(m, "right") // already on the last
	m = send(m, "space")
	if want := []string{"caddy off", "dnsmasq off"}; !slices.Equal(b.setServices, want) {
		t.Errorf("SetService calls = %q, want %q", b.setServices, want)
	}
	bar := ansi.Strip(m.statusBar())
	if !strings.HasPrefix(bar, " lodo   caddy ○   dnsmasq ○   loopback ●   resolvers ●") {
		t.Errorf("status bar = %q, want both services off", bar)
	}
	if got := strings.TrimSpace(ansi.Strip(m.statusLine())); got != "" {
		t.Errorf("status line = %q; a service turned off is not a problem", got)
	}

	m = send(m, "space") // dnsmasq back on
	if got := b.setServices[len(b.setServices)-1]; got != "dnsmasq on" {
		t.Errorf("last SetService = %q, want dnsmasq on", got)
	}
	m = send(m, "h")
	if m.service != 0 || m.mode != modeList {
		t.Errorf("after h: service %d, mode %v; want the first service, still the list", m.service, m.mode)
	}
	m = send(m, "l")
	if m.service != 1 || m.mode != modeList {
		t.Errorf("after l: service %d, mode %v; want the second service and no log", m.service, m.mode)
	}
	m = send(m, "down") // moves nothing while the keys act on the services
	m = send(m, "tab")
	if m.onServices || !m.table.Focused() {
		t.Fatalf("after the second tab: onServices %v, table focused %v", m.onServices, m.table.Focused())
	}
	if d, _ := m.selected(); d.Name != sample[0].Name {
		t.Errorf("cursor on %q, want it where it was", d.Name)
	}
}

func TestModel_ServiceSpinsWhileItChanges(t *testing.T) {
	t.Parallel()

	m := send(ready(&fakeBackend{}, sample), "tab")
	next, _ := m.Update(press("space")) // the change has started, not landed
	m = next.(Model)
	bar := ansi.Strip(m.statusBar())
	if !strings.Contains(bar, "caddy "+m.spinner.View()) {
		t.Errorf("status bar = %q, want the spinner after caddy, the first service", bar)
	}
	for _, row := range m.table.Rows() {
		if strings.HasPrefix(row[0], m.spinner.View()) {
			t.Errorf("row %q spins; only the service changes", row[0])
		}
	}
}

func TestModel_ServiceFails(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{serviceErr: errors.New("stop caddy: exit status 1")}
	m := send(send(ready(b, sample), "tab"), "space")
	if m.busy || m.err == nil {
		t.Fatalf("busy %v, err %v; want idle with the error", m.busy, m.err)
	}
	if got := ansi.Strip(m.statusBar()); !strings.Contains(got, "caddy ●") {
		t.Errorf("status bar = %q, want caddy still on", got)
	}
}

func TestModel_StatusBarMarks(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{failingChecks: map[int]string{3: "job not loaded"}, off: map[string]bool{"dnsmasq": true}}
	m := ready(b, sample)
	if bar, want := ansi.Strip(m.statusBar()), " lodo   caddy ●   dnsmasq ○   loopback ○   resolvers ●"; bar != want {
		t.Errorf("status bar = %q, want %q", bar, want)
	}
	for _, tt := range []struct {
		label string
		id    int
		want  string
	}{
		{"caddy", 8, m.styles.ok.Render("●")},
		{"dnsmasq", 1, m.styles.dim.Render("○")},
		{"loopback", 3, m.styles.bad.Render("○")},
	} {
		if got := m.state(tt.label, tt.id); got != tt.want {
			t.Errorf("%s mark = %q, want %q", tt.label, got, tt.want)
		}
	}
}

func TestModel_AddRow(t *testing.T) {
	t.Parallel()

	toAddRow := func(m Model) Model {
		for range sample {
			m = send(m, "down")
		}
		return m
	}
	m := ready(&fakeBackend{}, sample)
	last := func(m Model) string { return m.table.Rows()[len(sample)][0] }
	if got := last(m); got != m.styles.dim.Render(addRowText) {
		t.Errorf("add row away from the cursor = %q, want it dim", got)
	}
	m = toAddRow(m)
	if !m.onAddRow() || last(m) != addRowText {
		t.Fatalf("on the add row %v, row %q; want the cursor there and the row plain, so it takes the highlight", m.onAddRow(), last(m))
	}
	if _, ok := m.selected(); ok {
		t.Error("the add row selects a name")
	}
	if got := m.keys(); got != addRowHelp {
		t.Errorf("keys = %q, want the add row's", got)
	}
	for _, k := range []string{"e", "d", "l", "s"} {
		if got := send(m, k); got.mode != modeList || got.note != "" {
			t.Errorf("%s on the add row: mode %v, note %q; want nothing", k, got.mode, got.note)
		}
	}
	for _, k := range []string{"space", "enter", "a"} {
		got := send(m, k)
		if got.mode != modeForm || got.form.parent != "" || got.form.suffix != ".test" {
			t.Errorf("%s on the add row: mode %v, parent %q; want the plain add form", k, got.mode, got.form.parent)
		}
	}
}

func TestModel_AddKeys(t *testing.T) {
	t.Parallel()

	for _, k := range []tea.KeyPressMsg{
		{Code: 'A', Text: "A"},
		{Code: 'a', ShiftedCode: 'A', Text: "A", Mod: tea.ModShift},
		{Code: 'a', Mod: tea.ModShift}, // a terminal that reports keys without their text
	} {
		next, _ := ready(&fakeBackend{}, sample).Update(k) // the cursor is on crm.test
		if m := next.(Model); m.mode != modeForm || m.form.parent != "" {
			t.Errorf("%s: mode %v, parent %q; want the plain add form", k, m.mode, m.form.parent)
		}
	}
	m := send(ready(&fakeBackend{}, sample), "a")
	if m.mode != modeForm || m.form.parent != "crm.test" {
		t.Errorf("a on crm.test: mode %v, parent %q; want a subdomain of crm.test", m.mode, m.form.parent)
	}
}

func TestModel_RowsIndentTheMark(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{}, sample)
	for i, want := range []string{"● crm.test", "  ● dashboard.crm.test", "● flowy.test", "○ old.test"} {
		if got := m.table.Rows()[i][0]; got != want {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
	}
}

// lastProject ends with a project and its subdomain.
var lastProject = []store.Domain{
	{Name: "aaa.test", Address: "127.0.1.2", Enabled: true},
	{Name: "crm.test", Address: "127.0.1.1", Enabled: true},
	{Name: "dashboard.crm.test", Address: "127.0.1.1", Port: 3000, Enabled: true},
}

func TestDelete_TakesItsSubdomains(t *testing.T) {
	t.Parallel()

	many := append(slices.Clone(sample), store.Domain{Name: "api.crm.test", Address: "127.0.1.1", Enabled: true})
	tests := []struct {
		name     string
		domains  []store.Domain
		downs    int
		wantAsk  string
		wantLeft []string
	}{
		{name: "one subdomain, named", domains: sample, wantAsk: "delete crm.test and dashboard.crm.test? y/N", wantLeft: []string{"flowy.test", "old.test"}},
		{name: "several, counted", domains: many, wantAsk: "delete crm.test and its 2 subdomains? y/N", wantLeft: []string{"flowy.test", "old.test"}},
		{name: "a subdomain alone", domains: sample, downs: 1, wantAsk: "delete dashboard.crm.test? y/N", wantLeft: []string{"crm.test", "flowy.test", "old.test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &fakeBackend{}
			m := ready(b, tt.domains)
			for range tt.downs {
				m = send(m, "down")
			}
			m = send(m, "d")
			if got := strings.TrimSpace(ansi.Strip(m.statusLine())); got != tt.wantAsk {
				t.Errorf("question = %q, want %q", got, tt.wantAsk)
			}
			send(m, "y")
			if len(b.saved) != 1 {
				t.Fatalf("saved %d lists, want 1", len(b.saved))
			}
			var left []string
			for _, d := range b.saved[0] {
				left = append(left, d.Name)
			}
			if !slices.Equal(left, tt.wantLeft) {
				t.Errorf("left %q, want %q", left, tt.wantLeft)
			}
		})
	}
}

func TestModel_DeleteMovesToANeighbor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		domains []store.Domain
		downs   int
		want    string // the name under the cursor after the delete; empty for the add row
	}{
		{name: "a middle name", domains: sample, downs: 1, want: "flowy.test"},
		{name: "the last name", domains: sample, downs: 3, want: "flowy.test"},
		{name: "the only name", domains: sample[2:3], want: ""},
		{name: "a project with its subdomain: past them", domains: sample, downs: 0, want: "flowy.test"},
		{name: "the last project with its subdomain: back up", domains: lastProject, downs: 1, want: "aaa.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := ready(&fakeBackend{}, tt.domains)
			for range tt.downs {
				m = send(m, "down")
			}
			m = send(send(m, "d"), "y")
			got := ""
			if d, ok := m.selected(); ok {
				got = d.Name
			}
			if got != tt.want {
				t.Errorf("cursor on %q (add row %v), want %q", got, m.onAddRow(), tt.want)
			}
		})
	}
}

func TestModel_CursorMovedDuringAChangeStays(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		start func(Model) (tea.Model, tea.Cmd) // starts the change and returns before it lands
		moves []string
		want  string // the name under the cursor once the change lands
	}{
		{
			name:  "toggle, then move down",
			start: func(m Model) (tea.Model, tea.Cmd) { return m.Update(press("space")) },
			moves: []string{"down"},
			want:  "dashboard.crm.test",
		},
		{
			name:  "toggle, no move",
			start: func(m Model) (tea.Model, tea.Cmd) { return m.Update(press("space")) },
			want:  "crm.test",
		},
		{
			name: "add above the row moved to",
			start: func(m Model) (tea.Model, tea.Cmd) {
				return typeText(send(m, "a"), "api").Update(press("enter")) // api.crm.test is listed right under crm.test
			},
			moves: []string{"down", "down", "down"},
			want:  "flowy.test",
		},
		{
			name: "move onto the name being added",
			start: func(m Model) (tea.Model, tea.Cmd) {
				return typeText(send(m, "a"), "api").Update(press("enter"))
			},
			moves: []string{"down", "down", "up"},
			want:  "api.crm.test",
		},
		{
			name: "add, no move",
			start: func(m Model) (tea.Model, tea.Cmd) {
				return typeText(send(m, "a"), "api").Update(press("enter"))
			},
			want: "api.crm.test",
		},
		{
			name:  "move to the add row",
			start: func(m Model) (tea.Model, tea.Cmd) { return m.Update(press("space")) },
			moves: []string{"down", "down", "down", "down"},
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			next, cmd := tt.start(ready(&fakeBackend{}, sample))
			m := next.(Model)
			if !m.busy {
				t.Fatal("the change landed before the moves")
			}
			for _, k := range tt.moves {
				m = send(m, k)
			}
			m = settle(m, cmd)
			if m.busy {
				t.Fatal("the change has not landed")
			}
			if got := m.cursorName(); got != tt.want {
				t.Errorf("cursor on %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModel_AddingShowsTheRowAtOnce(t *testing.T) {
	t.Parallel()

	m := send(ready(&fakeBackend{}, sample), "a") // a subdomain of crm.test
	next, cmd := typeText(m, "api").Update(press("enter"))
	m = next.(Model)
	rows := m.table.Rows()
	if len(rows) != len(sample)+2 || rows[1][0] != "  "+m.spinner.View()+" api.crm.test" {
		t.Fatalf("rows while saving = %q, want api.crm.test under crm.test with the spinner", rows)
	}
	if got := m.table.Height(); got != len(sample)+2 {
		t.Errorf("table shows %d rows, want room for the new one", got)
	}
	m = settle(m, cmd)
	if m.adding.Name != "" || len(m.table.Rows()) != len(sample)+2 || m.table.Rows()[1][0] != "  ● api.crm.test" {
		t.Errorf("rows after saving = %q, adding %q; want the saved row once", m.table.Rows(), m.adding.Name)
	}
}

func TestModel_AddingRowGoesWhenTheSaveFails(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{saveErr: errors.New("write domains.json: permission denied")}
	m := typeText(send(ready(b, sample), "A"), "web")
	m = send(m, "enter")
	if m.err == nil || m.adding.Name != "" || len(m.table.Rows()) != len(sample)+1 {
		t.Errorf("err %v, adding %q, %d rows; want the error and the row gone", m.err, m.adding.Name, len(m.table.Rows()))
	}
	if got := m.table.Height(); got != len(sample)+1 {
		t.Errorf("table shows %d rows after the failed save, want %d", got, len(sample)+1)
	}
}

func TestModel_ApplyFails(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{applyErr: errBoom}
	m := send(ready(b, sample), "space")
	if m.err == nil || m.busy {
		t.Fatalf("err %v, busy %v; want the apply error and idle", m.err, m.busy)
	}
	if m.domains[0].Enabled {
		t.Error("the saved toggle was dropped; it must stay so r can apply it again")
	}
}

func TestModel_SaveFails(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{saveErr: errBoom}
	m := send(ready(b, sample), "space")
	if m.err == nil {
		t.Fatal("err = nil, want the save error")
	}
	if !m.domains[0].Enabled || len(b.applied) != 0 {
		t.Errorf("list changed or applied after a failed save: %+v, %d applies", m.domains[0], len(b.applied))
	}
}

func TestModel_Reload(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := ready(b, sample)
	b.onDisk = []store.Domain{{Name: "new.test", Address: "127.0.1.9", Enabled: true}}
	m = send(m, "r")
	if len(b.applied) != 1 || b.applied[0][0].Name != "new.test" {
		t.Errorf("applied %v, want the list on disk", b.applied)
	}
	if len(m.domains) != 1 || m.domains[0].Name != "new.test" {
		t.Errorf("domains = %v, want the list on disk", m.domains)
	}
}

func TestModel_KeysWhileBusy(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := ready(b, sample)
	next, _ := m.Update(press("space")) // starts a change and leaves it running
	m = next.(Model)

	tests := []struct {
		name     string
		key      string
		wantQuit bool
	}{
		{name: "space does nothing", key: "space"},
		{name: "r does nothing", key: "r"},
		{name: "q does not quit", key: "q"},
		{name: "ctrl+c quits", key: "ctrl+c", wantQuit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, cmd := m.Update(press(tt.key))
			if quit := cmd != nil && isQuit(cmd()); quit != tt.wantQuit {
				t.Errorf("quit = %v, want %v", quit, tt.wantQuit)
			}
		})
	}
	if next, _ := m.Update(press("down")); next.(Model).table.Cursor() != 1 {
		t.Error("the cursor does not move while busy")
	}
	if len(b.saved) != 0 {
		t.Errorf("a key started a second change: %v", b.saved)
	}
}

func TestModel_Quit(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{}, sample)
	_, cmd := m.Update(press("q"))
	if cmd == nil || !isQuit(cmd()) {
		t.Error("q does not quit when idle")
	}
}

func TestModel_WindowSize(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{}, sample)
	longest := ansi.StringWidth("  ● dashboard.crm.test")
	fixed := addressWidth + portWidth + httpsWidth + ownWidth + checkWidth + 2*columns
	tests := []struct {
		terminal, box, name int
	}{
		{terminal: 200, box: 160, name: longest}, // 80%
		{terminal: 120, box: 96, name: longest},
		{terminal: 100, box: minBoxWidth, name: longest},                // the floor still fits a 22-character name
		{terminal: 80, box: 80, name: 80 - 2 - fixed - minComposeWidth}, // narrower than the floor: the whole terminal
	}
	for _, tt := range tests {
		next, _ := m.Update(tea.WindowSizeMsg{Width: tt.terminal, Height: 40})
		cols := next.(Model).table.Columns()
		if cols[0].Width != tt.name {
			t.Errorf("name column = %d wide at %d columns, want %d", cols[0].Width, tt.terminal, tt.name)
		}
		if want := tt.box - 2 - tt.name - fixed; cols[4].Width != want {
			t.Errorf("compose column = %d wide at %d columns, want %d, the rest", cols[4].Width, tt.terminal, want)
		}
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 2})
	if got := next.(Model).table.Columns()[0].Width; got != minNameWidth {
		t.Errorf("name column = %d wide in a tiny terminal, want %d", got, minNameWidth)
	}
}

func TestModel_BoxFitsTheNames(t *testing.T) {
	t.Parallel()

	many := make([]store.Domain, 40)
	for i := range many {
		many[i] = store.Domain{Name: "n" + strconv.Itoa(i) + ".test", Address: "127.0.0.1"}
	}
	tests := []struct {
		name    string
		domains []store.Domain
		want    int // rows the table shows, without its header: the names and the add row
	}{
		{name: "no names", domains: nil, want: 1},
		{name: "a few names", domains: sample, want: len(sample) + 1},
		{name: "more names than fit", domains: many, want: defaultHeight*boxHeightPercent/100 - 2 - 3 - keyLines - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := ready(&fakeBackend{}, tt.domains)
			if got := m.table.Height(); got != tt.want {
				t.Errorf("table height = %d, want %d", got, tt.want)
			}
		})
	}
}

func isQuit(msg tea.Msg) bool {
	_, ok := msg.(tea.QuitMsg)
	return ok
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestModel_HTTPSKey(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := send(ready(b, sample), "down") // dashboard.crm.test, on port 3000
	next, _ := m.Update(press("s"))     // the change has started, not landed
	started := next.(Model)
	if !started.busy || !strings.HasPrefix(strings.TrimLeft(started.table.Rows()[1][0], " "), started.spinner.View()) {
		t.Errorf("after s: busy %v, row %q; want the spinner on dashboard.crm.test", started.busy, started.table.Rows()[1][0])
	}

	m = send(m, "s")
	want := slices.Clone(sample)
	want[1].HTTPS = true
	if len(b.saved) != 1 || !slices.Equal(b.saved[0], want) {
		t.Errorf("saved %v, want %v", b.saved, want)
	}
	if len(b.applied) != 1 || !slices.Equal(b.applied[0], want) {
		t.Errorf("applied %v, want %v", b.applied, want)
	}
	if !m.domains[1].HTTPS || m.busy || m.err != nil {
		t.Errorf("after the change: %+v, busy %v, err %v", m.domains[1], m.busy, m.err)
	}
	if got := m.rows()[1][3]; got != "✓" {
		t.Errorf("https cell = %q, want ✓", got)
	}
	if got := m.rows()[1][6]; got != "dns ✓  http ✓  https ✓" {
		t.Errorf("check cell = %q, want the https probe after http", got)
	}
	if got := m.table.Columns()[6].Width; got != secureWidth {
		t.Errorf("check column = %d wide, want %d while a name has https on", got, secureWidth)
	}

	m = send(m, "s")
	if m.domains[1].HTTPS || len(b.applied) != 2 || m.rows()[1][3] != "" || m.rows()[1][6] != "dns ✓  http ✓" {
		t.Errorf("after s again: %+v, applied %d, row %q; want https off", m.domains[1], len(b.applied), m.rows()[1])
	}
}

func TestModel_HTTPSKeyNeedsAPort(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{}
	m := send(ready(b, sample), "s") // crm.test has no port
	if m.busy || len(b.saved) != 0 || m.domains[0].HTTPS {
		t.Errorf("s without a port: busy %v, saved %d, %+v; want nothing changed", m.busy, len(b.saved), m.domains[0])
	}
	if got := strings.TrimSpace(ansi.Strip(m.statusLine())); got != "✗ https needs a port: Caddy serves only names with one" {
		t.Errorf("status line = %q, want the reason", got)
	}
}

func TestModel_HTTPSCheckCell(t *testing.T) {
	t.Parallel()

	domains := slices.Clone(sample)
	domains[1].HTTPS = true
	tests := []struct {
		name    string
		backend *fakeBackend
		want    string
	}{
		{name: "passes", backend: &fakeBackend{}, want: "dns ✓  http ✓  https ✓"},
		{name: "fails", backend: &fakeBackend{failing: map[string]string{"dashboard.crm.test": "https: bad certificate"}}, want: "dns ✗  http ✗  https ✗"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := ready(tt.backend, domains)
			if got := m.rows()[1][6]; got != tt.want {
				t.Errorf("check cell = %q, want %q", got, tt.want)
			}
			if got := m.rows()[0][6]; got != "dns ✓" {
				t.Errorf("check cell without https = %q, want dns only", got)
			}
		})
	}
}
