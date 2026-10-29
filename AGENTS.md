# CLAUDE.md

`lodo` is a Go terminal app that manages `.test` names on macOS through dnsmasq, `/etc/resolver` files and
Caddy. `docs/plan.md` holds the design, the settled decisions and the phases; `docs/setup-log.md` records
the hand test on a real Mac. The README stays short; user-facing detail goes in `docs/reference.md`.

## Commands

```bash
go test -race ./...                       # all tests
go test ./internal/store -run TestAdd     # one test
go test ./internal/dnsmasq -update        # rewrite that package's golden files after an intended change
go vet ./... && golangci-lint run ./...   # lint (config in .golangci.yml)
go run ./cmd/lodo doctor                  # run the CLI from source; read-only
go run ./cmd/lodo scan --json             # what the project in this folder lacks; read-only
go build -o lodo ./cmd/lodo && ./lodo ps  # the dashboard; it opens only after lodo setup on this Mac
```

The installed `lodo` is `~/go/bin/lodo`. Every finished change ends with the steps that make it take effect,
for a user new to Go: `go install ./cmd/lodo` from the repo root, then nothing, `lodo apply` or `lodo setup`, by
the table in the README's "Change it and use it right away".

`-update` works per package: only packages with golden files define the flag. A commit needs `gofmt -l .`
empty, `go vet`, `golangci-lint run` and `go test -race ./...` clean; chain them with `set -e` so a failure
stops the commit.

## Layout

- `cmd/lodo`: subcommand dispatch and output. The only code that builds `paths.Default()` and `run.Exec`. Bare
  `lodo` prints the help, `lodo ps` opens the dashboard, `lodo dev` (`dev.go`) starts a project, and `lodo exec`
  runs a command with the project's variables.
- `internal/paths`: every file, tool and system path. `paths.ForTest(root)` moves all of them under `root`.
- `internal/run`: the only code that runs commands. `Replace` execs one in lodo's place, for `lodo dev`. `run.Fake`
  records them in tests.
- `internal/fsutil`: atomic writes that skip unchanged content.
- `internal/store`: `domains.json` and every domain rule: names, addresses, ports, projects, the own block, order.
  Other packages call its helpers (`Find`, `HasCompose`, `Domain.HasSite`, `ValidateProject`, `Suffix`) rather
  than spell a rule out again.
- `internal/brew`, `internal/dnsmasq`, `internal/caddy`: Homebrew's service jobs, the generated dnsmasq config
  and resolver list, the generated Caddyfile. `brew` reads a job's state with `launchctl print` and restarts a
  loaded job with `launchctl kickstart -k`, since each `brew` call costs 0.4s; only an unloaded job and `Stop`
  go through `brew services`.
- `internal/system`: setup, apply and uninstall; the root script, sudoers rule and loopback plist templates;
  lodo's blocks in Homebrew's dnsmasq.conf and Caddyfile. `Apply` refuses to run until setup has.
  `Generated`: the three generated files, which `WriteFiles` writes and doctor compares.
- `internal/compose`: finds the project a folder is in (`ProjectRoot`) and its compose file, reads its services'
  images and published ports (`Services`), and rewrites the file's ports and `localhost` URLs for lodo; `MapURLs`
  and `Values.Point` also serve the scan. It reads files and never writes them.
- `internal/scan`: reads a project's apps, dev servers, ports, `.env` address and the compose web pages it names
  (Mailpit's inbox), and proposes the names and compose link the list lacks, and the variables `lodo dev` sets
  (`Proposal.Env`, `scan.Var`). It also reads `PORT=` from an app's `.env` (NestJS, Create React App, unknown
  commands), the `KEY=value` lines of each app's `.env`, the git repository's main checkout (`repoRoot`), so a
  worktree's variables swap another checkout's names for its own (`siblingNames`), and the names Caddy serves per
  port (`ValuesFor`). Each app keeps its own port; a
  port taken on the project's address moves the app to the next free address, with a note, since `lodo dev` binds
  one app per port. Its notes point at `lodo dev --edit`. All dev server rules and the images lodo names
  (`webImages`) live here, and so does the rule for a listed name another project's folder still has (`Taken`,
  `BelongsTo`). It reads files and never writes them.
- `internal/dev`: what `lodo dev` needs: the package manager (`PackageManager`), the address per port (`Ports`),
  the embedded listen hook `bind.cjs`, the project's hook file (`Hook`, `WriteHooks`) and `NodeOptions`.
- `internal/check`: the eight doctor checks and the per-domain probes: dnsmasq directly, macOS, and HTTP through
  Caddy. `Report` returns both from one probe; `Prerequisites` runs checks 1 to 5 without probing. Code names a
  check by its ID constant (`check.IDDnsmasq` … `check.IDCaddy`), never by a bare number.
- `internal/tui`: the Bubble Tea v2 dashboard and the setup form (`Setup`), the only review UI: `lodo dev` runs it
  inline, the dashboard's `i` in its box. The form scrolls to the height it gets: the terminal's, or the box's from
  `layout`. It talks to the system only through its `Backend` interface; tests use a fake backend, and snapshot
  `View().Content` with golden files.

## Rules

- No test changes the system: nothing writes to `/etc`, `/Library`, `/opt/homebrew` or the real `~/.config/lodo`,
  and nothing runs `sudo` or starts a service. Tests build paths with `paths.ForTest(t.TempDir())` and run lodo's
  commands through `run.Fake`. A few run `dnsmasq --test`, `caddy validate`, `visudo -c` and `plutil -lint`
  read-only on generated files, and `node --test` on the bind hook (`TestBind`), and skip when the tool is missing.
