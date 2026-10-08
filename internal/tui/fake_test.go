package tui

import (
	"context"
	"errors"
	"slices"
	"sync"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/sangdth/lcd/internal/check"
	"github.com/sangdth/lcd/internal/store"
)

// fakeBackend records what the TUI asks of it. Every check passes and every
// enabled name resolves unless failing or failingChecks says otherwise.
type fakeBackend struct {
	mu            sync.Mutex
	onDisk        []store.Domain
	saveErr       error
	applyErr      error
	failing       map[string]string // a name's probe detail when it fails
	failingChecks map[int]string    // a check's detail when it fails
	saved         [][]store.Domain
	applied       [][]store.Domain
	reports       int
}

func (f *fakeBackend) Load() ([]store.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.onDisk), nil
}

func (f *fakeBackend) Save(domains []store.Domain) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, domains)
	f.onDisk = slices.Clone(domains)
	return nil
}

func (f *fakeBackend) Apply(_ context.Context, domains []store.Domain) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, domains)
	return f.applyErr
}

func (f *fakeBackend) Report(_ context.Context, domains []store.Domain) ([]check.Check, []check.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports++
	ports := false
	var results []check.Result
	for _, d := range store.Sort(domains) {
		if !d.Enabled {
			continue
		}
		ports = ports || d.Port > 0
		r := check.Result{Name: d.Name, Address: d.Address, Port: d.Port, Direct: true, System: true, HTTP: d.Port > 0}
		if detail, ok := f.failing[d.Name]; ok {
			r.System, r.HTTP, r.Detail = false, false, detail
		}
		results = append(results, r)
	}
	checks := make([]check.Check, 8)
	for i := range checks {
		id := i + 1
		checks[i] = check.Check{ID: id, OK: true}
		if detail, ok := f.failingChecks[id]; ok {
			checks[i] = check.Check{ID: id, Detail: detail, Fix: "lcd setup"}
		}
	}
	if !ports {
		checks[7] = check.Check{ID: 8, Skipped: true}
	}
	return checks, results
}

func (f *fakeBackend) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}

var errBoom = errors.New("restart dnsmasq: brew services restart dnsmasq: Error: Failure while executing")

// sample is a project with a subdomain on a port, another project and a
// disabled name.
var sample = []store.Domain{
	{Name: "crm.lcd", Address: "127.0.1.1", Enabled: true},
	{Name: "dashboard.crm.lcd", Address: "127.0.1.1", Port: 3000, Enabled: true},
	{Name: "flowy.lcd", Address: "127.0.1.3", Enabled: true},
	{Name: "old.lcd", Address: "127.0.0.1"},
}

// settle runs cmd and every command its messages lead to, then returns the
// model at rest. The spinner's next frame is skipped: it would only wait.
func settle(m Model, cmd tea.Cmd) Model {
	for queue := []tea.Cmd{cmd}; len(queue) > 0; {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case spinner.TickMsg, tea.QuitMsg:
		default:
			next, more := m.Update(msg)
			m = next.(Model)
			queue = append(queue, more)
		}
	}
	return m
}

// ready returns a model for domains after its first report.
func ready(b *fakeBackend, domains []store.Domain) Model {
	m := New(context.Background(), b, domains)
	return settle(m, m.Init())
}

// press returns the message for a key as tea's String names it.
func press(k string) tea.KeyPressMsg {
	switch k {
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	return tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
}

// send presses a key and settles what it started.
func send(m Model, k string) Model {
	next, cmd := m.Update(press(k))
	return settle(next.(Model), cmd)
}
