# serialtap

**A zero-touch USB serial log collector for Linux.** Plug in a device — serialtap
detects it, opens the port once, and keeps timestamped logs rolling with an
error-signature event stream. Built for ESP32 fleet debugging, works with any
USB serial adapter (CH340/CH343/CP210x/FTDI/native USB-CDC…).

A single Go binary: plug in a device → auto-detected → continuous capture →
dual-channel logs (full stream + error events) → offline analysis (signature
tally / Backtrace addr2line decoding). On macOS, `run` lives in the menu bar
(pause/resume/open logs/open web panel/quit); a built-in web observation
panel serves at `127.0.0.1:8801` by default.

[![CI](https://github.com/mickeyzzc/serialtap/actions/workflows/ci.yml/badge.svg)](https://github.com/mickeyzzc/serialtap/actions/workflows/ci.yml)

**English** | [简体中文](README.zh-CN.md)

## Documentation

| Document | Contents |
|---|---|
| [CLI Reference](docs/en/cli-reference.md) | Every subcommand, flag, exit code, and device-matching pattern semantics |
| [Configuration](docs/en/configuration.md) | Every config field with defaults, naming rules, `elf_map`, `esptool_cmd` |
| [Architecture](docs/en/architecture.md) | Package layout, design decisions (open-once-and-hold, by-path identity, zero-loss assembly), test strategy |
| [Control Protocol](docs/en/control-protocol.md) | The daemon's JSON-line unix socket API — script `status`/`release`/`flash` yourself |
| [Deployment](docs/en/deployment.md) | Install, serial permissions, systemd user service, troubleshooting |

## Features

- **Hot-plug auto capture**: polls for USB serial ports (1 s), one collector
  goroutine per device — start logging on plug-in, stop on unplug
- **macOS menu-bar tray**: `run` lives in the menu bar by default — live
  device status, pause/resume all, open log folder, open the web panel, quit
  (`--no-tray` for headless; Linux builds stay pure static binaries)
- **Web observation panel**: served by `run` at `127.0.0.1:8801` by default
  (`--web off` or `web_addr` config to change) — live serial tail (SSE),
  event stream, log browsing, pause/resume/proxy actions
- **Layered self-healing**: `reopen` (serial-layer soft reconnect, skips
  backoff) and `reset` (Windows USB-layer soft replug via pnputil) recover
  from wedged ports/drivers without physical replugging
- **Stable identity**: the physical USB port (by-path) is the device identity —
  re-enumeration changing ttyUSB numbers doesn't matter, and identical adapters
  (same-model CH340s with serial-less by-id) don't collide either
- **Dual-channel logs**: `serial-DATE.log` full stream (millisecond per-line
  timestamps) + `events-DATE.log` event stream (error-signature hits +
  collector lifecycle), rotated by day and by size, pruned after the retention
  period
- **Error signature engine**: built-in ESP-IDF fault lines (`rst:0x` reset
  banner, `E (` error-level logs, lwIP `accept (n)`, Guru Meditation, WDT,
  Backtrace…), freely extensible with regexes
- **Backtrace decoding**: `decode-backtrace` extracts address frames and hands
  them to addr2line for `file:line` translation; auto-discovers the ESP-IDF
  toolchain
- **Flashing safety gate**: `pause`/`resume` pause list, `release` temporarily
  yields a port (auto re-acquire when idle), and `flash` proxies firmware
  flashing — the daemon orchestrates over a unix socket control channel:
  yield port → esptool → resume capture, so esptool and the resident collector
  never fight over the port and wedge the ESP32 into ROM download mode
- **Zero loss**: lines are assembled across read chunks; a trailing half-line
  at port close is flushed to disk marked `…partial`
- **Pure Go static binary**: no CGO, no libudev, all dependencies vendored —
  builds offline, cross-compiles, copy-and-run

## Quick start

```bash
# Option 1: download a prebuilt binary from the Releases page (linux-amd64/arm64, built on tag push)
# Option 2: build from source
git clone https://github.com/mickeyzzc/serialtap && cd serialtap
make build                 # or: go build .
./serialtap list           # see current devices: tty / name / VID:PID / by-id / physical port
./serialtap run            # daemon mode (macOS: menu-bar tray included)
```

The macOS tray build needs clang (Xcode Command Line Tools); `go build .`
enables CGO by default. `CGO_ENABLED=0` builds a tray-less variant
(enumeration/capture unaffected); released Linux binaries stay pure static.

## Commands

| Command | Purpose |
|---|---|
| `run` | Daemon mode: poll for USB serial ports, one collector per device; macOS lives in the menu bar (`--no-tray` disables), `--web ADDR` configures the panel |
| `attach TTY [--name N]` | Capture a single port (manual watching/testing, works with a socat PTY) |
| `list` | List current devices and their identities |
| `pause [RE]` / `resume [RE]` | Pause/resume capture (omitted = all) |
| `proxy RE [--stop]` | **Transparent USB proxy**: opens a TCP endpoint per matched device — business software treats it as direct serial; capture continues (`proxy_tap_exclude` config drops high-rate telemetry from the full log) |
| `release RE [--for 5m]` | **Temporarily yield a port** to an external tool: re-acquired automatically after 3 idle seconds by default, or after the given duration |
| `flash RE <bin>[@0x10000]...` | **Proxy firmware flashing**: yield port → esptool → auto-resume, progress streamed back; `--args-file build/flasher_args.json` flashes a full ESP-IDF image set in one go. RE is a regex; multiple matches flash **one by one** — anchor it (e.g. `^board$`) to flash exactly one board |
| `reopen RE [--all]` | **Serial-layer soft reconnect**: close port → reopen immediately, skipping backoff. Fast self-heal for wedged ports; interrupts live proxy sessions (clients just reconnect) |
| `reset RE [--all]` | **USB-layer soft replug** (Windows): yield port → restart the device node via pnputil (needs admin; UAC auto-elevation) → re-enumerate → resume |
| `status` | Live daemon and device state (collecting/paused/suspended/flashing) |
| `tray` (Windows) | Resident tray: per-device pause/resume, open logs, open web panel |
| `analyze LOG...` | Offline signature scan: counts / first-last times / sample lines summary table |
| `decode-backtrace LOG` | addr2line decoding of `Backtrace:` address frames |

Flags (`--root/--baud/--config/--poll-ms/--exclude`) may appear before or after
positional arguments. Full details: [CLI Reference](docs/en/cli-reference.md).

## Reset semantics (read this)

**Both opening and closing a USB serial device deliver a reset pulse to the
board** — including the ESP32-S3 native USB-JTAG port (the USB_SERIAL_JTAG
peripheral implements the same auto-reset semantics as a CH340 in silicon;
measured `rst:0x15 (USB_UART_CHIP_RESET)` 3 ms after open, and a reboot after
close). This is host-side CDC handshake-line behavior and cannot be avoided
from userspace.

So serialtap's discipline is **open each physical device exactly once and hold
it** — never re-open periodically:

- Hot-plug attach: the device just powered up, this pulse is harmless
- `pause` (before flashing): resets the board — fine, esptool was going to
  reset it anyway
- The silent watchdog (`silent_reopen_s`) is **off by default**: enable it only
  for devices guaranteed to emit periodic output (e.g. a 30 s heartbeat);
  otherwise legitimately quiet devices get caught in a reset loop

## Log layout

```
<root>/<device>/serial-YYYYMMDD.log     # full stream, each line prefixed [YYYY-MM-DD HH:MM:SS.mmm]
<root>/<device>/serial-YYYYMMDD.001.log # rotated after exceeding rotate_max_mb
<root>/<device>/events-YYYYMMDD.log     # event stream: signature hits + collector lifecycle
<root>/PAUSED                            # pause list (one regex per line, hot-reloaded on mtime)
```

## Device naming

Priority: config `names` (by-id regexes) → built-in rules (`ch340` / `ch343` /
`esp32s3-jtag`) → by-id base name → tty name. Same-named devices get a `-2`
suffix automatically. Use the serial number/MAC embedded in by-id to pin
specific boards among identical chips in your config (see
`config.example.json`).

The default config path is `~/.config/serialtap/config.json` (used only if it
exists; otherwise all defaults). All fields: [Configuration](docs/en/configuration.md).

## Offline analysis

```bash
./serialtap analyze logs/esp32s3-jtag/serial-*.log
# SIGNATURE            COUNT  FIRST                   LAST                     SAMPLE
# reset-banner             3  2026-09-20 11:11:21.165 2026-09-20 11:13:34.229 ...

./serialtap decode-backtrace logs/esp32s3-jtag/serial-20260920.log
# addr2line: ~/.espressif/tools/.../xtensa-esp32s3-elf-addr2line
# elf:       picked from config elf_map by device name (or pass --elf explicitly)
```

## systemd deployment (user service)

```bash
mkdir -p ~/.local/bin ~/.config/serialtap ~/.config/systemd/user
cp serialtap ~/.local/bin/
cp config.example.json ~/.config/serialtap/config.json   # adjust root/names/elf_map
cp deploy/serialtap.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now serialtap
journalctl --user -u serialtap -f          # follow runtime logs
loginctl enable-linger $USER               # start at boot without logging in
```

Serial permissions: on Linux your user must be in the `uucp` (Arch) or
`dialout` (Debian-family) group. Full guide: [Deployment](docs/en/deployment.md).

## Development

```bash
make test      # go test -race
make cover     # coverage (CI enforces an 80% gate)
make lint      # golangci-lint (config in .golangci.yml)
make fmt       # gofmt
```

Code layout (`internal/` packages, one-way dependencies, clean boundaries):

```
main.go                    # thin entry (just os.Exit(cli.Run(...)))
internal/cli/              # subcommand dispatch and flag parsing (orchestration layer)
internal/config/           # config definition and loading
internal/device/           # device discovery and stable identity (by-path key / by-id naming / sysfs)
internal/collector/        # per-device collector (open-once-and-hold, injectable Port seam)
internal/logstore/         # dual-channel log writing / rotation / retention sweep
internal/signature/        # error signature engine
internal/pause/            # flash pause list
internal/daemon/           # hot-plug daemon loop (enumerate diff + start/stop collectors + release/flash orchestration)
internal/flash/            # proxy firmware flashing (esptool orchestration + flasher_args.json parsing)
internal/ctl/              # control unix socket (JSON-line protocol)
internal/analyze/          # offline analysis (signature tally + addr2line decoding)
internal/testutil/         # cross-package test helpers (fake serial ports, etc.)
```

- TDD; fake-port injection drives full-chain offline tests of the collector,
  plus socat PTY end-to-end tests
- Dependencies vendored via `go mod vendor`: `go.bug.st/serial` (the same
  serial library used by arduino-cli, pure Go)
- Go 1.27+

Design decisions in depth: [Architecture](docs/en/architecture.md).

## License

GPL-3.0-or-later, see [LICENSE](LICENSE).
