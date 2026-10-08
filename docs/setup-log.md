# Hand test on Sang's Mac, 2026-10-08

The tool was named `lcd` and its names ended in `.lcd` when this ran. The prose uses today's names, `oo` and
`.oo`; the command output in code blocks is kept as it was printed.

macOS 27.0.1, dnsmasq 2.93, Caddy 2.11.6, Go 1.27.1. Step 2.6 of `docs/plan.md`.

## `oo setup`

Run by Sang in his terminal. One password prompt, every step passed:

```text
✓ Homebrew and dnsmasq are installed
lcd needs your password once, to install its root parts.
✓ lcd's files in /Users/sang/.config/lcd
✓ Homebrew's dnsmasq.conf includes lcd's (backup in dnsmasq.conf.before-lcd)
✓ no root dnsmasq job (stopped system/homebrew.mxcl.dnsmasq, removed /Library/LaunchDaemons/homebrew.mxcl.dnsmasq.plist)
✓ resolver script installed
✓ sudoers rule installed
✓ loopback addresses 127.0.1.1–127.0.1.50 on lo0
✓ no /etc/resolver/local
✓ dnsmasq runs as you on port 53535
✓ Caddy serves lcd's sites on port 80
✓ sudo runs the resolver script without a password
Left alone, from LocalDNS (delete them by hand when you no longer need it):
  /Users/sang/.config/localdns
  /Users/sang/Library/LaunchAgents/com.localdns.caddy.plist
  /Users/sang/Library/Application Support/LocalDNS

✓ 1 dnsmasq              running as sang, pid 35242
✓ 2 dnsmasq config       includes /Users/sang/.config/lcd/dnsmasq.conf
✓ 3 loopback             50 of 50 addresses on lo0
✓ 4 resolvers            0 files
✓ 5 resolver for .local  absent
✓ 6 generated files      match domains.json
✓ 7 names resolve        no enabled domains
– 8 caddy                no enabled domain has a port
```

Installed as designed: the script `root:wheel 0755`, the sudoers rule `root:wheel 0440`, the plist
`root:wheel 0644`; the root dnsmasq job gone; dnsmasq and Caddy run as `sang` under Homebrew's `sh.brew.*`
labels; dnsmasq listens on `127.0.0.1:53535`; `sudo -n -k -l <script>` succeeds without a password.

## Names

`domains.json` held `crm.local`, `api.crm.local`, `flowy.local`, `test.flowy.local` and
`dashboard.flowy.local` (port 3000). macOS loaded every `/etc/resolver` file with port 53535; the `# oo`
comment line is accepted.

| Name                    | Labels | macOS lookup     | Reached dnsmasq |
| ----------------------- | ------ | ---------------- | --------------- |
| `api.crm.local`         | 3      | 127.0.1.1, 20 ms | yes             |
| `test.flowy.local`      | 3      | 127.0.1.3, 20 ms | yes             |
| `foo.flowy.local`       | 3      | 127.0.1.3, 20 ms | yes, unlisted   |
| `crm.local`             | 2      | none, 5–10 s     | no              |
| `flowy.local`           | 2      | none, 5–10 s     | no              |

**Finding:** macOS 27 sends a `.local` name with one label before `.local` to Bonjour only and ignores its
`/etc/resolver` file; names with two or more labels before `.local` follow the resolver file, including
unlisted subdomains. `dscacheutil`, Python's `getaddrinfo` and dnsmasq's query log agree. The flowy README
recipe (`/etc/resolver/flowy.local`) has the same problem.

## Ports through Caddy

- `curl http://dashboard.flowy.local/` with a test server on `127.0.1.3:3000` answered through Caddy
  (`Via: 1.0 Caddy`).
- With the route pointed at a free port, `oo apply` reports `app down: nothing answers on 127.0.1.3:3998`.
- A Node app listening on `*:3000` answers on every loopback address, so it shadows a project's port 3000.
  Apps share a port across projects only when each binds its own address (`next dev -H 127.0.1.3`).

**Fixed during the test:** right after a Caddy restart the HTTP probe reported "is Caddy running?" because
Caddy had not bound port 80 yet. The probe now retries refused connections for up to 3 s.

## Before setup

Running `oo apply` before `oo setup` started a user dnsmasq against LocalDNS's config, which crash-looped
on port 53. It was stopped with `brew services stop dnsmasq`; Homebrew had replaced the unused
`~/Library/LaunchAgents/homebrew.mxcl.dnsmasq.plist` with `sh.brew.dnsmasq.plist` and then removed it.
`apply` now refuses to run until setup has.

## Decision

Names end in `.oo` (`store.TLD`), Sang's choice after the finding above. `.dev` was ruled out first: the whole
TLD is on the HSTS preload list (hstspreload.org reports `dev` as preloaded) and `flowy.dev` is a registered
domain. Public DNS answers `flowy.oo` with NXDOMAIN; `.oo`, `.internal` and `.test` have no public
nameservers.

## Round trip with `.oo`

Sang ran `oo uninstall`, `oo doctor` and `oo setup` with `domains.json` holding `crm.oo`, `api.crm.oo`,
`flowy.oo`, `test.flowy.oo` and `dashboard.flowy.oo` (port 3998).

- `uninstall`: every step passed. It removed the resolver files, loopback addresses, sudoers rule and script;
  stopped dnsmasq and Caddy; restored `dnsmasq.conf` from the backup; removed oo's `Caddyfile`.
- `doctor` in between: checks 1–4 and 6–8 failed, check 5 passed, as expected for a Mac without oo.
- `setup` again: every step passed and made a fresh backup from the restored `dnsmasq.conf`.

| Name                  | Labels | macOS lookup     | Through apps |
| --------------------- | ------ | ---------------- | ------------ |
| `crm.oo`             | 2      | 127.0.1.1, 17 ms | 5 ms         |
| `flowy.oo`           | 2      | 127.0.1.3, 18 ms | 1 ms         |
| `api.crm.oo`         | 3      | 127.0.1.1, 15 ms |              |
| `test.flowy.oo`      | 3      | 127.0.1.3, 16 ms | 1 ms         |
| `foo.flowy.oo`       | 3      | 127.0.1.3, 70 ms | unlisted     |
| `unknown.oo`         | 2      | none, 0.3 s      | not listed   |

With a test server on `127.0.1.3:3998`, `curl http://dashboard.flowy.oo/` answered through Caddy
(`Via: 1.0 Caddy`), `oo apply` showed all five names passing, and `oo doctor` passed all eight checks.

Two-label `.oo` names work, so the hand test is done.
