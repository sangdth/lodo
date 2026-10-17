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
go run ./cmd/lodo scan --json              # what the project in this folder lacks; read-only
go build -o lodo ./cmd/lodo && ./lodo        # the TUI; it opens only after lodo setup on this Mac
```

The installed `lodo` is `~/go/bin/lodo`. Every finished change ends with the steps that make it take effect,
for a user new to Go: `go install ./cmd/lodo` from the repo root, then nothing, `lodo apply` or `lodo setup`, by
the table in the README's "Change it and use it right away".

`-update` works per package: only packages with golden files define the flag. A commit needs `gofmt -l .`
empty, `go vet`, `golangci-lint run` and `go test -race ./...` clean; chain them with `set -e` so a failure
stops the commit.

## Layout

- `cmd/lodo`: subcommand dispatch and output. The only code that builds `paths.Default()` and `run.Exec`.
- `internal/paths`: every file, tool and system path. `paths.ForTest(root)` moves all of them under `root`.
- `internal/run`: the only code that runs commands. `run.Fake` records them in tests.
- `internal/fsutil`: atomic writes that skip unchanged content.
- `internal/store`: `domains.json` and every domain rule: names, addresses, ports, projects, the own block, order.
  Other packages call its helpers (`Find`, `HasCompose`, `Domain.HasSite`, `ValidateProject`, `Suffix`) rather
  than spell a rule out again.
- `internal/brew`, `internal/dnsmasq`, `internal/caddy`: Homebrew's service jobs, the generated dnsmasq config
  and resolver list, the generated Caddyfile. `brew` reads a job's state with `launchctl print` and restarts a
  loaded job with `launchctl kickstart -k`, since each `brew` call costs 0.4s; only an unloaded job and `Stop`
  go through `brew services`.
- `internal/system`: setup, apply and uninstall; the root script, sudoers rule and loopback plist templates;
  lodo's blocks in Homebrew's dnsmasq.conf and Caddyfile. `Apply` refuses to run until setup has. `Tracked`: the
  one git call. `Generated`: the three generated files, which `WriteFiles` writes and doctor compares.
- `internal/compose`: finds the project a folder is in (`ProjectRoot`) and its compose file, reads its services'
  images and published ports (`Services`), and rewrites the file's ports and `localhost` URLs for lodo; `MapURLs`
  and `Values.Point` also serve the scan. It reads files and never writes them.
- `internal/scan`: reads a project's apps, dev servers, ports, `.env` address and the compose web pages it names
  (Mailpit's inbox), and proposes the names, compose link and host fixes the list lacks. It also reads `PORT=` from
  an app's `.env` (NestJS, Create React App, unknown commands), the localhost URL lines of each app's `.env`
  (`Proposal.Env`, `ApplyEnv`) and the names Caddy serves per port (`ValuesFor`). In a turbo project whose root
  `.env` holds the project's address, or gets it from `y`'s link, the host fixes use `compose.HostIP`, and the
  root's turbo line loads it from `.env` (`turbo.go`). All dev server rules and the images lodo names (`webImages`)
  live here. It reads files and never writes them.
- `internal/check`: the eight doctor checks and the per-domain probes: dnsmasq directly, macOS, and HTTP through
  Caddy. `Report` returns both from one probe; `Prerequisites` runs checks 1 to 5 without probing. Code names a
  check by its ID constant (`check.IDDnsmasq` … `check.IDCaddy`), never by a bare number.
- `internal/tui`: the Bubble Tea v2 model. It talks to the system only through its `Backend` interface; tests
  use a fake backend, and snapshot `View().Content` with golden files.

## Rules

- No test changes the system: nothing writes to `/etc`, `/Library`, `/opt/homebrew` or the real `~/.config/lodo`,
  and nothing runs `sudo` or starts a service. Tests build paths with `paths.ForTest(t.TempDir())` and run lodo's
  commands through `run.Fake`. A few run `dnsmasq --test`, `caddy validate`, `visudo -c` and `plutil -lint`
  read-only on generated files, and skip when the tool is missing.
- `lodo apply`, `lodo setup`, `lodo uninstall` and the TUI's keys change this Mac's DNS and services: run them only
  when the task asks for it. `lodo doctor`, `lodo scan`, `lodo version` and the tests are safe.
- Every external command goes through `run.Runner`, with the absolute tool path from `paths.Paths` and each
  argument passed separately. Nothing builds a shell command line from data.
- The root script reads the names on standard input (`run.Runner.RunInput`), never from a path, and copies at
  most `system.MaxScriptInput` bytes into its own temp dir. It only creates `/etc/resolver` files named by lines
  matching `store.NamePattern`, with fixed content, and only deletes files that start with lodo's marker line.
  Changes to it keep those three properties.
- Setup trusts Caddy's local CA only when a name has HTTPS on: it copies the root file Caddy wrote to
  `~/.config/lodo/caddy-root.crt` and runs `security add-trusted-cert` on that copy; uninstall untrusts the copy.
  A trusted root signs certificates for any site. Nothing uses Caddy's admin API; lodo's block in Homebrew's
  Caddyfile turns it off.
- Caddy runs as the user and listens on port 80 of every address: macOS needs root to listen below port 1024 on
  one address, so a Caddy `bind` fails. Every generated site aborts connections from outside `127.0.0.0/8` and `::1`.
- The TUI writes two kinds of `.env` lines in a project: `DOCKER_HOST_IP` in the `.env` a linked compose file runs
  with, and the scan's URL lines in an app's `.env`, each only while it still reads as scanned (`scan.ApplyEnv`).
  Both go through `fsutil`, only inside the project once symlinks are followed, never into a file git tracks
  (`system.Tracked`; git runs with `safe.bareRepository=explicit` and `core.fsmonitor=false`). Everything else in a
  project stays read-only, and lodo reads project files only through `fsutil.ReadRegular`.
- Everything lodo prints from a `.env` masks a URL's user info as `***@`: a `.env` `Fix` holds the masked line in
  `Old`/`New` and the real one in unexported fields only `scan.ApplyEnv` reads.
- The user picks a project's name. The folder's name is only a suggestion: every flow that proposes a project
  name (the scan's review, `lodo scan`) lets the user change it before anything is added, and the subdomains
  follow it (`<app>.<name>`). The chosen name keeps the project's folder as `Domain.Root`, so later scans use it
  and never propose the folder's name next to it. `e` in the review also names an app: `<label>.<project>`, or
  `<project>` alone. A later scan keeps a listed name of the project on an app's port and address as that app's
  name.
- Names end in `.test`, from `store.TLD`. macOS sends a name with one label before `.local` to Bonjour only, so
  `.local` can't serve project names like `media.local`.
- Golden files live in each package's `testdata/` and use `github.com/charmbracelet/x/exp/golden`.
- Charm v2 modules use the `charm.land/...` import paths, such as `charm.land/bubbletea/v2`.
- Errors are wrapped with `fmt.Errorf("context: %w", err)`, lowercase, without trailing punctuation.

## Go development

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.
