# lcd

lcd lets several projects run on one Mac at the same time, each database and app on its usual port. Every
project gets its own loopback address, like `127.0.1.3`, and a name that ends in `.lcd`, so flowy's Postgres
listens on `127.0.1.3:5432` while crm's listens on `127.0.1.1:5432`. lcd is a terminal app: it keeps the list
of names and makes dnsmasq, macOS's `/etc/resolver` files and, for names with a port, Caddy follow it.

```text
flowy.lcd -> 127.0.1.3 -> 127.0.1.3:5432 (Postgres), 127.0.1.3:3000 (Next)
crm.lcd   -> 127.0.1.1 -> 127.0.1.1:5432 (Postgres)
http://dashboard.crm.lcd -> Caddy on port 80 -> 127.0.1.1:3000
```

## Requirements

- macOS on Apple Silicon, with Homebrew in `/opt/homebrew`. Intel Macs, with Homebrew in `/usr/local`, don't work.
- Go 1.27 or newer, to build lcd.
- dnsmasq: `brew install dnsmasq`.
- Caddy, only for names with a port: `brew install caddy`.
- An admin account: setup uses `sudo`.

## Install

```sh
go install github.com/sangdth/lcd/cmd/lcd@latest
```

That puts `lcd` in `$(go env GOPATH)/bin`, usually `~/go/bin`, which needs to be on your `PATH`. From a clone,
`go build -o lcd ./cmd/lcd` builds it in place.

## Setup

Run `lcd setup` as yourself, not with `sudo`. It asks for your password once, before it changes anything. Then it:

1. creates `~/.config/lcd`, an empty `domains.json` when there is none, and the files lcd generates from it;
2. backs up Homebrew's `/opt/homebrew/etc/dnsmasq.conf` to `dnsmasq.conf.before-lcd` (first run only), drops its
   `conf-file`, `listen-address`, `port`, `bind-interfaces` and `bind-dynamic` lines, and adds lcd's block:
   include `~/.config/lcd/dnsmasq.conf` and listen on `127.0.0.1` port 53535. Every other line stays;
3. stops a root dnsmasq job, the kind `sudo brew services start dnsmasq` makes, and removes its plist from
   `/Library/LaunchDaemons`;
4. installs the resolver script, `/Library/Application Support/lcd/apply-resolvers.sh`, and the sudoers rule,
   `/etc/sudoers.d/lcd`, which lets you run that script, and nothing else, without a password;
5. installs `/Library/LaunchDaemons/io.lcd.loopback.plist`, which adds `127.0.1.1`–`127.0.1.50` to `lo0` now
   and at every boot;
6. removes `/etc/resolver/local` if it exists: that file takes every `.local` name away from Bonjour;
7. starts dnsmasq as you, with `brew services`;
8. when Caddy is installed, backs up `/opt/homebrew/etc/Caddyfile` to `Caddyfile.before-lcd` if there is one,
   makes it import `~/.config/lcd/Caddyfile`, and starts Caddy as you;
9. runs the script through `sudo -n` to prove the rule works, then prints `lcd doctor`'s checks.

It prints one line per step and stops at the first failure, with the command that fixes it. Running it again is
safe: it repairs the setup and keeps the first backup. Do so after you install Caddy, and when `lcd doctor`
reports the script out of date after an upgrade. After setup, adding, editing and deleting names never asks for
a password, and neither does `lcd apply`.

## Using lcd

`lcd` opens the list of names. It refuses to open while `lcd doctor` checks 1–5 fail, and prints them.

```text
 lcd   dnsmasq ●   loopback ●   resolvers ●   caddy ●
 name                      address          port   own  check
 ● crm.lcd                 127.0.1.1               own  dns ✓
 ●   dashboard.crm.lcd     127.0.1.1        3000   own  dns ✓  http ✓
 ● flowy.lcd               127.0.1.3               own  dns ✓
 ○ old.lcd                 127.0.0.1                    –
```

The top line shows doctor checks 1, 3, 4 and 8: a green dot passes, a red one needs `lcd doctor`, and Caddy's
dot is dim when no enabled name has a port. `●` marks a name that is on, `○` one that is off. Each change saves
`domains.json`, applies it and checks every name. The status line at the bottom, above the key help, says what
failed, or why the selected name fails.

