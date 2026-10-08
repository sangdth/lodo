package compose_test

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"
	"go.yaml.in/yaml/v3"

	"github.com/sangdth/oo/internal/compose"
)

// flowy is what every test rewrites for: the project flowy.oo, with Caddy
// serving dashboard.flowy.oo on port 3000.
var flowy = compose.Values{Domain: "flowy.oo", Names: map[int]string{3000: "dashboard.flowy.oo"}}

// yml joins lines into a file.
func yml(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// rewrite runs Rewrite on src for flowy and checks what holds for any file.
func rewrite(t *testing.T, src string) (string, []compose.Change) {
	t.Helper()
	out, changes, err := compose.Rewrite([]byte(src), flowy)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	checkRewrite(t, []byte(src), out, changes)
	return string(out), changes
}

// checkRewrite checks what holds for any rewrite: as many lines as before, a
// change for every line that differs, a file that still parses, and nothing
// left to change in it.
func checkRewrite(t *testing.T, src, out []byte, changes []compose.Change) {
	t.Helper()
	before, after := strings.Split(string(src), "\n"), strings.Split(string(out), "\n")
	if len(after) != len(before) {
		t.Fatalf("Rewrite turned %d lines into %d:\n%s", len(before), len(after), out)
	}
	changed := make(map[int]bool, len(changes))
	for _, c := range changes {
		changed[c.Line] = true
	}
	for i := range before {
		if before[i] != after[i] && !changed[i+1] {
			t.Errorf("line %d changed without a Change:\n-%s\n+%s", i+1, before[i], after[i])
		}
	}
	if err := yaml.Unmarshal(out, &yaml.Node{}); err != nil {
		t.Errorf("the rewritten file does not parse: %v\n%s", err, out)
	}
	again, more, err := compose.Rewrite(out, flowy)
	if err != nil || len(more) > 0 || !bytes.Equal(again, out) {
		t.Errorf("a second Rewrite changed %+v, err %v", more, err)
	}
}

// checkChanges compares the changes Rewrite made with the ones expected.
func checkChanges(t *testing.T, got, want []compose.Change) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("changes:\n%swant:\n%s", list(got), list(want))
	}
}

// list formats changes one per line.
func list(changes []compose.Change) string {
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "  %+v\n", c)
	}
	return b.String()
}

