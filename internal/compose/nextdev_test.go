package compose

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestFixLine(t *testing.T) {
	t.Parallel()

	const addr = "127.0.1.3"
	tests := []struct {
		name, line, want string
	}{
		{name: "a package.json script", line: `    "dev": "next dev",`, want: `    "dev": "next dev -H 127.0.1.3",`},
		{name: "options after it", line: `next dev --turbopack -p 3001`, want: `next dev -H 127.0.1.3 --turbopack -p 3001`},
		{
			name: "a shell line with arguments and a pipe",
			line: `next dev "$@" 2>&1 | tee -i logs/terminal.log`,
			want: `next dev -H 127.0.1.3 "$@" 2>&1 | tee -i logs/terminal.log`,
		},
		{name: "another address", line: `next dev -H 127.0.1.9 -p 3000`, want: `next dev -H 127.0.1.3 -p 3000`},
		{name: "--hostname=", line: `next dev --hostname=0.0.0.0`, want: `next dev --hostname=127.0.1.3`},
		{name: "two commands", line: `next dev -p 3100 && next dev`, want: `next dev -H 127.0.1.3 -p 3100 && next dev -H 127.0.1.3`},
		{name: "the address already", line: `next dev -H 127.0.1.3`, want: `next dev -H 127.0.1.3`},
		{name: "a variable", line: `next dev -H $HOST`, want: `next dev -H $HOST`},
		{name: "a quoted host", line: `next dev -H "${DOCKER_HOST_IP:-0.0.0.0}"`, want: `next dev -H "${DOCKER_HOST_IP:-0.0.0.0}"`},
		{name: "-H of the next command", line: `next dev && curl -H x`, want: `next dev -H 127.0.1.3 && curl -H x`},
		{name: "next build", line: `"build": "next build",`, want: `"build": "next build",`},
		{name: "not next", line: `"dev": "vite dev",`, want: `"dev": "vite dev",`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := fixLine(tt.line, addr); got != tt.want {
				t.Errorf("fixLine(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestNextDev(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	compose := filepath.Join(root, "docker", "compose.dev.yml")
	write(t, compose, "services: {}\n")
	write(t, filepath.Join(root, "package.json"), `{
  "scripts": {
    "dev": "./scripts/dev.sh",
    "booking": "PORT=3100 next dev -p 3100",
    "start": "next start",
    "outside": "sh ../other/dev.sh"
  }
}
`)
	write(t, filepath.Join(root, "scripts", "dev.sh"), "#!/usr/bin/env sh\r\n# runs `next dev` at the end\r\nnext dev \"$@\"\r\n")

	got := NextDev(compose, "127.0.1.5")
	want := []Fix{
		{File: "package.json", Line: 4, Old: `    "booking": "PORT=3100 next dev -p 3100",`, New: `    "booking": "PORT=3100 next dev -H 127.0.1.5 -p 3100",`},
		{File: "scripts/dev.sh", Line: 3, Old: `next dev "$@"`, New: `next dev -H 127.0.1.5 "$@"`},
	}
	if !slices.Equal(got, want) {
		t.Errorf("NextDev =\n%+v\nwant\n%+v", got, want)
	}

	if got := NextDev(filepath.Join(t.TempDir(), "compose.yml"), "127.0.1.5"); got != nil {
		t.Errorf("without a package.json: %+v, want none", got)
	}
}
