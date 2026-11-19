package tui

import (
	"bytes"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"
)

// TestProgram runs the TUI in a real program loop: it draws the list, takes
// a key, applies the change and quits.
func TestProgram(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{onDisk: slices.Clone(sample)}
	tm := teatest.NewTestModel(t, New(t.Context(), b, sample, Start{}), teatest.WithInitialTermSize(80, 24))
	teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
		return bytes.Contains(out, []byte("crm.lodo")) && bytes.Contains(out, []byte("dns ✓"))
	}, teatest.WithDuration(5*time.Second))

	tm.Send(press("space"))
	deadline := time.Now().Add(5 * time.Second)
	for b.appliedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	tm.Send(press("q"))

	final, ok := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(Model)
	if !ok {
		t.Fatal("final model is not a tui.Model")
	}
	if final.domains[0].Enabled {
		t.Error("space did not turn crm.lodo off")
	}
	var _ tea.Model = final
}
