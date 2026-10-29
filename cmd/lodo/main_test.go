package main

import (
	"strings"
	"testing"
)

func TestDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "dev\n"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "no command", args: nil, wantCode: 0, wantStdout: "  lodo ps "},
		{name: "unknown command", args: []string{"frobnicate"}, wantCode: 2, wantStderr: `lodo: unknown command "frobnicate"`},
		{name: "extra argument", args: []string{"doctor", "now"}, wantCode: 2, wantStderr: "lodo: doctor takes no arguments"},
		{name: "scan with an unknown flag", args: []string{"scan", "--yaml"}, wantCode: 2, wantStderr: "lodo: scan: flag provided but not defined: -yaml"},
		{name: "scan with an argument", args: []string{"scan", "now"}, wantCode: 2, wantStderr: `lodo: scan: unexpected argument "now"`},
		{name: "ps with an argument", args: []string{"ps", "now"}, wantCode: 2, wantStderr: "lodo: ps takes no arguments"},
		{name: "dev with scan's flag", args: []string{"dev", "--json"}, wantCode: 2, wantStderr: "lodo: dev: flag provided but not defined: -json"},
		{name: "dev with a command", args: []string{"dev", "--", "next", "dev"}, wantCode: 2, wantStderr: `lodo: dev: unexpected argument "next"`},
		{name: "dev --edit -y", args: []string{"dev", "--edit", "-y"}, wantCode: 2, wantStderr: "lodo: dev: --edit opens the form and -y skips it: pick one"},
		{name: "exec without a command", args: []string{"exec"}, wantCode: 2, wantStderr: "lodo: exec needs a command to run"},
		{name: "exec with only --", args: []string{"exec", "--"}, wantCode: 2, wantStderr: "lodo: exec needs a command to run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			code := dispatch(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
