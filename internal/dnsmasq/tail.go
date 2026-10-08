package dnsmasq

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// MaxTail is the most Tail returns from one call, so a burst of queries
// cannot flood the log view.
const MaxTail = 64 << 10

// Tail returns what was appended to the file at path since offset, and the
// offset to pass next time: the file's size. A file shorter than offset was
// truncated or replaced, so Tail reads it from the start. A missing file gives
// "", 0 and no error. When more than MaxTail bytes are new, Tail returns only
// the last MaxTail, without the partial line they start with; a window with
// no line break comes back whole. On error the offset comes back unchanged.
func Tail(path string, offset int64) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path comes from paths.Paths
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, nil
	}
	if err != nil {
		return "", offset, fmt.Errorf("read dnsmasq log: %w", err)
	}
	defer func() { _ = f.Close() }() // read only: a close error loses nothing

	info, err := f.Stat()
	if err != nil {
		return "", offset, fmt.Errorf("read dnsmasq log: %w", err)
	}
	size := info.Size()
	start := offset
	if size < start {
		start = 0
	}
	window := size-start > MaxTail
	if window {
		// One byte more than the window, so a '\n' right before it shows
		// that the window already starts on a line.
		start = size - MaxTail - 1
	}
	buf := make([]byte, size-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", offset, fmt.Errorf("read dnsmasq log: %w", err)
	}
	buf = buf[:n]
	if window {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else if len(buf) > 0 {
			buf = buf[1:]
		}
	}
	return string(buf), size, nil
}
