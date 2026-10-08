package run

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Fake records commands instead of running them. A command it has no answer
// for succeeds with no output. It is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	answers map[string]answer
	calls   []string
}

type answer struct {
	out string
	err error
}

// NewFake returns a Fake with no answers.
func NewFake() *Fake {
	return &Fake{answers: map[string]answer{}}
}

// Set makes the command line, as built by Line, succeed with out.
func (f *Fake) Set(line, out string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[line] = answer{out: out}
}

// Fail makes the command line, as built by Line, exit 1 with stderr.
func (f *Fake) Fail(line, stderr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[line] = answer{err: &Error{Cmd: line, Stderr: stderr, Err: errors.New("exit status 1")}}
}

// Calls returns every command line run so far, in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Run implements Runner.
func (f *Fake) Run(_ context.Context, name string, args ...string) (string, error) {
	line := Line(name, args...)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, line)
	a := f.answers[line]
	return a.out, a.err
}

// RunTTY implements Runner.
func (f *Fake) RunTTY(ctx context.Context, name string, args ...string) error {
	_, err := f.Run(ctx, name, args...)
	return err
}
