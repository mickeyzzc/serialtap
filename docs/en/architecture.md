# Architecture

[English](architecture.md) | [简体中文](../zh-CN/architecture.md)

serialtap is a daemon for Linux, macOS and Windows that turns "a pile of USB
serial boards on one host" into "a directory of continuously captured,
timestamped, rotatable logs" — without ever fighting other tools for the
ports. This document explains how it is built and, more importantly, *why*
the tricky parts are the way they are.

## Package layout

Dependencies are one-way, from orchestration down:

```
main.go                    thin entry: os.Exit(cli.Run(...))
   │
internal/cli/              subcommand dispatch, flag parsing, command orchestration
   ├── internal/config/        config struct + JSON loading
   ├── internal/device/        discovery + stable identity (Linux sysfs by-path / Windows registry / macOS ioreg)
   ├── internal/daemon/        hot-plug loop: enumerate diff, start/stop collectors, release/flash/reopen/reset orchestration
   │     ├── internal/collector/   per-device goroutine: open-once-and-hold, line assembly, suspend/resume/manual reopen
   │     ├── internal/logstore/    dual-channel files, rotation, retention sweep
   │     ├── internal/signature/   error-signature engine
   │     ├── internal/flash/       esptool command building/execution, flasher_args.json parsing
   │     └── internal/pause/       PAUSED file + pattern state
   ├── internal/ctl/           control socket server/client (JSON lines; AF_UNIX on Windows)
   ├── internal/web/           observation & operations panel (SSE live tail, flash upload, cmd forwarding)
   ├── internal/tray/          tray/menu bar (darwin+cgo embedded in run; Windows systray process; stubs elsewhere)
   └── internal/analyze/       offline: signature tally + addr2line decoding

internal/testutil/          cross-package test helpers (fake serial ports, wait/read helpers)
```

## Data flow (daemon mode)

```
every poll_interval_ms:
  Enumerate()                       ← /dev/serial/by-{id,path} + sysfs VID:PID
    diff against live collectors (by by-path key)
      new key      → create DeviceWriter + Collector goroutine
      missing key  → stop collector, release its name
  reload PAUSED file if mtime changed
  process release expirations (timed / idle-detected)

Collector goroutine (per device):
  open port (once) → release DTR/RTS → read loop
    lines completed by '\n' → WriteLine to serial channel + signature match
    signature hit            → WriteEvent to events channel
  on error/pause/suspend: flush partial line, close port, back off, retry
```

## Stable identity: the physical port, not by-id or tty name

tty numbers are assigned in enumeration order — they change whenever a device
is replugged or a neighbor disappears. by-id is stable *only if the adapter
has a serial number*: two identical CH340s have byte-identical by-id strings,
so by-id alone cannot tell two same-model boards apart.

serialtap therefore keys devices by the **physical USB port**, with one
discovery path per platform:

| Platform | key source | by-id shape |
|---|---|---|
| Linux | sysfs `/dev/serial/by-path` | `usb-Espressif_USB_JTAG_...` |
| Windows | registry USB instance ID (composite devices resolve PortName under the `&MI_00` interface subkey) | `USB\VID_303A&PID_1001\...` instance path (embeds the MAC) |
| macOS | USB `locationID` parsed from `ioreg` | `usb-<vid>_<pid>[-<serial>]` (style aligned with Linux) |

A device's identity survives re-enumeration, and two identical adapters in two
different ports are two different keys. by-id remains the *display/naming*
identity (it is human-readable and can embed serials/MACs), which is why
config `names` rules carry across platforms. On Linux, discovery requires a
by-path entry to exist, which also filters out non-USB serial ports
(mainboard `ttyS*`); Windows only accepts USB serial ports from the registry;
macOS only enumerates `/dev/cu.usb*`.

On Linux, VID:PID are read from sysfs by walking up from
`/sys/class/tty/<tty>/device` (at most 4 levels) to the directory holding
`idVendor`/`idProduct`.

## Reset semantics: open-once-and-hold

