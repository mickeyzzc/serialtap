# Control Protocol

[English](control-protocol.md) | [简体中文](../zh-CN/control-protocol.md)

The daemon (`serialtap run`) serves a local unix stream socket that the CLI
subcommands `status` / `pause` / `resume` / `release` / `flash` speak. You can
speak it too — for dashboards, CI jobs, or flashing scripts — from any
language that can write a line to a socket.

## Transport

- **Path resolution** (first non-empty wins): `run --sock` flag → config
  `control_socket` → `$XDG_RUNTIME_DIR/serialtap.sock` →
  `/tmp/serialtap-<uid>.sock`. Clients accept `--sock` the same way,
  defaulting to the same resolution.
- **Permissions**: the socket file is created mode `0600` — only the same
  user may connect.
- **Framing**: one JSON object per line (UTF-8, `\n`-terminated), in both
  directions.
- **Sessions**: a connection may carry several requests; each is answered in
  order. `flash` streams multiple response lines before its final one.
- **Single instance**: if a live daemon holds the socket, a second `run`
  refuses to start. A socket file with no listener (leftover from a crash)
  is removed and re-bound on startup.
- Invalid JSON in a request line gets `{"ok":false,"error":"bad request: …"}`
  and the connection stays open.

## Request

```json
{
  "cmd": "status | pause | resume | release | flash | proxy | reopen | reset",
  "action": "stop",          // proxy only: stop passthrough (default = start)
  "pattern": "regex, matched against tty / device name / by-path key / by-id",
  "for_ms": 300000,
  "until_idle": true,
  "spec": { "flash parameters, cmd=flash only" }
}
```

| Field | Used by | Meaning |
|---|---|---|
| `cmd` | all | The command. Unknown commands get `ok:false` with `unknown cmd`. |
| `pattern` | pause/resume/release/flash/proxy/reopen/reset | Device regex. Omitted on pause/resume = all devices. For the rest it is required and must match at least one live collector. |
| `action` | proxy | `stop` = stop passthrough for matched devices; default = start. |
| `all` | flash/reopen/reset | Execute one-by-one even when the pattern matches several devices (default: refuse and list the device names, protecting devices under test). |
| `for_ms` | release | Re-acquire after this many milliseconds (instead of idle detection). |
| `until_idle` | release | Re-acquire after the port has been idle (no other process holding it) for 3 continuous seconds. Used when `for_ms` is absent. |
| `spec` | flash | See below. |

### Flash `spec`

```json
{
  "esptool": "/path/to/esptool",
  "chip": "esp32s3",
  "baud": 921600,
  "bins":   [ { "path": "build/app.bin", "offset": "0x10000" } ],
  "args_file": "build/flasher_args.json"
}
```

`args_file` takes precedence over `bins`. Offsets are hex strings. `esptool`
empty = auto-discovery (`esptool` → `esptool.py` on PATH); `chip` empty =
auto-detect (esptool, or `flasher_args.json`'s `extra_esptool_args["--chip"]`).

## Response

```json
{
  "ok": true,
  "error": "only present on failure",
  "event": "flash-log | flash-done, flash only",
  "line": "esptool output line, flash-log events only",
  "devices": [ { "name", "tty", "key", "state" } ]
}
```

| Field | Meaning |
|---|---|
| `ok` | Success of this response. |
| `error` | Human-readable failure reason (daemon messages are in Chinese). |
| `event` | `flash-log` for each streamed esptool output line (payload in `line`); exactly one final `flash-done` ends the command. |
| `devices` | `status` only: per-device `{name, tty, key, state}` with state `collecting` / `paused` / `suspended` / `flashing`. |
| `line` | `release` success carries the number of yielded collectors as a string here. |

## Commands in detail

### `status`

Returns `devices`. The field is omitted entirely when no device is attached.

### `pause` / `resume`

Edits the same `PAUSED` file semantics as the CLI: `pause` with no pattern
pauses everything (`.*`); `resume` with no pattern clears the file — and also
revokes outstanding `release` holds. With a pattern, pause appends it; resume
removes entries equal to it. Effective immediately in the running daemon.

### `release`

Suspends matching collectors and **waits until their ports are actually
closed**, then replies `ok` with the count in `line`. From that moment an
external tool may open the port. Re-acquisition happens when `for_ms`
elapses, or (with `until_idle`) once no other process holds the tty for 3
continuous seconds. Fails if the pattern matches no collector.

### `flash`

Per matched device, sequentially: suspend collector → run esptool → resume.
Streams `flash-log` responses (one per esptool output line, `\r`-split for
progress bars), then one final `flash-done` with `ok` reflecting the outcome.
On esptool failure the collector is still resumed. Multiple matches flash one
by one — send an anchored pattern (`^board$`) to flash exactly one.

### `proxy`

For each matched device: suspends the exclusive hold, opens a TCP endpoint
(`endpoint` carries the address; `device`/`device_key` identify the owning
device — authoritative when several boards share a name), capture continues
during passthrough. `action:"stop"` stops it; `line` carries the count.

### `reopen`

For each matched device: close the port → the collector loop exits on read
error → reopens immediately **skipping the backoff**. `line` carries the count.
Interrupts live proxy sessions.

### `reset` (Windows only)

For each matched device: yield the port → restart the serial interface node
via pnputil → verify re-enumeration → resume capture. Needs admin (an
unelevated daemon pops UAC).

## Examples

`status` via `socat` (or any netcat with unix socket support):

```bash
echo '{"cmd":"status"}' | socat - UNIX-CONNECT:"$XDG_RUNTIME_DIR/serialtap.sock"
# {"ok":true,"devices":[{"name":"board-a","tty":"/dev/ttyACM0","key":"usb-Espressif_...-if00","state":"collecting"}]}
```

Yield a port for 10 minutes:

```bash
echo '{"cmd":"release","pattern":"^board-a$","for_ms":600000}' | socat - UNIX-CONNECT:"$XDG_RUNTIME_DIR/serialtap.sock"
# {"ok":true,"line":"1"}
```

Flash and follow the stream (the final line carries `event":"flash-done"`):

```bash
echo '{"cmd":"flash","pattern":"^board-a$","spec":{"bins":[{"path":"build/app.bin","offset":"0x10000"}],"chip":"esp32s3"}}' \
  | socat - UNIX-CONNECT:"$XDG_RUNTIME_DIR/serialtap.sock"
# {"ok":true,"event":"flash-log","line":"esptool.py v4.8"}
# {"ok":true,"event":"flash-log","line":"Chip is ESP32-S3"}
# ...
# {"ok":true,"event":"flash-done"}
```