// TestRewrite rewrites a file built from the shapes in the compose files in
// ~/Projects.
func TestRewrite(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("testdata/compose.dev.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, changes := rewrite(t, string(src))
	golden.RequireEqual(t, out)

	const ip = compose.HostIP
	checkChanges(t, changes, []compose.Change{
		{Line: 16, Service: "postgres", Kind: compose.KindPort, Old: "5432:5432", New: ip + ":5432:5432"},
		{Line: 26, Service: "postgres-test", Kind: compose.KindPort, Old: "5432:5432", New: ip + ":5432:5432"},
		{Line: 32, Service: "postgres-dev", Kind: compose.KindPort, Old: "${POSTGRES_PORT:-5432}:5432", New: ip + ":${POSTGRES_PORT:-5432}:5432"},
		{Line: 43, Service: "redis", Kind: compose.KindPort, Old: "127.0.0.1:6379:6379", New: ip + ":6379:6379"},
		{Line: 49, Service: "minio", Kind: compose.KindPort, Old: "9000:9000", New: ip + ":9000:9000"},
		{Line: 50, Service: "minio", Kind: compose.KindPort, Old: "9001:9001", New: ip + ":9001:9001"},
		{Line: 61, Service: "app", Kind: compose.KindPort, Old: "127.0.0.1", New: ip},
		{Line: 63, Service: "app", Kind: compose.KindURL, Old: "${BETTER_AUTH_URL:-http://localhost:3000}", New: "${BETTER_AUTH_URL:-http://dashboard.flowy.oo}"},
		{Line: 64, Service: "app", Kind: compose.KindURL, Old: "http://localhost:3000", New: "http://dashboard.flowy.oo"},
		{Line: 65, Service: "app", Kind: compose.KindURL, Old: "https://localhost:8443/api", New: "https://flowy.oo:8443/api"},
		{Line: 74, Service: "worker", Kind: compose.KindURL, Old: "OLLAMA_ENDPOINT=http://localhost:11434", New: "OLLAMA_ENDPOINT=http://flowy.oo:11434"},
		{Line: 76, Service: "worker", Kind: compose.KindURL, Old: "EVENTS_URL=ws://localhost:3000/events", New: "EVENTS_URL=ws://dashboard.flowy.oo/events"},
		{Line: 83, Service: "proxy", Kind: compose.KindPort, Old: "8443:443/tcp", New: ip + ":8443:443/tcp"},
	})
}

func TestRewrite_Ports(t *testing.T) {
	t.Parallel()

	const ip = compose.HostIP
	tests := []struct {
		name  string
		token string // as written in the ports list
		want  string // the value it becomes; empty means it stays
	}{
		{name: "host and container port", token: `"5432:5432"`, want: ip + ":5432:5432"},
		{name: "plain", token: `5432:5432`, want: ip + ":5432:5432"},
		{name: "single-quoted", token: `'5432:5432'`, want: ip + ":5432:5432"},
		{name: "protocol", token: `8080:80/udp`, want: ip + ":8080:80/udp"},
		{name: "port range", token: `"8000-8010:8000-8010"`, want: ip + ":8000-8010:8000-8010"},
		{name: "host port from a variable", token: `"${POSTGRES_PORT:-5432}:5432"`, want: ip + ":${POSTGRES_PORT:-5432}:5432"},
		{name: "loopback address", token: `"127.0.0.1:6379:6379"`, want: ip + ":6379:6379"},
		{name: "localhost address", token: `localhost:6379:6379/tcp`, want: ip + ":6379:6379/tcp"},
		{name: "loopback with a random host port", token: `"127.0.0.1::80"`, want: ip + "::80"},
		{name: "container port only", token: `"3000"`},
		{name: "container range only", token: `"3000-3005"`},
		{name: "number", token: `3000`},
		{name: "every address", token: `"0.0.0.0:8080:80"`},
		{name: "lan address", token: `"192.168.1.5:8080:80"`},
		{name: "ipv6 loopback", token: `"[::1]:8080:80"`},
		{name: "already bound", token: `"${DOCKER_HOST_IP:-127.0.0.1}:5433:5432"`},
		{name: "address from a variable", token: `"${BIND_ADDRESS}:8080:80"`},
		{name: "four parts", token: `"127.0.0.1:1:2:3"`},
		{name: "escape", token: `"\x35432:5432"`},
		{name: "anchor", token: `&db "5432:5432"`},
		{name: "tag", token: `!!str 5432:5432`},
		{name: "quote inside a plain token", token: `5432:5432"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := yml("services:", "  db:", "    image: postgres:17", "    ports:", "      - "+tt.token+" # localhost:5432")
			out, changes := rewrite(t, src)
			want, wantChanges := src, []compose.Change(nil)
			if tt.want != "" {
				want = strings.Replace(src, tt.token, `"`+tt.want+`"`, 1)
				wantChanges = []compose.Change{
					{Line: 5, Service: "db", Kind: compose.KindPort, Old: strings.Trim(tt.token, `"'`), New: tt.want},
				}
			}
			if out != want {
				t.Errorf("Rewrite =\n%s\nwant\n%s", out, want)
			}
			checkChanges(t, changes, wantChanges)
		})
	}
}