**Opening or closing a USB serial device pulses the board's reset line.** For
adapter chips (CH340/CH343…) the driver asserts the reset line on open; the
ESP32-S3's native USB_SERIAL_JTAG peripheral implements the same auto-reset
semantics in silicon (measured: `rst:0x15 (USB_UART_CHIP_RESET)` 3 ms after
open; reboot after close). This is host-side CDC handshake behavior; it cannot
be suppressed from userspace (pyserial's `rts=False/dtr=False` does not help).

The consequences, codified in the collector:

1. **Open each physical device exactly once and hold it.** Never re-open
   periodically. Hot-plug attach happens right after power-up, so that pulse
   is harmless; `pause` before flashing resets the board, which esptool was
   going to do anyway.
2. **Release DTR/RTS immediately after open.** On CH340-wired boards RTS
   connects to EN; leaving it asserted holds the board in reset with zero
   bytes of output. Other boards are unaffected.
3. **The silent watchdog is off by default.** One failure mode of a wedged fd
   is `read` returning 0 forever without error; a watchdog forcing a reopen
   after N silent seconds recovers it — but it also resets any legitimately
   quiet board every N seconds. Hence `silent_reopen_s = 0` unless you know
   the device emits periodic output (e.g. a 30 s heartbeat).
4. **On read error, drop the fd immediately.** USB re-enumeration surfaces as
   a read error; clinging to a dead fd would miss the device's reboot — the
   most expensive diagnostic data to lose. Reconnects back off exponentially
   (`reopen_min_s` doubling up to `reopen_max_s`); the backoff resets after
   any session that opened successfully, so one flapping neighbor doesn't
   ratchet you to the cap.

## Zero-loss line assembly

Serial data arrives in arbitrary chunks that ignore line boundaries. The
collector's line assembler keeps a tail buffer across reads and emits a line
only when `\n` arrives (`\r` stripped). When the port closes with a partial
line pending, the tail is flushed to disk prefixed `…partial` so nothing is
silently dropped. A pathological stream with no newline for 64 KiB is dumped
as one (huge) line rather than buffering unboundedly.

## Dual-channel logs, rotation, retention

Per device, under `<root>/<name>/`:

- `serial-YYYYMMDD.log` — every line, prefixed `[YYYY-MM-DD HH:MM:SS.mmm]`
- `events-YYYYMMDD.log` — signature hits (`[sig] first 200 chars`) plus
  collector lifecycle events (`[collector <name>] …`)

Rotation: date change opens a new pair; size exceeding `rotate_max_mb` adds a
`.001`, `.002`… suffix (both channels). After a daemon restart, writing
resumes at the existing day file and the suffix counter continues from the
highest existing index, so restarts never collide with earlier rotations.
Retention sweeps (startup + hourly) delete `*.log` files older than
`retention_days` by mtime.

## Pause / release / flash orchestration

Three levels of "not collecting", all built so external tools can use the
port safely:

1. **PAUSED file** (`<root>/PAUSED`, one regex per line). Pattern-level,
   persistent across restarts, hot-reloaded on mtime change. Collectors close
   the port and poll until the pattern goes away.
2. **release** (`Suspend`/`Resume` on the collector). Programmatic, per
   device, via the control socket. `Suspend` does not return until the port
   is *actually closed* (or times out), so the caller may open it
   exclusively. Re-acquisition is either timed (`--for`) or idle-triggered:
   the daemon scans `/proc/*/fd` for other holders of the tty and resumes
   after 3 continuous idle seconds — the scan only reads symlinks and never
   touches the port itself, so probing emits no reset pulse.
3. **flash** — the daemon chains the primitives per matched device, one at a
   time: `Suspend` (10 s budget) → mark `flashing` → run esptool (its stdout
   and stderr are streamed back over the control socket; `\r` counts as a
   line boundary so progress bars come through) → `Resume`. On esptool
   failure the collector is still resumed; the client retries automatically
   (default 3 attempts × 5 s, absorbing Windows CDC first-open flakiness).
   A pattern matching several devices is **refused by default** with the
   device names listed (anti-misflash protection); `--all` opts into
   one-by-one execution.

`resume` (control command) clears matching PAUSED entries *and* revokes
outstanding releases in one shot.

## proxy / reopen / reset: passthrough and two layers of self-heal

- **proxy (transparent passthrough)**: suspend the exclusive hold (capture
  logic keeps running) → open a 127.0.0.1 TCP endpoint per device bridging
  both directions → capture continues meanwhile (tap mode mirrors both
  directions into the full log; per-line regexes can keep high-rate telemetry
  out). Endpoint ownership semantics: what sharer sessions need survives the
  owner stopping; endpoints close automatically on unplug/daemon exit.
- **reopen (serial-layer self-heal)**: close the port now → the collector
  loop exits on read error → reopen **skipping the backoff**. This is the
  manual exception path (see reset semantics); multi-device gate as `flash`.
- **reset (USB-layer self-heal, Windows only)**: yield the port → restart
  the device's serial interface node via `pnputil /restart-device` (a
  software replug) → verify re-enumeration with the built-in enumerator →
  resume. Needs admin: an unelevated daemon pops UAC to retry; pnputil can
  exit 0 even on failure (measured on Win11), so success is decided by
  output markers + an enumeration re-check, never the exit code.

