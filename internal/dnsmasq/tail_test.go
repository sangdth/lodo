package dnsmasq_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sangdth/lodo/internal/dnsmasq"
)

func TestTail(t *testing.T) {
	t.Parallel()

	// Each case calls Tail from offset 0 with the file as before, sets it to
	// after, and calls Tail again with the offset the first call returned.
	tests := []struct {
		name       string
		before     *string // nil means no file
		after      *string // nil means no file
		want       string  // what the second call returns
		wantOffset int64
	}{
		{name: "missing file", want: "", wantOffset: 0},
		{name: "file appears", after: ptr("a\n"), want: "a\n", wantOffset: 2},
		{name: "append", before: ptr("a\n"), after: ptr("a\nb\nc"), want: "b\nc", wantOffset: 5},
		{name: "nothing new", before: ptr("a\n"), after: ptr("a\n"), want: "", wantOffset: 2},
		{name: "truncated", before: ptr("a\nb\n"), after: ptr("c\n"), want: "c\n", wantOffset: 2},
		{name: "file removed", before: ptr("a\n"), want: "", wantOffset: 0},
		{
			name:  "gap of exactly MaxTail comes back whole",
			after: ptr(lines(64, 0, 1024)), want: lines(64, 0, 1024), wantOffset: 65536,
		},
		{
			// The window starts 64 bytes into line 344.
			name:   "gap over MaxTail starts at the next line",
			before: ptr("a\n"), after: ptr("a\n" + lines(100, 0, 1000)),
			want: lines(100, 345, 1000), wantOffset: 100002,
		},
		{
			// The window starts exactly at line 76, so no line is dropped.
			name:  "gap over MaxTail on a line boundary keeps the first line",
			after: ptr(lines(64, 0, 1100)), want: lines(64, 76, 1100), wantOffset: 70400,
		},
		{
			name:  "gap over MaxTail with no line break",
			after: ptr(strings.Repeat("x", dnsmasq.MaxTail+10)),
			want:  strings.Repeat("x", dnsmasq.MaxTail), wantOffset: dnsmasq.MaxTail + 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "dnsmasq.log")
			write(t, path, tt.before)
			_, offset, err := dnsmasq.Tail(path, 0)
			if err != nil {
				t.Fatalf("first call: %v", err)
			}
			write(t, path, tt.after)

			got, gotOffset, err := dnsmasq.Tail(path, offset)
			if err != nil {
				t.Fatalf("second call: %v", err)
			}
			if got != tt.want {
				t.Errorf("text = %d bytes from %.24q, want %d bytes from %.24q", len(got), got, len(tt.want), tt.want)
			}
			if gotOffset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", gotOffset, tt.wantOffset)
			}
		})
	}
}

func TestTail_Error(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A path under a regular file fails with "not a directory".
	text, offset, err := dnsmasq.Tail(filepath.Join(file, "dnsmasq.log"), 7)
	if err == nil || text != "" || offset != 7 {
		t.Errorf("Tail = %q, %d, %v; want \"\", 7 and an error", text, offset, err)
	}
}

// lines returns lines from to to-1, each width bytes long with its '\n', and
// starting with its number so a mismatch shows which line it is.
func lines(width, from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		line := fmt.Sprintf("line %d ", i)
		b.WriteString(line + strings.Repeat(".", width-1-len(line)) + "\n")
	}
	return b.String()
}

// write sets the file at path to content, or removes it when content is nil.
func write(t *testing.T, path string, content *string) {
	t.Helper()

	if content == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, []byte(*content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ptr(s string) *string { return &s }
