package compose

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		repo    bool   // the project is a git repository
		compose string // the compose file, from the project's root
		scripts string // package.json's scripts; empty for no package.json
		want    string // from the project's root
	}{
		{name: "no package.json", repo: true, compose: "docker/compose.dev.yml", want: ".env"},
		{
			name: "a script runs the file with --env-file", repo: true, compose: "docker/compose.dev.yml",
			scripts: `{"docker": "docker compose -f docker/compose.dev.yml --env-file .env up -d"}`, want: ".env",
		},
		{
			name: "--env-file= and another file", repo: true, compose: "docker/compose.dev.yml",
			scripts: `{"db": "docker compose --env-file=.env.docker -f ./docker/compose.dev.yml up"}`, want: ".env.docker",
		},
		{
			name: "a quoted env file", repo: true, compose: "compose.yml",
			scripts: `{"up": "docker compose -f compose.yml --env-file 'config/dev.env' up"}`, want: "config/dev.env",
		},
		{
			name: "a script for another compose file", repo: true, compose: "compose.yml",
			scripts: `{"up": "docker compose -f docker-compose.yml --env-file .env.other up"}`, want: ".env",
		},
		{
			name: "a script without --env-file", repo: true, compose: "compose.yml",
			scripts: `{"up": "docker compose -f compose.yml up"}`, want: ".env",
		},
		{name: "outside a repository: the file's folder", compose: "docker/compose.yml", want: "docker/.env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.repo {
				mkdir(t, filepath.Join(root, ".git"))
			}
			path := filepath.Join(root, filepath.FromSlash(tt.compose))
			write(t, path, "services: {}\n")
			if tt.scripts != "" {
				write(t, filepath.Join(root, "package.json"), `{"name": "app", "scripts": `+tt.scripts+`}`)
			}
			if got, want := EnvFile(path), filepath.Join(root, filepath.FromSlash(tt.want)); got != want {
				t.Errorf("EnvFile = %s, want %s", got, want)
			}
		})
	}
}

func TestSetEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{name: "empty file", in: "", want: "DOCKER_HOST_IP=127.0.1.3\n"},
		{name: "appended", in: "DATABASE_URL=postgres://db\n", want: "DATABASE_URL=postgres://db\nDOCKER_HOST_IP=127.0.1.3\n"},
		{name: "appended after a last line without a newline", in: "A=1", want: "A=1\nDOCKER_HOST_IP=127.0.1.3\n"},
		{name: "replaced", in: "A=1\nDOCKER_HOST_IP=127.0.0.1\nB=2\n", want: "A=1\nDOCKER_HOST_IP=127.0.1.3\nB=2\n"},
		{name: "every line that sets it", in: "DOCKER_HOST_IP=1\nDOCKER_HOST_IP=2\n", want: "DOCKER_HOST_IP=127.0.1.3\nDOCKER_HOST_IP=127.0.1.3\n"},
		{name: "export kept", in: "export DOCKER_HOST_IP=127.0.0.1\n", want: "export DOCKER_HOST_IP=127.0.1.3\n"},
		{name: "spaces around = kept", in: "DOCKER_HOST_IP = 127.0.0.1\n", want: "DOCKER_HOST_IP = 127.0.1.3\n"},
		{name: "quoted value, comment kept", in: `DOCKER_HOST_IP="127.0.0.1" # lodo` + "\n", want: "DOCKER_HOST_IP=127.0.1.3 # lodo\n"},
		{name: "unquoted value, comment kept", in: "DOCKER_HOST_IP=127.0.0.1 # lodo\n", want: "DOCKER_HOST_IP=127.0.1.3 # lodo\n"},
		{name: "empty value", in: "DOCKER_HOST_IP=\n", want: "DOCKER_HOST_IP=127.0.1.3\n"},
		{name: "CRLF kept when replaced", in: "A=1\r\nDOCKER_HOST_IP=1\r\n", want: "A=1\r\nDOCKER_HOST_IP=127.0.1.3\r\n"},
		{name: "CRLF used when appended", in: "A=1\r\n", want: "A=1\r\nDOCKER_HOST_IP=127.0.1.3\r\n"},
		{name: "a commented line stays", in: "# DOCKER_HOST_IP=127.0.0.1\n", want: "# DOCKER_HOST_IP=127.0.0.1\nDOCKER_HOST_IP=127.0.1.3\n"},
		{name: "a longer key stays", in: "DOCKER_HOST_IP_V6=::1\n", want: "DOCKER_HOST_IP_V6=::1\nDOCKER_HOST_IP=127.0.1.3\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := string(SetEnv([]byte(tt.in), EnvVar, "127.0.1.3")); got != tt.want {
				t.Errorf("SetEnv(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