## Web panel

`internal/web` rides the **same handler path** as ctl: panel buttons →
`/api/cmd` (allowlist shared with ctl) → daemon methods — no second control
logic. Observation uses SSE log tailing (incremental pushes, follows
rotation); flashing goes through the dedicated `/api/flash` upload endpoint
(images+offsets or a flasher_args.json → stored under `root/.flash-upload/`
→ the same daemon orchestration → esptool output streamed back over SSE with
history replay). The panel listens on loopback only, same trust domain as
the ctl socket, and **contains no business logic**.

## Control socket

A unix stream socket (AF_UNIX on Windows, Win10 1803+) carrying one JSON
object per line in both directions; see
[control-protocol.md](control-protocol.md). The socket is mode 0600 —
same-user only. Default paths differ per platform (Linux
`$XDG_RUNTIME_DIR` → `/tmp` fallback; Windows
`%LOCALAPPDATA%\serialtap\serialtap.sock`). A second `serialtap run` refuses
to start while a live instance holds the socket; a stale file from a crash
is cleaned up automatically.

## Testing strategy

- **Port seam**: `collector.OpenPort` is a package-level variable; tests swap
  it for `testutil.FakePort` (scripted chunks, close/DTR/RTS tracking), so
  the entire read→assemble→write→signature chain runs offline, without
  hardware.
- **Injected boundaries**: the daemon's enumerate function and logger are
  constructor parameters — tests feed synthetic device lists; logstore tests
  cover rotation and retention on a temp dir.
- **Fuzzing**: the line assembler and the Backtrace address parser have fuzz
  targets.
- **End-to-end**: socat PTY pairs exercise a real collector against a real
  (virtual) port on Linux.
- CI enforces golangci-lint, `go test -race`, and an 80% coverage gate.

## Platform support

All three platforms are first-class: device discovery goes through native
sources (Linux sysfs / Windows registry / macOS ioreg) and the control
channel is a unix socket everywhere (AF_UNIX on Windows, Win10 1803+). The
platform differences concentrate in a few capabilities (see the platform
table in the README):

- `reset` (USB-layer soft replug) is Windows-only — it depends on `pnputil`
  restarting the device node
- release's idle auto re-acquisition depends on port-holder detection
  (Linux `/proc/*/fd`, macOS `lsof`); Windows has no equivalent, so use
  `--for` timed re-acquisition or a manual `resume` there
- tray shapes differ: macOS embeds the menu bar in `run` (darwin+cgo);
  Windows uses the separate `serialtap tray` process; Linux has a GUI-less
  stub and relies on the web panel
- release artifacts (CI on `v*` tags): Linux tar.gz (amd64/arm64), macOS
  universal .app+DMG, Windows Inno installer + portable zip

Windows engineering conclusions (measured): on AF_UNIX, `conn.Close()` does
not abort an in-flight `Read` and `connect()` to a closed listener blocks
forever — the ctl server's Close actively disconnects accepted connections
with a bounded wait; USB-CDC devices often fail the first
open/SetCommState after a reset — the flash client retries to absorb it.