| Key         | What it does                                     |
| ----------- | ------------------------------------------------ |
| `a`         | add a name                                       |
| `e`         | edit the selected name                           |
| `d`         | delete it; `y` confirms, any other key keeps it  |
| `space`     | turn it on or off                                |
| `c`         | copy `DOCKER_HOST_IP=<address>` to the clipboard |
| `l`         | show dnsmasq's query log; `l` or `esc` goes back |
| `r`         | read `domains.json` again and apply it           |
| `q`         | quit; `ctrl+c` quits from anywhere               |
| `up` `down` | move; `j` and `k` work too                       |

In the add or edit form, `enter` saves, `tab` goes to the next field (`shift+tab` back) and `esc` cancels. While
you type a new name, the form fills in the address: the parent's address for a subdomain of a listed name, else
the lowest free own address. Typing in the address field stops that. A port needs Caddy installed and set up;
until then the form says what to run.

### Rules

- **Names end in `.lcd`.** macOS sends a name like `flowy.local`, with one label before `.local`, to Bonjour
  only. Labels use `a-z`, `0-9` and inner `-`, 1–63 characters each.
- **A project is the last two labels.** `crm.lcd`, `api.crm.lcd` and `test.crm.lcd` are one project.
- **A subdomain shares its project's address or takes its own.** The form fills in the parent's. While `crm.lcd`
  is on, an unlisted `foo.crm.lcd` resolves to its address anyway, and so does a listed one you turn off.
- **An own address belongs to one project.** The own block is `127.0.1.1`–`127.0.1.50`. `127.0.0.1`, or any
  other address in `127.0.0.0/8`, can be shared.
- **A port makes `http://<name>` reach `<address>:<port>`** through Caddy on port 80. HTTP only. Port 80 itself
  is refused, because Caddy listens there.
- **Apps listen on their project's address**, for example `next dev -H 127.0.1.3` or `vite --host 127.0.1.3`.
- **Docker:** `c` copies `DOCKER_HOST_IP=127.0.1.3`. Put it in the project's `.env` and publish ports as
  `"${DOCKER_HOST_IP:-127.0.0.1}:5432:5432"`, so each project's Postgres gets port 5432 on its own address.

## Commands

`lcd apply` writes lcd's files from `domains.json`, restarts dnsmasq, runs the resolver script, restarts Caddy
when its sites changed or it stopped, then checks every enabled name, prints one line each and exits 1 if one
fails. The list does the same after every change. It refuses to run before `lcd setup`. Use it after editing
`domains.json` by hand:

```json
{"version": 1, "domains": [
  {"name": "crm.lcd", "address": "127.0.1.1", "enabled": true},
  {"name": "dashboard.crm.lcd", "address": "127.0.1.1", "port": 3000, "enabled": true}
]}
```

`lcd doctor` runs eight checks and prints one line each, with the fix under each failure. It exits 1 if one fails.

1. `dnsmasq`: installed, run by `brew services` as you, and no root job shadows it.
2. `dnsmasq config`: Homebrew's `dnsmasq.conf` has one `conf-file=` line, lcd's, and listens on `127.0.0.1:53535`.
3. `loopback`: the `io.lcd.loopback` job is loaded and all 50 addresses are on `lo0`.
4. `resolvers`: the script is root-owned, mode 755 and current; `sudo` runs it without a password; every
   enabled name has its `/etc/resolver` file.
5. `resolver for .local`: there is no `/etc/resolver/local`.
6. `generated files`: lcd's generated files in `~/.config/lcd` match `domains.json`.
7. `names resolve`: each enabled name resolves through dnsmasq and macOS; with a port, Caddy reaches the app.
8. `caddy`, only when an enabled name has a port: Caddy is installed, imports lcd's file, runs, passes
   `caddy validate`, and holds port 80.