func TestRewrite_URLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string // empty means it stays
	}{
		{name: "http to the name caddy serves", value: "http://localhost:3000", want: "http://dashboard.flowy.oo"},
		{name: "path and query stay", value: "http://localhost:3000/api/auth?next=/", want: "http://dashboard.flowy.oo/api/auth?next=/"},
		{name: "http on a port without a name", value: "http://localhost:8080", want: "http://flowy.oo:8080"},
		{name: "http without a port", value: "http://localhost", want: "http://flowy.oo"},
		{name: "loopback address", value: "http://127.0.0.1:3000/health", want: "http://dashboard.flowy.oo/health"},
		{name: "https keeps its port", value: "https://localhost:3000", want: "https://flowy.oo:3000"},
		{name: "ws to the name caddy serves", value: "ws://localhost:3000/socket", want: "ws://dashboard.flowy.oo/socket"},
		{name: "wss keeps its port", value: "wss://127.0.0.1:3000", want: "wss://flowy.oo:3000"},
		{name: "scheme keeps its case", value: "HTTP://LocalHost:3000", want: "HTTP://dashboard.flowy.oo"},
		{name: "default of a variable", value: "${BETTER_AUTH_URL:-http://localhost:3000}", want: "${BETTER_AUTH_URL:-http://dashboard.flowy.oo}"},
		{name: "port from a variable", value: "http://localhost:${PORT}", want: "http://flowy.oo:${PORT}"},
		{name: "two urls", value: "http://localhost:3000,https://127.0.0.1:8443", want: "http://dashboard.flowy.oo,https://flowy.oo:8443"},
		{name: "host alone", value: "localhost"},
		{name: "host and port", value: "localhost:9222"},
		{name: "address alone", value: "127.0.0.1"},
		{name: "other scheme", value: "postgres://postgres:postgres@localhost:5432/flowy"},
		{name: "longer host", value: "http://localhost.example.com"},
		{name: "address in a longer host", value: "http://127.0.0.1.nip.io:3000"},
		{name: "port too long", value: "http://localhost:123456"},
		{name: "user info", value: "http://localhost@example.com"},
		{name: "scheme inside a word", value: "xhttp://localhost:3000"},
	}
	forms := []struct {
		name   string
		lead   string // the line up to the token
		prefix string // the token up to the value
	}{
		{name: "map", lead: "      APP_URL: "},
		{name: "list", lead: "      - ", prefix: "APP_URL="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, form := range forms {
				t.Run(form.name, func(t *testing.T) {
					t.Parallel()

					src := yml("services:", "  app:", "    environment:", form.lead+form.prefix+tt.value)
					out, changes := rewrite(t, src)
					want, wantChanges := src, []compose.Change(nil)
					if tt.want != "" {
						want = yml("services:", "  app:", "    environment:", form.lead+form.prefix+tt.want)
						wantChanges = []compose.Change{
							{Line: 4, Service: "app", Kind: compose.KindURL, Old: form.prefix + tt.value, New: form.prefix + tt.want},
						}
					}
					if out != want {
						t.Errorf("Rewrite =\n%s\nwant\n%s", out, want)
					}
					checkChanges(t, changes, wantChanges)
				})
			}
		})
	}
}

