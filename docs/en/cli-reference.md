# CLI Reference

[English](cli-reference.md) | [简体中文](../zh-CN/cli-reference.md)

All commands live in the single `serialtap` binary. Run `serialtap` with no
arguments to print the built-in usage banner (which also shows the version).

## Conventions

**Exit codes**

| Code | Meaning |
|---|---|
| `0` | success |
| `1` | runtime error (message printed to stderr, prefixed `错误:`) |
| `2` | usage error: no/unknown subcommand |

**Flags may appear anywhere.** Go's `flag` package stops parsing at the first
positional argument, but serialtap loops around that, so the natural form
`attach /dev/ttyACM0 --name x` works, as does `attach --name x /dev/ttyACM0`.

**Config resolution** (for commands that accept `--config`/`--root`/`--baud`):

1. `--config FILE` if given; otherwise `~/.config/serialtap/config.json`, but
   only if it exists
2. if the file does not exist, built-in defaults are used
3. `--root` / `--baud` flags override the corresponding config values

See [configuration.md](configuration.md) for every field.

**Device patterns (`RE`) are regexes.** Wherever a command takes a device
pattern, it is a Go regular expression matched against any of the device's
identities: tty path (`/dev/ttyUSB0`), device name (`ch340`, `board-a`…),
by-path key (physical USB port), or by-id string. **An unanchored regex
matches by substring** — `usb` matches every ttyUSB device. Use anchors
(`^board-a$`) when you need exactly one device. This matters most for `flash`,
which flashes every match one by one.

---

## `run` — daemon mode

```
serialtap run [--config F] [--root DIR] [--baud N] [--exclude RE]... [--poll-ms N] [--sock PATH]
```

Polls for USB serial ports every `poll_interval_ms`, starts one collector
goroutine per device, stops it on unplug. Also:

- serves the **control socket** used by `status`/`pause`/`resume`/`release`/
  `flash` (see [control-protocol.md](control-protocol.md))
- sweeps expired logs at startup and then hourly (`retention_days`)
- reloads the `PAUSED` file whenever its mtime changes

Flags:

| Flag | Meaning |
|---|---|
| `--config F` | config file (see resolution order above) |
| `--root DIR` | log root directory (overrides config) |
| `--baud N` | baud rate (overrides config) |
| `--exclude RE` | ignore devices matching RE; may be repeated; **added to** the config's `exclude` list |
| `--poll-ms N` | device poll interval in ms (overrides config) |
| `--sock PATH` | control socket path. Default: config `control_socket`, else `$XDG_RUNTIME_DIR/serialtap.sock`, else `/tmp/serialtap-<uid>.sock` |

If the socket path is already held by a **live** serialtap instance, startup is
refused (a second daemon cannot steal the control channel). A stale socket file
left over from a crash is removed automatically.

`SIGINT`/`SIGTERM` stop all collectors (closing the ports) and exit cleanly.

## `attach TTY` — single-port capture

```
serialtap attach TTY [--name N] [--config F] [--root DIR] [--baud N]
```

Captures exactly one port, in the foreground — handy for manual watching or
testing (point it at a socat PTY to test without hardware). Writes the same
dual-channel log layout under `<root>/<name>/`. The device name defaults to
the tty basename (`ttyACM0`) unless `--name` is given. Honors the `PAUSED`
file like the daemon does. `Ctrl-C` stops it.

## `list` — show devices

```
serialtap list [--config F] [--root DIR]
```

Enumerates current USB serial devices and prints:

```
TTY             NAME            VID:PID   BY-ID                                       BY-PATH(key)
/dev/ttyUSB0    ch340           1a86:7523 usb-1a86_USB_Serial-if00-port0             pci-0000:00:14.0-usb-0:2:1.0-port0
```

The BY-PATH column is the stable device key; use it (or the name) in patterns.
Non-USB serial ports (e.g. mainboard `ttyS*`) are not listed — serialtap only
manages ports that have a `/dev/serial/by-path` entry.

## `status` — daemon state

```
serialtap status [--sock PATH]
```

Asks the running daemon (over the control socket) for live per-device state:

```
NAME             TTY            STATE      KEY
board-a          /dev/ttyACM0   collecting usb-Espressif_USB_JTAG_...-if00
```

States: `collecting` | `suspended` (port yielded via `release`/PAUSED) |
`flashing` (proxy flash in progress) | `paused` (matched by a PAUSED entry).
Fails with a connection error if the daemon is not running.

