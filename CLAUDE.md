# CLAUDE.md

`lodo` is a Go terminal app that manages `.test` names on macOS through dnsmasq, `/etc/resolver` files and
Caddy. `docs/plan.md` holds the design, the settled decisions and the phases; `docs/setup-log.md` records
the hand test on a real Mac.

## Commands

```bash
go test -race ./...                       # all tests
go test ./internal/store -run TestAdd     # one test
go test ./internal/dnsmasq -update        # rewrite that package's golden files after an intended change
go vet ./... && golangci-lint run ./...   # lint (config in .golangci.yml)
go run ./cmd/lodo doctor                   # run the CLI from source; read-only
go build -o lodo ./cmd/lodo && ./lodo        # the TUI; it opens only after lodo setup on this Mac
```

`-update` works per package: only packages with golden files define the flag. A commit needs `gofmt -l .`
empty, `go vet`, `golangci-lint run` and `go test -race ./...` clean; chain them with `set -e` so a failure
stops the commit.

## Layout

- `cmd/lodo`: subcommand dispatch and output. The only code that builds `paths.Default()` and `run.Exec`.
- `internal/paths`: every file, tool and system path. `paths.ForTest(root)` moves all of them under `root`.
- `internal/run`: the only code that runs commands. `run.Fake` records them in tests.
- `internal/fsutil`: atomic writes that skip unchanged content.
- `internal/store`: `domains.json` and every domain rule: names, addresses, ports, projects, the own block, order.
- `internal/brew`, `internal/dnsmasq`, `internal/caddy`: `brew services`, the generated dnsmasq config and
  resolver list, the generated Caddyfile.
- `internal/system`: setup, apply and uninstall; the root script, sudoers rule and loopback plist templates;
  lodo's blocks in Homebrew's dnsmasq.conf and Caddyfile. `Apply` refuses to run until setup has.
- `internal/compose`: finds a project's compose file and rewrites its ports and `localhost` URLs for lodo, and
  finds `next dev` lines without `-H`, for the TUI's preview. It reads files and never writes them.
- `internal/check`: the eight doctor checks and the per-domain probes: dnsmasq directly, macOS, and HTTP through
  Caddy. `Report` returns both from one probe; `Prerequisites` runs checks 1 to 5 without probing.
- `internal/tui`: the Bubble Tea v2 model. It talks to the system only through its `Backend` interface; tests
  use a fake backend, and snapshot `View().Content` with golden files.

## Rules

- No test changes the system: nothing writes to `/etc`, `/Library`, `/opt/homebrew` or the real `~/.config/lodo`,
  and nothing runs `sudo` or starts a service. Tests build paths with `paths.ForTest(t.TempDir())` and run lodo's
  commands through `run.Fake`. A few run `dnsmasq --test`, `caddy validate`, `visudo -c` and `plutil -lint`
  read-only on generated files, and skip when the tool is missing.
- `lodo apply`, `lodo setup`, `lodo uninstall` and the TUI's keys change this Mac's DNS and services: run them only
  when the task asks for it. `lodo doctor`, `lodo version` and the tests are safe.
- Every external command goes through `run.Runner`, with the absolute tool path from `paths.Paths` and each
  argument passed separately. Nothing builds a shell command line from data.
- The root script only creates `/etc/resolver` files named by lines matching `store.NamePattern`, with fixed
  content, and only deletes files that start with lodo's marker line. Changes to it keep those three properties.
- Setup trusts Caddy's local CA (`caddy trust`) only when a name has HTTPS on, and uninstall untrusts it: a
  trusted root signs certificates for any site.
- The TUI writes one project file: the `.env` a linked compose file runs with, only its `DOCKER_HOST_IP`
  lines, through `fsutil`, and never one that git tracks. Everything else in a project stays read-only.
- Names end in `.test`, from `store.TLD`. macOS sends a name with one label before `.local` to Bonjour only, so
  `.local` can't serve project names like `media.local`.
- Golden files live in each package's `testdata/` and use `github.com/charmbracelet/x/exp/golden`.
- Charm v2 modules use the `charm.land/...` import paths, such as `charm.land/bubbletea/v2`.
- Errors are wrapped with `fmt.Errorf("context: %w", err)`, lowercase, without trailing punctuation.

## Go development

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.