// TestRewrite_Tokens checks that a URL edit keeps its token's style, and that
// a token Rewrite can't place exactly stays as it is.
func TestRewrite_Tokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string // empty means it stays
	}{
		{
			name: "plain, with a comment after",
			in:   yml("services:", "  app:", "    environment:", "      APP_URL: http://localhost:3000 # the app"),
			want: yml("services:", "  app:", "    environment:", "      APP_URL: http://dashboard.flowy.oo # the app"),
		},
		{
			name: "double quotes stay",
			in:   yml("services:", "  app:", "    environment:", `      APP_URL: "http://localhost:3000"`),
			want: yml("services:", "  app:", "    environment:", `      APP_URL: "http://dashboard.flowy.oo"`),
		},
		{
			name: "single quotes stay",
			in:   yml("services:", "  app:", "    environment:", `      APP_URL: 'http://localhost:3000'`),
			want: yml("services:", "  app:", "    environment:", `      APP_URL: 'http://dashboard.flowy.oo'`),
		},
		{
			name: "quoted list item",
			in:   yml("services:", "  app:", "    environment:", `      - "APP_URL=http://localhost:8080"`),
			want: yml("services:", "  app:", "    environment:", `      - "APP_URL=http://flowy.oo:8080"`),
		},
		{
			name: "wide characters before the token",
			in:   yml("services:", "  app:", `    environment: {NAME: "café ☕", APP_URL: http://localhost:3000}`),
			want: yml("services:", "  app:", `    environment: {NAME: "café ☕", APP_URL: http://dashboard.flowy.oo}`),
		},
		{
			name: "crlf line endings",
			in:   "services:\r\n  db:\r\n    ports:\r\n      - \"5432:5432\"\r\n    environment:\r\n      APP_URL: http://localhost:3000\r\n",
			want: "services:\r\n  db:\r\n    ports:\r\n      - \"" + compose.HostIP + ":5432:5432\"\r\n    environment:\r\n      APP_URL: http://dashboard.flowy.oo\r\n",
		},
		{
			name: "byte order mark",
			in:   "\uFEFFservices: {db: {ports: [\"5432:5432\"]}}\n",
			want: "\uFEFFservices: {db: {ports: [\"" + compose.HostIP + ":5432:5432\"]}}\n",
		},
		{
			// The parser puts the port a line lower, where the comment repeats
			// it at the same column: an edit there would change the comment.
			name: "lone cr in a comment",
			in:   yml("# shapes from ~/Projects\r\r", "services:", "  db:", "    ports:", `      - "5432:5432"`, `      # "5432:5432" is the default`),
		},
		{
			name: "next line in a comment",
			in:   yml("# shapes from ~/Projects\u0085", "services:", "  db:", "    ports:", `      - "5432:5432"`, `      # "5432:5432" is the default`),
		},
		{
			name: "line separator in a comment",
			in:   yml("# shapes from ~/Projects\u2028", "services:", "  db:", "    ports:", `      - "5432:5432"`, `      # "5432:5432" is the default`),
		},
		{
			name: "escape",
			in:   yml("services:", "  app:", "    environment:", `      APP_URL: "http://localhost:3000\t"`),
		},
		{
			name: "escape in the host",
			in:   yml("services:", "  app:", "    environment:", `      APP_URL: "http://local\x68ost:3000"`),
		},
		{
			name: "doubled single quote",
			in:   yml("services:", "  app:", "    environment:", `      APP_URL: 'it''s http://localhost:3000'`),
		},
		{
			name: "literal block",
			in:   yml("services:", "  app:", "    environment:", "      APP_URL: |", "        http://localhost:3000"),
		},
		{
			name: "folded block",
			in:   yml("services:", "  app:", "    environment:", "      APP_URL: >-", "        http://localhost:3000"),
		},
		{
			name: "plain over two lines",
			in:   yml("services:", "  app:", "    environment:", "      APP_URL: http://localhost:3000", "        /api"),
		},
		{
			name: "double quotes over two lines",
			in:   yml("services:", "  app:", "    environment:", `      APP_URL: "http://localhost:3000`, `        /api"`),
		},
		{
			name: "anchor",
			in:   yml("services:", "  app:", "    environment:", "      APP_URL: &url http://localhost:3000"),
		},
		{
			name: "tag",
			in:   yml("services:", "  app:", "    environment:", "      APP_URL: !!str http://localhost:3000"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			want := cmp.Or(tt.want, tt.in)
			if out, _ := rewrite(t, tt.in); out != want {
				t.Errorf("Rewrite =\n%s\nwant\n%s", out, want)
			}
		})
	}
}

