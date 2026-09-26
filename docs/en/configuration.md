# Configuration

[English](configuration.md) | [简体中文](../zh-CN/configuration.md)

serialtap is configured by a single JSON file. An annotated example ships in
[config.example.json](../../config.example.json) at the repo root.

## File resolution

- Default path: `~/.config/serialtap/config.json` — loaded **only if it
  exists**; otherwise built-in defaults apply. You can run with no config at
  all.
- `--config F` on the command line points somewhere else.
- `--root` / `--baud` command-line flags override the config values for that
  invocation.
- Numeric fields left at zero in the file fall back to defaults (e.g.
  omitting `rotate_max_mb` gets you 64, not 0) — **except** `retention_days`
  (explicit `0` = keep forever) and `silent_reopen_s` (explicit `0` = off),
  where zero is itself a meaningful value.

## Fields

| Field | Type | Default | Meaning |
|---|---|---|---|
| `root` | string | `"logs"` | Log root directory. One subdirectory per device is created underneath, plus the `PAUSED` file. |
| `baud` | int | `115200` | Serial baud rate. Meaningless (but harmless) for native USB-CDC devices like the ESP32-S3 USB-JTAG port. |
| `poll_interval_ms` | int | `1000` | Hot-plug poll interval of the daemon loop. |
| `silent_reopen_s` | int | `0` (off) | Silent watchdog: force a port reopen after this many seconds of zero bytes. **Only enable for devices guaranteed to emit periodic output** (e.g. a 30 s heartbeat) — otherwise legitimately quiet boards get caught in a reset loop. See [architecture.md](architecture.md#reset-semantics-open-once-and-hold). |
| `reopen_min_s` | int | `5` | Reconnect backoff lower bound (seconds). |
| `reopen_max_s` | int | `60` | Reconnect backoff upper bound. Backoff doubles from min up to max on repeated failures and resets after any session that opened successfully. |
| `rotate_max_mb` | int | `64` | Per-file size rotation threshold for both log channels. |
| `retention_days` | int | `14` | Delete `<root>/*/*.log` files older than this many days (by mtime), at startup and hourly. `<= 0` keeps everything forever. |
| `exclude` | [string] | `[]` | Regexes of devices to ignore entirely (matched against tty / key / by-id / name). Bad regexes are skipped with a warning. |
| `names` | [{match, name}] | `[]` | by-id regex → device directory name rules. See below. |
| `signatures_extra` | [string] | `[]` | Additional event-signature regexes. See below. |
| `elf_map` | {name: path} | `{}` | Device name → firmware `.elf`, used by `decode-backtrace`. See below. |
| `control_socket` | string | `""` | Control socket path. Empty = platform default: on Linux/macOS `$XDG_RUNTIME_DIR/serialtap.sock` (fallback `/tmp/serialtap-<uid>.sock`), on Windows `%LOCALAPPDATA%\serialtap\serialtap.sock`. The `run --sock` flag overrides it. |
| `web_addr` | string | `""` | Web panel listen address. Empty = `127.0.0.1:8801`; `"off"` disables. The `run --web` flag overrides it. |
| `esptool_cmd` | string | `""` | esptool executable for `flash`. Empty = auto-discover (`esptool` then `esptool.py` on PATH). `--esptool` flag wins over this. |
| `flash_baud` | int | `0` | Baud rate for `flash`. `0` = esptool's default. `--baud` flag wins over this. |
| `flash_timeout_s` | int | `0` | Per-device flash timeout in seconds (on timeout esptool is killed and capture resumed). `0` = no timeout. |
| `proxy_tap_exclude` | string | `""` | Per-line regex kept out of the full log during proxy sessions (e.g. `^#S1 ` to drop high-rate telemetry). Empty = log everything. |

## Device naming

Every device gets a directory name under `root`. Resolution order:

1. **Config `names`** — first rule whose `match` regex matches the device's
   by-id string wins
2. **Built-in rules**:

   | by-id contains | name |
   |---|---|
   | `USB_Serial-if00` | `ch340` (1a86:7523, ai-thinker style boards) |
   | `USB_Single_Serial` | `ch343` (1a86:7522/55d3, luatos style boards) |
   | `Espressif_USB_JTAG` | `esp32s3-jtag` (native USB-JTAG, seeed/n16r8…) |

3. by-id base name, else the tty name

The name is sanitized to `[A-Za-z0-9-_.]` (other characters become `-`,
truncated to 64 chars). If two live devices resolve to the same name, the
later one gets an **identity-derived suffix** `-<token>`: a 4-hex-character
hash of the device's stable identity (key/by-id; on Windows the instance
path embeds the MAC) — so the same board keeps the same suffix no matter
when it attaches or how often the daemon restarts, and the bare base name
goes to whoever attached first. With two such boards attached, anchor the
suffixed name (e.g. `^esp32s3-jtag-1x2y$`).

**Telling identical boards apart:** same-model adapters often share an
identical by-id (CH340 exposes no serial number), so distinguish them by
physical port (never rename-dependent). Espressif native USB-JTAG by-id does
embed the MAC — match on it to pin a specific board:

```json
"names": [
  { "match": "Espressif_USB_JTAG_serial_debug_unit_AA:BB:CC:DD:EE:FF", "name": "board-a" },
  { "match": "Espressif_USB_JTAG_serial_debug_unit_11:22:33:44:55:66", "name": "board-b" }
]
```

## Error signatures

Lines matching a signature are copied (truncated to 200 chars) into the
device's `events-*.log` channel, tagged `[signature-name]`. The built-in set
covers common ESP-IDF fault lines:

| Name | Pattern (i = case-insensitive) |
|---|---|
| `reset-banner` | `rst:0x` |
| `boot-mode` | `boot:0x` |
| `restart-call` | `esp_restart` |
| `esp-log-error` | `E \(` |
| `guru-meditation` | `Guru Meditation` |
| `backtrace` | `Backtrace:` |
| `panic` | `panic` (i) |
| `watchdog` | `WDT\|watchdog` (i) |
| `abort` | `abort` (i) |
| `assert` | `assert` (i) |
| `lwip-accept-err` | `accept \(-?\d+\)` |
| `probe-failed` | `probe failed` (i) |
| `pausing` | `pausing` (i) |
| `reboot` | `reboot` (i) |

`signatures_extra` appends your own regexes; they are named `extra-0`,
`extra-1`, … A bad regex is skipped (logged), never fatal. The first matching
signature wins. Note: offline `analyze` uses only the built-in set.

## `elf_map`

`decode-backtrace` needs the firmware ELF that matches the log's device:

```json
"elf_map": {
  "board-a": "/path/to/firmware/build/app.elf"
}
```

The key is the device directory name (the log's parent directory). The
`--elf` flag overrides the mapping for a single run.
