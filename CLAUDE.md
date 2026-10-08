# CLAUDE.md

`lcd` is a Go terminal app that manages `.local` names on macOS through dnsmasq, `/etc/resolver` files and
Caddy. `docs/plan.md` holds the design, the settled decisions and the phases.

## Commands

```bash
go test -race ./...                       # all tests
go test ./internal/store -run TestAdd     # one test
go test ./internal/dnsmasq -update        # rewrite that package's golden files after an intended change
go vet ./... && golangci-lint run ./...   # lint (config in .golangci.yml)
go run ./cmd/lcd doctor                   # run the CLI from source
```

`-update` works per package: only packages with golden files define the flag.

## Rules

- No test touches `/etc`, `/Library`, `/opt/homebrew`, `sudo` or the real `~/.config/lcd`. Tests build paths with
  `paths.ForTest(t.TempDir())` and run commands through `run.Fake`.
- Every external command goes through `run.Runner`, with the absolute tool path from `paths.Paths` and each
  argument passed separately. Nothing builds a shell command line from data.
- The root script only creates `/etc/resolver` files named by lines matching `store.NamePattern`, with fixed
  content, and only deletes files that start with lcd's marker line. Changes to it keep those three properties.
- Golden files live in each package's `testdata/` and use `github.com/charmbracelet/x/exp/golden`.
- Charm v2 modules use the `charm.land/...` import paths, such as `charm.land/bubbletea/v2`.
- Errors are wrapped with `fmt.Errorf("context: %w", err)`, lowercase, without trailing punctuation.

## Go development

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.