`lcd uninstall` removes what setup installed; see [Uninstall](#uninstall). `lcd version` prints the version, and
`lcd help` the usage.

## How it works

- **dnsmasq** runs as you, from Homebrew's service, on `127.0.0.1:53535`, a port that needs no root. Homebrew's
  `dnsmasq.conf` includes `~/.config/lcd/dnsmasq.conf`: one `address=/<name>/<address>` line per enabled name,
  and every query logged to `~/.config/lcd/dnsmasq.log`.
- **`/etc/resolver/<name>`** makes macOS ask dnsmasq for that name and its subdomains. lcd writes one file per
  enabled name through a root-owned script that `sudo` runs without a password. The script only writes names
  that match the name rule, and only deletes files that start with lcd's marker line, `# lcd`.
- **The loopback LaunchDaemon**, `io.lcd.loopback`, adds `127.0.1.1`–`127.0.1.50` to `lo0` at every boot. macOS
  has only `127.0.0.1` by default.
- **Caddy** runs as you on port 80. Homebrew's `Caddyfile` imports `~/.config/lcd/Caddyfile`, which has one
  `http://<name>` site per enabled name with a port, forwarding to `<address>:<port>`.
- **`~/.config/lcd/domains.json`** is the only source of truth. lcd rewrites its generated files from it on
  every change.

## Troubleshooting

Start with `lcd doctor`: every failed check prints the command that fixes it.

- **`can't assign requested address` from Docker, or `EADDRNOTAVAIL` from Node:** the loopback addresses are
  missing. Check 3; `lcd setup` adds them again.
- **`sudo: a password is required`:** the sudoers rule is missing or the script changed. Check 4, then `lcd setup`.
- **`dig flowy.lcd` finds nothing:** expected. `dig` and `nslookup` skip `/etc/resolver`; apps and browsers use
  it. Test with `dscacheutil -q host -a name flowy.lcd`, or ask dnsmasq: `dig @127.0.0.1 -p 53535 flowy.lcd`.
- **A name isn't found right after you add it, or the log view shows no queries:** the lookup never reached
  dnsmasq. Its resolver file is missing (check 4), or a VPN is capturing DNS.
- **Check 4 says a resolver file is "not written by lcd":** someone made it by hand, and lcd never replaces such
  a file. Run the `sudo rm` that doctor prints, then `lcd apply`.
- **A name resolves to the wrong address:** an `/etc/hosts` line for it wins over DNS. The name's check points
  at the line; remove it.
- **`foo.crm.lcd` resolves although it isn't listed:** expected. dnsmasq's `address=` lines and the resolver
  files both match subdomains. Add a row only to give it another address or a port.
- **`502 Bad Gateway` on `http://dashboard.crm.lcd`:** Caddy is up but nothing listens on `127.0.1.1:3000`; the
  check says `app down`. Vite needs `--host 127.0.1.1`, and Docker ports bind `DOCKER_HOST_IP`.
- **The wrong app answers:** a Node app listening on `*:3000` answers on every loopback address, so it shadows
  other projects' port 3000. Start each app on its own address: `next dev -H 127.0.1.3`.
- **`http://dashboard.crm.lcd` refuses the connection:** Caddy isn't running or another program holds port 80.
  Check 8 names it.
- **Names resolve for one app but not another:** a VPN or Cloudflare WARP is capturing DNS.
- **Containers can't reach `flowy.lcd`:** they don't need to. Inside Docker they use the service name.

## Uninstall

`lcd uninstall` asks for your password, then removes lcd's `/etc/resolver` files, stops dnsmasq, and restores
Homebrew's `dnsmasq.conf` from `dnsmasq.conf.before-lcd`, or removes lcd's block when there is no backup. When
Caddy was set up, it stops Caddy and restores `Caddyfile.before-lcd`, or removes lcd's import. Last, it takes
`127.0.1.1`–`127.0.1.50` off `lo0` and removes the loopback job, the sudoers rule and the script.

It keeps `~/.config/lcd`, so a later `lcd setup` brings your names back. dnsmasq stays stopped; to run it as root
on port 53 again, as before lcd: `sudo brew services start dnsmasq`.

## Development

```sh
go test -race ./...                       # all tests
go vet ./... && golangci-lint run ./...   # lint, config in .golangci.yml
go test ./internal/tui -update            # rewrite that package's golden files after an intended change
go run ./cmd/lcd doctor                   # run lcd from source
```

Golden files live in each package's `testdata/`; only packages that have them take `-update`. No test changes
the system, and the few that run `dnsmasq --test`, `caddy validate`, `visudo -c` or `plutil -lint` skip when the
tool is missing. `go build -ldflags "-X main.Version=v0.1.0" -o lcd ./cmd/lcd` sets the version `lcd version`
prints. `docs/plan.md` holds the design and its decisions, `docs/setup-log.md` the hand test on a real Mac, and
`CLAUDE.md` the layout and rules for changing the code.
