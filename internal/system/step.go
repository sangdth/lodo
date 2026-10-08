package system

import (
	"errors"
	"fmt"
	"io"
)

// StepError is a setup or uninstall step that failed, with what to run next.
type StepError struct {
	Step string
	Err  error
	Fix  string
}

func (e *StepError) Error() string { return e.Step + ": " + e.Err.Error() }

func (e *StepError) Unwrap() error { return e.Err }

// steps prints one line per finished step and turns a failure into a
// *StepError. The caller prints the error; steps never prints a failure.
type steps struct {
	out io.Writer
}

// skipped marks a step that does not apply; its text says why.
type skipped string

func (s skipped) Error() string { return string(s) }

func (s steps) do(step, fix string, fn func() (string, error)) error {
	note, err := fn()
	if why, ok := errors.AsType[skipped](err); ok {
		fmt.Fprintf(s.out, "– %s: %s\n", step, why)
		return nil
	}
	if err != nil {
		return &StepError{Step: step, Err: err, Fix: fix}
	}
	if note != "" {
		step += " (" + note + ")"
	}
	fmt.Fprintf(s.out, "✓ %s\n", step)
	return nil
}

func (s steps) note(format string, args ...any) {
	fmt.Fprintf(s.out, "  "+format+"\n", args...)
}
