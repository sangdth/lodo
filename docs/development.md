# Development

Contributor guide for building, testing, and reinstalling `lodo` from source. See the [README](../README.md) for quick
start and [docs/reference.md](reference.md) for user-facing behavior.

## Run tests and checks

```sh
go test -race ./...                       # all tests
go vet ./... && golangci-lint run ./...   # lint, config in .golangci.yml
go test ./internal/tui -update            # rewrite that package's golden files after an intended change
go run ./cmd/lodo doctor                  # run lodo from source
```

Set the version printed by `lodo version`:

```sh
go build -ldflags "-X main.Version=v0.1.0" -o lodo ./cmd/lodo
```

Further project docs:

- [docs/plan.md](plan.md) — architecture, settled decisions, and phases
- [docs/setup-log.md](setup-log.md) — hand test log on a real Mac
- [AGENTS.md](../AGENTS.md) — code layout and development rules

## Change it and use it right away

```sh
go install ./cmd/lodo   # replaces ~/go/bin/lodo
```

| What you changed                                      | After `go install`                         |
| ----------------------------------------------------- | ------------------------------------------ |
| TUI, messages, checks, the CLI                        | nothing, just run `lodo ps` or `lodo dev`  |
| The listen hook, `internal/dev/bind.cjs`              | nothing: the next `lodo dev` writes it     |
| What goes into the generated dnsmasq or Caddy files   | `lodo apply`                               |
| The root script, the sudoers rule or the loopback job | `lodo setup`, which asks for your password |

`lodo doctor` tells you which applies: "script out of date" means `lodo setup`, and "generated files out of date"
means `lodo apply`.
