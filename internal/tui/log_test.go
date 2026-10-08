package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/sangdth/lcd/internal/paths"
	"github.com/sangdth/lcd/internal/run"
)

const queries = "Oct  8 18:27:22 dnsmasq[52611]: query[A] crm.lcd from 127.0.0.1\n" +
	"Oct  8 18:27:22 dnsmasq[52611]: config crm.lcd is 127.0.1.1\n"

func TestLog_Open(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{log: queries}
	m := send(ready(b, sample), "l")
	if m.mode != modeLog {
		t.Fatalf("mode = %v after l, want the log", m.mode)
	}
	if !strings.Contains(m.logView(), "config crm.lcd is 127.0.1.1") {
		t.Errorf("log view lacks dnsmasq's lines:\n%s", m.logView())
	}
	golden.RequireEqual(t, m.View().Content)
}

func TestLog_Follows(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{log: queries}
	m := send(ready(b, sample), "l")
	b.appendLog("Oct  8 18:28:00 dnsmasq[52611]: query[A] flowy.lcd from 127.0.0.1\n")
	m = tick(m)
	if !strings.Contains(m.logView(), "query[A] flowy.lcd") {
		t.Errorf("the open log did not pick up the new line:\n%s", m.logView())
	}
	if len(m.logLines) != 3 {
		t.Errorf("log holds %d lines, want 3: %q", len(m.logLines), m.logLines)
	}
}

func TestLog_FollowsTheEnd(t *testing.T) {
	t.Parallel()

	var long strings.Builder
	for i := range 100 {
		fmt.Fprintf(&long, "line %d\n", i)
	}
	b := &fakeBackend{log: long.String()}
	m := send(ready(b, sample), "l")
	if !m.log.AtBottom() {
		t.Error("a long log opens at its start; want its end")
	}
	m = send(send(m, "up"), "up")
	b.appendLog("line 100\n")
	m = tick(m)
	if m.log.AtBottom() {
		t.Error("new lines pulled the view down after the user scrolled up")
	}
}

func TestLog_KeepsTheLastLines(t *testing.T) {
	t.Parallel()

	var long strings.Builder
	for i := range maxLogLines + 10 {
		fmt.Fprintf(&long, "line %d\n", i)
	}
	m := send(ready(&fakeBackend{log: long.String()}, sample), "l")
	if len(m.logLines) != maxLogLines || m.logLines[0] != "line 10" {
		t.Errorf("log holds %d lines from %q, want the last %d", len(m.logLines), m.logLines[0], maxLogLines)
	}
}

func TestLog_NoLogYet(t *testing.T) {
	t.Parallel()

	m := send(ready(&fakeBackend{}, sample), "l")
	if !strings.Contains(ansi.Strip(m.logView()), "no log yet") {
		t.Errorf("an empty log shows:\n%s", m.logView())
	}
	if h := strings.Count(m.View().Content, "\n") + 1; h != defaultHeight {
		t.Errorf("the empty log view is %d lines, want %d", h, defaultHeight)
	}
}

func TestLog_Close(t *testing.T) {
	t.Parallel()

	for _, k := range []string{"l", "esc"} {
		m := send(send(ready(&fakeBackend{log: queries}, sample), "l"), k)
		if m.mode != modeList {
			t.Errorf("%s left mode %v, want the list", k, m.mode)
		}
		if _, cmd := m.Update(logTickMsg{session: m.logSession}); cmd != nil {
			t.Errorf("after %s the log keeps reading", k)
		}
	}
}

func TestLog_StaleTimer(t *testing.T) {
	t.Parallel()

	m := send(send(send(ready(&fakeBackend{log: queries}, sample), "l"), "esc"), "l")
	if _, cmd := m.Update(logTickMsg{session: m.logSession - 1}); cmd != nil {
		t.Error("a timer from the first opening started a second reader")
	}
	if _, cmd := m.Update(logMsg{session: m.logSession - 1, text: "stale\n"}); cmd != nil {
		t.Error("a read from the first opening was kept")
	}
}

func TestLog_ReadError(t *testing.T) {
	t.Parallel()

	m := send(ready(&fakeBackend{tailErr: errors.New("read dnsmasq log: permission denied")}, sample), "l")
	if m.err == nil {
		t.Error("a failed read shows no error")
	}
}

func TestLog_OpensWhileBusy(t *testing.T) {
	t.Parallel()

	m := ready(&fakeBackend{log: queries}, sample)
	next, _ := m.Update(press("space")) // a change starts and keeps running
	next, _ = next.(Model).Update(press("l"))
	if next.(Model).mode != modeLog {
		t.Error("l does nothing while a change runs")
	}
}

func TestBackend_Tail(t *testing.T) {
	t.Parallel()

	p := paths.ForTest(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(p.Log), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Log, []byte(queries), 0o644); err != nil {
		t.Fatal(err)
	}
	text, next, err := NewBackend(p, run.NewFake()).Tail(0)
	if err != nil || text != queries || next != int64(len(queries)) {
		t.Errorf("Tail = %q, %d, %v", text, next, err)
	}
}

// tick fires the open log's timer once and settles the read it starts.
func tick(m Model) Model {
	next, cmd := m.Update(logTickMsg{session: m.logSession})
	return settle(next.(Model), cmd)
}