## `pause [RE]` / `resume [RE]` — pause list

```
serialtap pause [RE] [--sock PATH] [--root DIR]
serialtap resume [RE] [--sock PATH] [--root DIR]
```

Pause temporarily stops matching collectors: they close the port and wait
until the pattern is removed (this is the safe gate before flashing with an
external tool). The list is the file `<root>/PAUSED`, one regex per line
(`#` comments allowed), hot-reloaded by the daemon on mtime change.

- pattern omitted on `pause` → pause **all** devices (writes `.*`)
- pattern omitted on `resume` → clear the whole list (file removed)
- `resume RE` removes entries equal to `RE` from the list

If the daemon is running, these commands go through the control socket
(effective immediately, same file semantics); if not, they edit the `PAUSED`
file directly, and the daemon (when started later) picks the file up.

## `release RE` — temporarily yield a port

```
serialtap release RE [--for 5m] [--sock PATH]
```

Asks the daemon to suspend matching collectors and **wait until the port is
really closed**, so an external tool (idf.py monitor, minicom, your own
script…) can open it exclusively. Requires the daemon.

- default: the daemon watches `/proc/*/fd` and re-acquires the port after it
  has been **idle for 3 continuous seconds** (the external tool closed it)
- `--for 5m`: re-acquire after a fixed duration instead (`90s`, `12h`… any Go
  duration)
- `resume` cancels outstanding releases immediately

This yields the port without killing the daemon; the collector just waits.

## `flash RE ...` — proxy firmware flashing

```
serialtap flash RE <image>[@<offset>]... [--args-file F] [--esptool CMD] [--baud N] [--chip C] [--sock PATH] [--config F]
```

One command for the full cycle — the daemon, per matched device **in
sequence**: suspend collector and wait for the port to close → run esptool
(streaming its output back line by line) → resume capture. Only one
collector-side port is open at a time, so esptool never races the collector.

Image specification (either form):

- positional: `firmware.bin@0x10000 bootloader.bin@0x0` — offset defaults to
  `0x0` when `@offset` is omitted; offsets are hex
- `--args-file build/flasher_args.json`: ESP-IDF's generated args file; all
  `flash_files` images are flashed (paths resolved relative to the file's
  directory, sorted by offset). Takes precedence over positional images

Flags:

| Flag | Meaning |
|---|---|
| `--esptool CMD` | esptool executable. Resolution: flag → config `esptool_cmd` → `esptool` on PATH → `esptool.py` |
| `--baud N` | flash baud rate. Resolution: flag → config `flash_baud` → esptool default |
| `--chip C` | chip type (e.g. `esp32s3`); omitted → taken from `flasher_args.json` if present, else esptool auto-detect |
| `--sock PATH` | control socket path |

Output streams through until the final completion line (`✓ 刷写完成，已恢复采集`
— yes, the binary speaks Chinese) or the failure message; on failure the
collector is still resumed. Multiple matches flash one by one — anchor the
pattern (`^board$`) to flash exactly one board.

## `analyze LOG...` — offline signature tally

```
serialtap analyze LOG... [--lines]
```

Scans one or more log files with the **built-in** signature set (config
`signatures_extra` is not applied offline) and prints a per-signature summary:
hit count, first/last timestamp, and one sample line, sorted by count
descending. `--lines` additionally prints every matched line. First/last
timestamps are correct min/max across files regardless of argument order.

## `decode-backtrace LOG` — addr2line decoding

```
serialtap decode-backtrace LOG [--elf F] [--addr2line BIN] [--config F]
```

Extracts every `Backtrace:` line's address frames (`pc:sp` pairs plus the
`|<-PC` marker) and translates them via `addr2line -pfiaC` into
`function / file:line`.

- ELF resolution: `--elf` → config `elf_map[<device name>]`, where device name
  is the log's parent directory (`logs/esp32s3-jtag/serial-….log` →
  `esp32s3-jtag`). Error if neither is set
- addr2line resolution: `--addr2line` → `$ESP_ADDR2LINE` →
  `xtensa-esp32{,s3}-elf-addr2line` / `riscv32-esp-elf-addr2line` on PATH →
  glob under `~/.espressif/tools/` (newest match)

## `version`

Prints `serialtap <version>` (currently 0.1.0).
