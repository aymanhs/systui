# jeeves

> _"You rang, sir?"_ — your terminal-native butler for systemd.

A fast, keyboard-driven TUI for systemd. Browse every unit on the box, watch
logs live, inspect unit files, restart things — without leaving the terminal
and without typing `sudo systemctl restart <tab><tab><tab>` ever again.

![screenshot placeholder](docs/screenshot.png)

---

## Why?

`systemctl` is a great _command_. It's a lousy _interface_.

You want to know what's running, what's failed, what's eating RAM, what
`nginx.service` actually says, and why it died at 03:14 last night. You shouldn't
need eight terminal windows and a copy-paste pipeline through `awk` to find out.

**jeeves** is one screen, one keymap, and no surprises.

---

## Features

- 🚦 **Browse every service** — color-coded state, sub-state, enablement,
  description. Filter as you type with `/`. Cycle views with `a` (all /
  running / failed). Sort with `S` (name / state / memory / PID).
- 🔎 **Drill in** — split-panel detail view with properties on one side and
  live `journalctl` on the other. Memory, CPU, tasks, traffic, and uptime
  refresh every 2 seconds automatically.
- 📜 **Logs that don't lie** — wrap toggle (`w`), tail-line cycling (`[` / `]`,
  100 → 2,500), full scrollback (PgUp/PgDn/g/G), and a true follow mode (`f`)
  that streams `journalctl -f` straight into the panel without you reaching
  for another terminal.
- 📄 **Inspect the unit file** — press `u` for a full-screen, copy-pasteable
  view of the on-disk service file. No borders, no styling — just the bytes.
- 🛟 **Safety net** — destructive actions (`stop`, `restart`, `disable`,
  `mask`) ask `[y/N]` before they fire. You won't kill `sshd` with a stray
  keystroke.
- 🐭 **Mouse-friendly** — scroll wheel works in every viewport. Use it or
  don't; jeeves doesn't care.
- 🏠 **System or user bus** — `--user` for your own units, default for the
  system bus. Auto-falls-back when D-Bus says no.

---

## Install

```bash
go install github.com/aymanhs/jeeves@latest
```

Or build from source:

```bash
git clone https://github.com/aymanhs/jeeves
cd jeeves
go build -o jeeves .
```

Requirements: Linux, systemd, `journalctl` on `$PATH`. That's it.

---

## Use

```bash
# Manage system units (most of what you care about — needs root for actions)
sudo jeeves

# Manage your user units (no sudo)
jeeves --user
```

### Keymap

#### Service list

| Key            | Action                                    |
|----------------|-------------------------------------------|
| `↑/↓` `j/k`    | Navigate                                  |
| `Enter` `→` `l`| Open details                              |
| `/`            | Filter as you type                        |
| `a`            | Cycle view: all → running → failed        |
| `S`            | Cycle sort: name → state → memory → PID   |
| `s` `t` `r`    | Start / Stop / Restart                    |
| `e` `d`        | Enable / Disable                          |
| `R`            | Refresh                                   |
| `q` `Ctrl+C`   | Quit                                      |

#### Detail view

| Key          | Action                                          |
|--------------|-------------------------------------------------|
| `Tab`        | Switch focus: properties ↔ logs                 |
| `u`          | Open unit file (full screen)                    |
| `f`          | Follow logs (live tail)                         |
| `w`          | Toggle log line wrap                            |
| `[` / `]`    | Decrease / increase log tail length             |
| `s/t/r/e/d`  | Service actions (destructive ones prompt y/N)   |
| `↑/↓ PgUp/PgDn g/G` | Scroll logs                              |
| `R`          | Refresh                                         |
| `Esc` `←` `h`| Back to list                                    |

#### Unit-file view

Plain text, mouse-selectable, copy-pasteable. `R` to reload,
`Esc`/`u`/`←` to go back.

---

## Status

Single-binary, no config file, no daemon, no telemetry. Talks to systemd via
D-Bus directly (no shelling out to `systemctl` for state — only for
`journalctl`). Tested on Debian, Ubuntu, Arch, Fedora.

---

## License

MIT. See [LICENSE](LICENSE).