- `lodo apply`, `lodo setup`, `lodo uninstall`, `lodo ps`'s keys and `lodo dev` change this Mac's DNS and
  services, `lodo dev` starts a project's dev servers, and `lodo exec` runs whatever it is given: run them only when
  the task asks for it. `lodo doctor`,
  `lodo scan`, `lodo version`, bare `lodo` and the tests are safe.
- Every external command goes through `run.Runner`, with the absolute tool path from `paths.Paths` and each
  argument passed separately. Nothing builds a shell command line from data. The `PATH` lookups are the package
  manager `lodo dev` runs and the command `lodo exec` runs, in `cmd/lodo`; each then execs through
  `run.Runner.Replace`, `<pm> run dev` in the project root and `lodo exec`'s command in the current folder.
- `lodo dev` writes only `~/.config/lodo/bind.cjs` and `~/.config/lodo/dev/<project>.cjs` (`paths.BindHook`,
  `paths.DevDir`), besides what its setup form saves, and gives apps their address through
  `NODE_OPTIONS=--require`, also set in `npm_config_node_options` or `pnpm_config_node_options`, which beat npm's and
  pnpm's `node-options` setting (`<pm> config get node-options` reads it first). The hook moves a listen only on the
  project's listed ports, and only from no host or a loopback or wildcard host. It adds `.<project>` to
  `__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS` once, so Vite accepts the names. Node only. `lodo dev` and `lodo exec`
  export `DOCKER_HOST_IP` and the scan's variables in place of `.env` values (`projectEnv`); `lodo exec` skips the
  hook.
- The root script reads the names on standard input (`run.Runner.RunInput`), never from a path, and copies at
  most `system.MaxScriptInput` bytes into its own temp dir. It only creates `/etc/resolver` files named by lines
  matching `store.NamePattern`, with fixed content, and only deletes files that start with lodo's marker line.
  Changes to it keep those three properties.
- Setup trusts Caddy's local CA only when a name has HTTPS on: it copies the root file Caddy wrote to
  `~/.config/lodo/caddy-root.crt` and runs `security add-trusted-cert` on that copy; uninstall untrusts the copy.
  A trusted root signs certificates for any site. Nothing uses Caddy's admin API; lodo's block in Homebrew's
  Caddyfile turns it off.
- Every generated site passes `Host` through as the `.test` name, and turns `Origin` into
  `http://localhost:<port>` only on Next's dev paths and only when it is the site's own origin: a `Host` rewrite
  makes Mailpit's websocket answer 403, and a wider `Origin` rewrite breaks Server Actions and lets any web page
  open Next's hot reload socket (docs/plan.md, "Result of 11.0").
- Caddy runs as the user and listens on port 80 of every address: macOS needs root to listen below port 1024 on
  one address, so a Caddy `bind` fails. Every generated site aborts connections from outside `127.0.0.0/8` and `::1`.
- lodo never writes a project file. A project's per-checkout values reach its processes as exported variables,
  which beat a `.env` in dotenv, Next, Node and compose, so a `.env` copied between worktrees works as it is. lodo
  reads project files only through `fsutil.ReadRegular`.
- Everything lodo prints from a `.env` masks a URL's user info as `***@`: a `scan.Var` holds the masked value in
  `Value` and the real one in an unexported field only `Var.Env` returns.
- The user picks a project's name. The folder's name is only a suggestion: every flow that proposes a project name
  (the setup form, `lodo scan --name`) lets the user change it before anything is added, and the subdomains follow
  it (`<app>.<name>`). The chosen name keeps the project's folder as `Domain.Root`, so later scans use it and never
  propose the folder's name next to it; paths compare with `scan.SameFolder`, since the same folder can come by
  another case or symlink. A git worktree of a listed main checkout gets `<main><n>.test` on `127.0.1.<n>`, the
  lowest `n` whose address is free or holds only a gone folder's `<main><n>.test` (`scan.worktreeSlot`), with the
  main checkout's HTTPS per label; `lodo dev -y` saves that with no key pressed (`tui.Start.Auto`). A folder whose
  name another project's folder still has gets `<name>-2.test`, the first of `-2`, `-3` … that no listed name uses.
  A listed name, typed in or picked by the scan, whose `Root` is unset or gone is taken over (`Proposal.Claim`), and
  the project's old name goes with its names; one whose folder still exists is refused with the `scan.BelongsTo`
  note, in the form and in `lodo scan --name` alike (`scan.Taken`, with `Backend.Gone`). The setup form names each
  app itself: `<label>.<project>`, or `<project>` alone for one app, the root's when there is one, and no label
  another app's folder gives; the scan takes only the project's name. A later scan keeps a listed name of the
  project on an app's port and address as that app's name.
- Names end in `.test`, from `store.TLD`. macOS sends a name with one label before `.local` to Bonjour only, so
  `.local` can't serve project names like `media.local`.
- Golden files live in each package's `testdata/` and use `github.com/charmbracelet/x/exp/golden`.
- Charm v2 modules use the `charm.land/...` import paths, such as `charm.land/bubbletea/v2`.
- Errors are wrapped with `fmt.Errorf("context: %w", err)`, lowercase, without trailing punctuation.

## Go development

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.
