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
		{name: "unknown command", args: []string{"frobnicate"}, wantCode: 2, wantStderr: `oo: unknown command "frobnicate"`},
		{name: "extra argument", args: []string{"doctor", "now"}, wantCode: 2, wantStderr: "oo: doctor takes no arguments"},
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