// TestRewrite_Structure checks which parts of a file Rewrite visits: each
// service's ports and environment, and nothing else.
func TestRewrite_Structure(t *testing.T) {
	t.Parallel()

	const ip = compose.HostIP
	tests := []struct {
		name    string
		in      string
		want    string // empty means it stays
		changes []compose.Change
	}{
		{
			name: "long syntax with a loopback host_ip",
			in:   yml("services:", "  web:", "    ports:", "      - target: 80", "        published: 8080", "        host_ip: 127.0.0.1"),
			want: yml("services:", "  web:", "    ports:", "      - target: 80", "        published: 8080", `        host_ip: "`+ip+`"`),
			changes: []compose.Change{
				{Line: 6, Service: "web", Kind: compose.KindPort, Old: "127.0.0.1", New: ip},
			},
		},
		{
			name: "long syntax with localhost in quotes",
			in:   yml("services:", "  web:", "    ports:", "      - {target: 80, host_ip: 'localhost'}"),
			want: yml("services:", "  web:", "    ports:", `      - {target: 80, host_ip: "`+ip+`"}`),
			changes: []compose.Change{
				{Line: 4, Service: "web", Kind: compose.KindPort, Old: "localhost", New: ip},
			},
		},
		{
			name: "long syntax with another address",
			in:   yml("services:", "  web:", "    ports:", "      - target: 80", "        published: 8080", "        host_ip: 0.0.0.0"),
		},
		{
			name: "long syntax without host_ip",
			in:   yml("services:", "  web:", "    ports:", "      - target: 80", "        published: 8080"),
		},
		{
			name: "flow list",
			in:   yml("services:", "  db:", `    ports: ["5432:5432", 6379:6379, "3000"]`),
			want: yml("services:", "  db:", `    ports: ["`+ip+`:5432:5432", "`+ip+`:6379:6379", "3000"]`),
			changes: []compose.Change{
				{Line: 3, Service: "db", Kind: compose.KindPort, Old: "5432:5432", New: ip + ":5432:5432"},
				{Line: 3, Service: "db", Kind: compose.KindPort, Old: "6379:6379", New: ip + ":6379:6379"},
			},
		},
		{
			name: "aliases",
			in: yml(
				"x-ports: &ports",
				`  - "5432:5432"`,
				"x-port: &port 6379:6379",
				"x-env: &env",
				"  APP_URL: http://localhost:3000",
				"x-url: &url http://localhost:3000",
				"x-item: &item APP_URL=http://localhost:3000",
				"x-service: &service",
				`  ports: ["8080:80"]`,
				"services:",
				"  db:",
				"    ports: *ports",
				"    environment: *env",
				"  cache:",
				"    ports:",
				"      - *port",
				"    environment:",
				"      APP_URL: *url",
				"  worker:",
				"    environment:",
				"      - *item",
				"  web: *service",
				"  api:",
				"    <<: *service",
			),
		},
		{
			name: "only ports and environment",
			in: yml(
				"x-urls:",
				"  app: http://localhost:3000",
				"services:",
				"  minio:",
				"    image: minio/minio",
				`    command: ["server", "/data", "--address", "127.0.0.1:9000"]`,
				"    entrypoint: curl -f http://localhost:9000",
				"    healthcheck:",
				`      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]`,
				"    labels:",
				`      - "url=http://localhost:9000"`,
				"    expose:",
				`      - "9000:9000"`,
				"    x-ports:",
				`      - "9000:9000"`,
				"configs:",
				"  app:",
				`    content: "URL=http://localhost:3000"`,
			),
		},
		{name: "empty file", in: ""},
		{name: "only comments", in: yml("# services:", "#   db:", `#     ports: ["5432:5432"]`)},
		{name: "no services", in: yml("name: flowy", "volumes:", "  data: {}")},
		{name: "no service", in: yml("services:")},
		{name: "services in a list", in: yml("services:", "  - db")},
		{name: "a list at the top", in: yml("- services:", "    db:", `      ports: ["5432:5432"]`)},
		{name: "service that is not a mapping", in: yml("services:", "  db: postgres")},
		{
			name: "ports and environment that are not lists or mappings",
			in:   yml("services:", "  db:", `    ports: "5432:5432"`, "    environment: APP_URL=http://localhost:3000"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, changes := rewrite(t, tt.in)
			if want := cmp.Or(tt.want, tt.in); out != want {
				t.Errorf("Rewrite =\n%s\nwant\n%s", out, want)
			}
			checkChanges(t, changes, tt.changes)
		})
	}
}

func TestRewrite_InvalidYAML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{name: "unclosed list", in: yml("services:", "  db:", `    ports: ["5432:5432"`)},
		{name: "tab indent", in: "services:\n\tdb: {}\n"},
		{name: "unknown alias", in: yml("services:", "  db: *nope")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, changes, err := compose.Rewrite([]byte(tt.in), flowy)
			if err == nil || !strings.HasPrefix(err.Error(), "parse compose file: ") {
				t.Errorf("Rewrite error = %v, want one that starts with parse compose file", err)
			}
			if out != nil || changes != nil {
				t.Errorf("Rewrite returned %q and %v with its error", out, changes)
			}
		})
	}
}

// FuzzRewrite checks that Rewrite never panics, and that what holds for any
// rewrite holds for whatever file it accepts.
func FuzzRewrite(f *testing.F) {
	src, err := os.ReadFile("testdata/compose.dev.yaml")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(src)
	f.Add([]byte(yml("services:", "  db:", `    ports: ["5432:5432", 6379:6379, '127.0.0.1::80']`)))
	f.Add([]byte(yml("services:", "  app:", "    environment: {APP_URL: 'http://localhost:3000', WS: ws://127.0.0.1}")))
	f.Fuzz(func(t *testing.T, src []byte) {
		out, changes, err := compose.Rewrite(src, flowy)
		if err != nil {
			return
		}
		checkRewrite(t, src, out, changes)
	})
}
