# serialtap

**A zero-touch USB serial middleware for Linux, Windows & macOS.** Plug in a
device — serialtap detects it, opens the port once, and keeps timestamped logs
rolling with an error-signature event stream. Built for ESP32 fleet debugging,
works with any USB serial device (CH340/CH343/CP210x/FTDI/native USB-CDC…).

A single Go binary: auto-detect on plug → continuous capture → dual-channel
logs (full stream + error events) → offline analysis (signature tally /
Backtrace addr2line decoding). The same daemon also proxies ports to TCP for
business software, orchestrates esptool flashing without ever fighting it for
the port, and serves a web panel where **every** operation — including
uploading images and flashing — can be done from a browser.

[![CI](https://github.com/mickeyzzc/serialtap/actions/workflows/ci.yml/badge.svg)](https://github.com/mickeyzzc/serialtap/actions/workflows/ci.yml)

**English** | [简体中文](README.zh-CN.md)

## Features

- **Hot-plug capture**: polls for USB serial ports (1 s), one collector
  goroutine per device — starts logging on plug, stops on unplug
- **Tray / menu bar**: on macOS `run` lives in the menu bar by default; on
  Windows `serialtap tray` is a resident systray app — live device status,
  per-device pause/resume, open logs, open the web panel (see below)
- **Stable identity**: the physical USB port is the device identity (Linux
  by-path / macOS locationID / Windows instance ID) — re-enumeration changing
  ttyUSB numbers doesn't matter; same-model adapters without serial numbers
  don't collide either
- **Dual-channel logs**: `serial-DATE.log` full stream (millisecond per-line
  timestamps) + `events-DATE.log` event stream (signature hits + collector
  lifecycle), rotated by day and by size, retention swept automatically
- **Error signature engine**: built-in ESP-IDF fault lines (`rst:0x` reset
  banner, `E (` error-level logs, lwIP `accept (n)`, Guru Meditation, WDT,
  Backtrace…), freely extensible with regexes
- **Backtrace decoding**: `decode-backtrace` extracts address frames and
  translates them via addr2line to `file:line`, auto-discovering the ESP-IDF
  toolchain
- **Flash safety gates**: `pause`/`resume` pause lists, `release` temporary
  port yielding (auto re-acquire when idle), `flash` proxy-flashing — the
  daemon orchestrates yield → esptool → re-acquire over the control socket so
  esptool never races the resident collector (which can wedge an ESP32 into
  ROM download mode); automatic retries absorb Windows CDC first-open failures
- **Transparent USB proxy**: `proxy` opens a 127.0.0.1 TCP endpoint per device
  — business software (e.g. a sense engine) reads/writes the board through it
  as if directly attached; the port is never reopened (no reset pulse),
  capture continues (bidirectional tap logging), high-rate telemetry lines can
  be excluded via `proxy_tap_exclude`; single-client semantics, endpoints
  close automatically on unplug/daemon exit
- **Soft reconnect, two layers**: `reopen` (serial layer — close port, skip
  backoff, reopen immediately) and `reset` (USB layer — Windows `pnputil`
  device-node restart, a software replug) recover wedged ports/drivers
  without touching the cable
- **Zero loss**: lines are assembled across read chunks; a trailing partial
  line at port close is flushed to disk marked `…partial`
- **Web panel with full operations**: pause/resume, proxy start/stop, port
  yield, soft reconnect, USB reset, and **image-upload flashing with live SSE
  progress** — remote/terminal-less scenarios never need the CLI
- **Multi-PC mesh** (opt-in): serialtap instances on several PCs discover each
  other on the LAN (UDP beacons) and forward control requests over a
  pre-shared-key encrypted channel — any PC can manage, tail, and flash the
  boards plugged into any other PC (`--peer`, `mesh status`, remote `at`)
- **Pure Go static binary**: no CGO (except the opt-in macOS tray), no
  libudev, everything vendored — builds offline, cross-compiles trivially
- **Three platforms**: full functionality on Linux / macOS / Windows (see the
  next section); Windows device discovery goes through the registry, the
  control channel is AF_UNIX (Win10 1803+)

## Platform support

| Capability | Linux | macOS | Windows |
|---|---|---|---|
| run/attach/list/analyze/decode-backtrace | ✓ | ✓ | ✓ |
| pause / resume / status / proxy / flash | ✓ | ✓ | ✓ (Win10 1803+) |
| `reopen` serial-layer soft reconnect | ✓ | ✓ | ✓ |
| `reset` USB-layer soft replug | ✗ | ✗ | ✓ (pnputil, Win10+, admin/UAC) |
| Device identity (stable key) | by-path (physical port) | `cu.*` name (encodes location/serial) | USB instance ID (registry) |
| release idle auto re-acquire | ✓ (/proc) | ✓ (lsof) | ✗ — use `--for` timed re-acquire or manual `resume` |
| Default control socket | `$XDG_RUNTIME_DIR/serialtap.sock` → `/tmp/serialtap-<uid>.sock` | same (with /tmp fallback) | `%LOCALAPPDATA%\serialtap\serialtap.sock` |

Match regexes (pause/release/flash/exclude/names) apply to the
tty / device name / key / by-id fields on every platform, but **the field
shapes differ**: on Linux by-id looks like `usb-Espressif_USB_JTAG_...`, on
Windows it's a `USB\VID_303A&PID_1001\...` instance path, on macOS a device
name like `usbmodem2101` — run `serialtap list` to see the actual values
before writing regexes. Built-in VID:PID rules (ch340/ch343/esp32s3-jtag/
rp2040-cdc) work on all three platforms.

## Tray & menu bar

- **macOS**: `run` embeds a menu-bar tray by default (`--no-tray` to disable) —
  live device status, pause/resume all capture, open log directory, open the
  web panel, quit. Requires a CGO build (Xcode Command Line Tools); a
  `CGO_ENABLED=0` build simply runs headless
- **Windows**: `serialtap tray` is a separate resident systray process talking
  to the daemon only over the control socket (killing the tray never touches
  capture):
  - one menu item per device — the checkbox *is* the on/off switch; click to
    pause/resume that device
  - per-device submenu: **view serial log** (latest full log in the system
    editor), **open log directory**
  - pause all / resume all; one-click daemon start when it's not running
  - **open web panel**; hover shows the live device count; grey icon =
    daemon not connected

```powershell
serialtap tray --root <log root> --sock <control socket>   # match run's flags
```

Linux has no tray (systray needs libappindicator) — use the web panel instead.

## Web observation panel

`serialtap run` serves an observation panel (default
**http://127.0.0.1:8801/**, `--web off` to disable) — everything the daemon
sees, plus every operation:

- **Three-view navigation**: logs first — the multi-device workbench the panel
  opens on — then waveforms, then the dashboard (fleet-health overview,
  opened on demand; `#logs`/`#wave`/`#dash` deep-link)
- **Dashboard**: KPI strip (devices, collecting, total throughput, unhealthy
  boards, events in the last 30 min, mesh nodes online) + device cards + a
  30-minute event timeline + mesh peer management — whole-fleet health on one
  screen; boards silent >5 min or with ≥3 reopens count as unhealthy (red)
- **Device cards**: state (collecting/paused/suspended/flashing), proxy-session
  badge, live write rate (log-size delta), full/event log sizes and total
  retention usage, plus an **identity line** (chip/model/fw, IP·SSID masked
  by default)
- **Automatic device identity**: boards identify themselves the moment they
  are plugged in — passive facts extraction from the log stream (chip family,
  reset reason, PSRAM, channel, IP, SSID; persisted per device), a full facts
  table with evidence lines and origins behind the identity line, one-click
  deep identify via esptool (chip revision/MAC/flash size; resets the board),
  and a `#DEV model=.. fw=..` firmware self-descriptor line with top
  precedence. Identity rides the mesh status aggregation — remote boards
  show the same
- **Multi-device logs (the landing workbench)**: tick any number of boards
  (mesh remotes included) and watch them together — **side-by-side** panes
  (each header carries quick actions: pause/resume, soft reconnect, flash —
  local and remote alike) or a **merged** stream interleaved by timestamp
  with per-device colored tags and a per-device quota so chatty boards cannot
  flush quiet ones out; regex/substring search + highlight works across
  devices; export the buffered logs of every selected board. SSE tailing
  (700 ms incremental pushes, follows day and size rotation)
- **Multi-device waveforms**: every board is sampled continuously in the
  background (switching views or selection never loses data) — the **grid**
  shows one live scope per board on one screen, and "大图" (detail view) opens
  single-board analysis (freeze, hover crosshair readout, advanced regex
  extraction, CSV export); zero-config auto-recognition of telemetry lines
  (`key=value` and numeric tokens — when the line *starts* with key=value,
  every token is a channel)
- **Recent events**: signature hits (reset banner, Guru Meditation, WDT…) +
  collector lifecycle + proxy-flash history, grouped per device, matched lines
  highlighted
- **Operations — full coverage**: per-device pause/resume, proxy start/stop,
  port yield (5 min), serial soft reconnect, USB reset (UAC confirm), and
  **flashing from the browser** — upload images+offsets (or a
  `flasher_args.json`) and watch esptool progress stream back over SSE. No CLI
  needed for any of it.

The panel is read-only display + forwarding of the existing ctl operations
through the **same handler path** as the control socket — it introduces no
second control logic and **no business logic** (serialtap stays a middleware).
It listens on loopback by default, same trust domain as the ctl socket; with
mesh enabled it additionally aggregates peers and bridges their log tails /
flash uploads through the encrypted channel (see below).

**Accessing the panel from other machines**: set `"web_addr": "0.0.0.0:8801"`
and open the port in the firewall (`ufw allow 8801`, `firewall-cmd
--add-port=8801/tcp --permanent && firewall-cmd --reload`, or the Windows
prompt). The panel has **no authentication** — every operation (including
flashing) becomes available to anyone on the network, so only do this on a
trusted LAN (e.g. WPA2 home network); the daemon logs a warning at startup
when bound non-loopback. Each machine's panel shows the whole mesh, so
opening one machine's panel is usually enough.

## Multi-PC mesh

Several PCs, each with its own boards, one management surface: enable mesh on
every machine and they discover each other and interoperate.

```jsonc
// ~/.config/serialtap/config.json — same mesh_key on EVERY machine
{
  "mesh_enabled": true,
  "mesh_key": "run `serialtap mesh keygen` once, paste everywhere",
  "mesh_name": "bench1",              // optional, defaults to hostname
  "mesh_port": 8802,                  // TCP channel + UDP beacon, same number
  "mesh_announce_s": 5,               // beacon interval; peers expire after 3×
  "mesh_peers": ["192.168.63.9:8802"] // static seeds, optional (AP-isolation /
                                      // cross-subnet fallback; discovery still
                                      // tries broadcast)
}
```

Then, from **any** machine:

```bash
serialtap mesh status                 # all peers + their boards, one table
serialtap status --peer bench2        # that peer's boards
serialtap flash '^n16r8-u1$' app.bin@0x10000 --peer bench2
                                      # image is read locally, uploaded over the
                                      # encrypted channel, flashed there
serialtap nvs '^n16r8-u1$' --peer bench2          # read NVS on a remote board
serialtap dump '^n16r8-u1$' 0x9000 0x6000 out.bin --peer bench2  # artifact comes back
serialtap at '^sense$' "AT+GMR" --peer bench2     # console injection over a tunnel
serialtap mesh forward bench2 '^sense$'          # local endpoint → remote serial port
```

The web panel grows a **mesh** section: every peer and its boards, remote log
tailing, remote pause/reopen, and remote flashing with a target-node selector.

**Auto-link & pairing.** Discovery is not passive: each node automatically
dials every peer it learns (beacon or static seed) and keeps the link alive.
A peer that connects is *knocking*: with the same key it passes the crypto
gate, but its operations are refused until you **approve** it once —
`serialtap mesh pair` lists pending requests, `serialtap mesh approve <id>`
lets it in (the panel's mesh section shows ✓/✗ buttons). Approvals persist
at `<root>/.mesh-peers.json`; `mesh revoke` cuts an approved peer off
immediately (active connections are dropped). Set `"mesh_auto_approve": true`
to return to key-only trust (everything with the key is let in, no prompts).

Security model, briefly (details in `docs/en/architecture.md`):

- everything crosses the wire inside an **AES-256-GCM** channel derived from
  the passphrase (PBKDF2 600k); beacons carry only an HMAC fingerprint —
  instances with different keys never see each other, and the key itself never
  leaves the machine
- the key is the **first** gate; **pairing approval is the second** — a
  same-key node still cannot operate until explicitly approved
  (`mesh_auto_approve` disables the second gate for key-only trust)
- firmware images and NVS dumps **do** cross the channel (that's the point) —
  it is encrypted, but only mesh members should be on the LAN; mesh is off by
  default and the network surface stays loopback-only until you enable it
- downloads from a peer are limited to artifacts that peer itself created
  (dump results) — a mesh peer cannot read arbitrary files
- allow the port in the firewall (Linux `ufw allow 8802`, Windows firewall
  prompt, macOS 15+ asks for *Local Network* permission); AP/client isolation
  on the router blocks mesh entirely — use static `mesh_peers` only if the
  network still routes between the hosts

## Quick start

```bash
# Option 1: grab a build from the Releases page (built automatically on v* tags)
#   Windows: serialtap-setup-<version>.exe (installer; Start-menu/desktop icon
#            goes straight to the tray) + a portable zip
#   macOS:   .dmg (drag to Applications; double-click = menu-bar tray)
#   Linux:   tar.gz (binary + systemd user-service example, amd64/arm64)
# Option 2: build from source
git clone https://github.com/mickeyzzc/serialtap && cd serialtap
make build                 # or: go build .
./serialtap list           # see current devices: tty / name / VID:PID / by-id / port
./serialtap run            # daemon mode (menu-bar tray on macOS by default)
```

Building the macOS tray locally needs clang (Xcode Command Line Tools); plain
`go build .` enables CGO there by default. `CGO_ENABLED=0` builds a
tray-less binary (enumeration/capture unaffected); released Linux binaries
are always CGO-free static builds. On Windows it's `serialtap.exe list`
(devices look like `COM3`) and single-port capture is
`serialtap attach COM3`.

## Installers & CI

- **CI** (`.github/workflows/ci.yml`): lint + coverage report + full test
  matrix on ubuntu/windows/macos + pure-Go cross-compile smoke (linux/windows;
  the darwin tray needs cgo and is covered by the native macOS job)
- **Release** (`.github/workflows/release.yml`, triggered by a `v*` tag):
  - **Windows installer**: Inno Setup (`packaging/windows/serialtap.iss`),
    per-user install without admin; Start-menu/desktop "serialtap tray"
    shortcut launches `serialtap tray` (FreeConsole, no black window);
    optional launch-on-finish; plus a portable zip
  - **macOS**: universal binary (amd64+arm64 lipo) → `.app` (LSUIElement
    hides the Dock icon; double-click = menu-bar tray) → DMG
    (`packaging/macos/make-app.sh`; the icon is generated from the
    `assets/logo/` assets via iconutil)
  - **Linux**: tar.gz with the binary + README + a systemd user-service
    example + INSTALL.md
  - versions are injected via `-ldflags -X ...cli.Version=<tag>`; artifacts
    ship with sha256sums
- The **Wave·Tap logo** (UART square wave + mid-bus tap — the universal
  symbol of serial levels × serialtap's job, tapping the line) is used
  everywhere: panel favicon/header, tray icons, installer assets.
  `assets/logo/gen.py` regenerates every size from the SVG.

## Commands

| Command | What it does |
|---|---|
| `run` | Daemon mode. Polls for USB serial ports, one collector goroutine per device; menu-bar tray on macOS by default (`--no-tray` off) |
| `attach TTY [--name N]` | Capture a single port in the foreground (manual watching/testing; can point at a socat PTY) |
| `list` | List current devices and identities |
| `pause [RE]` / `resume [RE]` | Pause/resume capture (omitted = all) |
| `proxy RE [--stop]` | **Transparent USB proxy**: open a TCP endpoint per matched device — business software treats it as directly attached; capture continues meanwhile (`proxy_tap_exclude` keeps high-rate telemetry out of the full log) |
| `release RE [--for 5m]` | **Temporarily yield a port** to an external tool: default re-acquires after 3 idle seconds, or after the given duration |
| `flash RE <bin>[@0x10000]...` | **Proxy flashing**: yield → esptool → auto re-acquire, output streamed back; retries on failure (`--retries`, default 3 attempts × `--retry-wait` 5 s — Windows USB-CDC devices often fail the first open/SetCommState after a reset and esptool itself never retries); `--args-file build/flasher_args.json` flashes a whole IDF set; `--dry-run` previews the exact esptool commands. RE is a regex; **matching several devices is refused by default** (anti-misflash protection — an unanchored regex would drag other same-chip boards into the flash sequence; the device list is printed and an anchor demanded), `--all` flashes one-by-one on purpose, anchor (`^board$`) flashes exactly one. Remote flashing: `--peer` over the mesh channel (see [Multi-PC mesh](#multi-pc-mesh)), or an SSH socket forward for single-machine setups |
| `reopen RE [--all]` | **Serial-layer soft reconnect**: close the port now → skip backoff → reopen now. Fast self-heal for wedged ports (idle-spinning reads, odd driver states); does not change ownership or pause semantics (unlike `release`). Interrupts live proxy sessions (clients just reconnect), and open/close each deliver a reset pulse (see [reset semantics](#reset-semantics-read-this) — on CH340/Espressif native USB this effectively soft-reboots the board). Multi-device gate as `flash` (`--all`) |
| `reset RE [--all]` | **USB-layer soft replug** (Windows only): yield → `pnputil /restart-device` (disable+enable the device node, a software replug) → verify re-enumeration with the built-in enumerator → re-acquire. Targets the serial interface node; sibling JTAG interfaces are untouched. For devices present on the bus but wedged (won't open, zombie handles). Needs admin: an unelevated daemon auto-pops UAC to retry (cancellable). When the device has vanished from the bus entirely, only a physical replug helps. Multi-device gate as `flash` (`--all`) |
| `status` | Live daemon and per-device state (collecting/paused/suspended/flashing) |
| `tray` (Windows) | Resident systray: status, per-device pause/resume, open logs — see above (macOS has no separate `tray`; `run` embeds the menu bar) |
| `analyze LOG...` | Offline signature tally: counts / first-last / sample-line summary |
| `decode-backtrace LOG` | Decode `Backtrace:` address frames via addr2line |
| `mesh status [--json]` | **Mesh aggregate view**: every peer node on the LAN (discovered or static) with state, latency, and its boards |
| `mesh keygen` | Generate a mesh pre-shared key passphrase (paste into every machine's config; the key never enters logs or beacons) |
| `mesh pair` | **Pairing overview**: pending knocks (same-key nodes asking to link), approved peers, revoked peers |
| `mesh approve <id>` / `mesh revoke <id>` | Approve a pending pairing (operations unblocked within one knock cycle) / revoke an approved peer — its active connections are dropped immediately |
| `mesh forward <peer> <RE>` | Open a **local tunnel endpoint** to a peer's board — business tools treat it as a local serial port |
| `--peer <node>` | On every control command (`status`/`flash`/`pause`/`resume`/`release`/`proxy`/`reopen`/`reset`/`info`/`partitions`/`nvs`/`dump`/`at`): execute on that peer — images upload from this machine over the encrypted channel, dump artifacts come back here |
| `version` | Print the version |
| `info RE` | **芯片信息**：让口 → esptool flash_id（芯片/MAC/flash 容量）→ 回采。与 flash 同编排（opMu 互斥、多台默认拒绝、`--all` 逐台）；**读操作也会让目标板复位**（esptool download 模式进出） |
| `partitions RE` | **分区表读取并解析**（0x8000 legacy 二进制格式：类型/子类型/偏移/大小/标签） |
| `nvs RE [--show-secrets]` | **NVS 提取解析**：自动定位 nvs 分区 → 读取 → 解析命名空间/键/类型/值。凭据形键（pass/token/secret/seed/key）默认掩码 `****`，`--show-secrets` 明文（注意终端留痕）；事件审计只记动作与地址，**绝不记值**。IDF v6 自带 nvs_parser CLI 已失效，本解析器按 nvs_types.hpp 布局实现并经真机 dump 对账 |
| `dump RE <addr> <size> <file>` | **任意 flash 区域原始导出**（如 `dump cam 0x9000 0x6000 nvs.bin`）。产物权限 0600——可能含明文凭据，勿提交仓库 |
| `at RE "AT+..." [--wait 3s]` | **控制台命令注入**：经 proxy 透传通道写入并回显响应（写入以 `>` 前缀落档审计，采集全程不打断）。`--wait` 窗口内回显完整串口流，响应行以 `+`/`OK`/`ERROR` 开头；可一次注入多条依序执行 |
| `status` | 守护进程与设备实时状态（collecting/paused/suspended/flashing） |
| `tray`（Windows/macOS） | 托盘常驻：接入状态、按设备暂停/恢复、打开日志，见下节（Linux 暂无：systray 需 libappindicator，用 Web 面板） |
| `analyze LOG...` | 离线签名扫描：计数 / 首末时间 / 样本行汇总表 |
| `decode-backtrace LOG` | `Backtrace:` 地址帧 addr2line 解码 |


Flags (`--root/--baud/--config/--poll-ms/--exclude/--sock`) may appear before
or after positional arguments.

## Documentation

- [Architecture](docs/en/architecture.md) — package layering and dependency
  rules, collector state machine, open-once-and-hold, release/flash/reopen
  orchestration, testability seams
- [CLI reference](docs/en/cli-reference.md) — every command and flag
- [Configuration](docs/en/configuration.md) — every field and default,
  device-naming chain, PAUSED file, built-in signatures, log rotation
- [Control protocol](docs/en/control-protocol.md) — speak the daemon's JSON
  line protocol from any language; the `peer` field routes requests over the
  mesh channel
- [Waveform guide](docs/en/waveform-guide.md) — the panel oscilloscope from
  zero to reading charts: auto-detection, custom-extraction regex tutorial
  (templates + live preview), interpretation methods and examples (the panel's
  ? button carries a condensed in-app help page)
- [Control protocol](docs/en/control-protocol.md) — the ctl socket's JSON
  line protocol in full (scripting integration, SSH-tunnel remote use)
- [Deployment](docs/en/deployment.md) — installers, systemd/launchd/Task
  Scheduler, disk management
- [Contributing](CONTRIBUTING.md) — build/test/lint, TDD and injection seams,
  platform notes, release process

## Reset semantics (read this)

**Opening and closing a USB serial port both pulse the board's reset line**,
including the ESP32-S3 native USB-JTAG port (the USB_SERIAL_JTAG peripheral
implements the same auto-reset semantics as a CH340 in silicon; measured:
`rst:0x15 (USB_UART_CHIP_RESET)` 3 ms after open, reboot after close). This
is host-side CDC handshake-line behavior and cannot be avoided from userspace.

Hence serialtap's discipline: **open each physical device exactly once and
hold it** — never reopen periodically:

- hot-plug attach: the device just powered up, the pulse is harmless
- `pause` (before flashing): resets the board — fine, esptool was going to
- the silent watchdog (`silent_reopen_s`) is **off by default**: enable it
  only for devices guaranteed to emit periodic output (e.g. a 30 s
  heartbeat), otherwise legitimately quiet boards end up in a reset loop
- `reopen` (serial-layer soft reconnect) is an **explicit manual exception**:
  the user asked for a close/reopen, so the pulse is part of the feature (on
  CH340/Espressif native USB it amounts to soft-rebooting the board) — the
  automatic paths (disconnect-reopen loops) still obey the backoff discipline

## Log layout

```
<root>/<device>/serial-YYYYMMDD.log     # full stream, [YYYY-MM-DD HH:MM:SS.mmm] prefix per line
<root>/<device>/serial-YYYYMMDD.001.log # rotated after rotate_max_mb
<root>/<device>/events-YYYYMMDD.log     # event stream: signature hits + collector lifecycle
<root>/PAUSED                           # pause list (one regex per line, hot-reloaded on mtime)
```

## Device naming

Resolution order: config `names` (by-id regexes) → built-in rules
(`ch340` / `ch343` / `esp32s3-jtag` / `rp2040-cdc`) → by-id base name → tty
name. When two
live devices resolve to the same name (e.g. two Espressif native USB-JTAG
both `303a:1001` → both `esp32s3-jtag`), an **identity-derived suffix**
`-<token>` is appended: the token is a 4-hex hash of the device's stable
identity (key/by-id; on Windows the instance path embeds the MAC), so the
same board keeps the same suffix no matter when it attaches or how often the
daemon restarts; the bare base name goes to whoever attached first.
**With two identical boards attached, anchor the suffixed name** (e.g.
`^esp32s3-jtag-1x2y$`), or give the boards semantic names via the serial/MAC
in their by-id (config `names`, see `config.example.json`).

The config file defaults to `~/.config/serialtap/config.json` (only loaded if
present); see `config.example.json` and [configuration](docs/en/configuration.md)
for every field.

## macOS notes

- **Device identity**: Linux uses sysfs by-path; macOS parses `ioreg` (the
  IOKit registry) — the USB `locationID` (physical port) is the key, with
  `usb-<vid>_<pid>[-<serial>]` as by-id. The by-id style matches Linux, so
  `names` rules carry across platforms. ioreg runs only when the port set
  changes (hot-plug), so steady-state polling costs nothing. Only
  `/dev/cu.usb*` devices are enumerated (Bluetooth/wlan-debug ports are
  filtered naturally; capture uses cu.* — opening tty.* on macOS blocks
  waiting for carrier)
- **Menu-bar tray**: the default CGO build includes the tray
  (fyne.io/systray): live device status, pause/resume all, open log directory
  (Finder), open web panel (default http://127.0.0.1:8801/, hidden
  automatically when the panel is off), quit. `run --no-tray` runs headless
  (SSH-remote macs); a `CGO_ENABLED=0` build has no tray automatically
- **Control socket**: defaults to `/tmp/serialtap-$UID.sock` (BSD caps unix
  socket paths at 104 bytes; overlong paths fail with a clear error)

## Offline analysis

```bash
./serialtap analyze logs/esp32s3-jtag/serial-*.log
# SIGNATURE            COUNT  FIRST                   LAST                     SAMPLE
# reset-banner             3  2026-09-20 11:13:34.229 2026-09-20 11:11:21.165 ...

./serialtap decode-backtrace logs/esp32s3-jtag/serial-20260920.log
# addr2line: ~/.espressif/tools/.../xtensa-esp32s3-elf-addr2line
# elf:       chosen via config elf_map by device name (or --elf explicitly)
```

## Running as a service

Linux (systemd user service):

```bash
mkdir -p ~/.local/bin ~/.config/serialtap ~/.config/systemd/user
cp serialtap ~/.local/bin/
cp config.example.json ~/.config/serialtap/config.json   # adjust root/names/elf_map
cp deploy/serialtap.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now serialtap
journalctl --user -u serialtap -f          # follow runtime logs
loginctl enable-linger $USER               # start at boot without login
```

Serial permissions: on Linux the user must be in `uucp` (Arch) or `dialout`
(Debian-family). macOS and Windows need no permission setup — see
[deployment](docs/en/deployment.md) for the macOS launchd agent and the
Windows installer/Task Scheduler paths.

## Troubleshooting

- **`--peer` says "尚未授权/未授权"**: the remote node hasn't approved yours
  yet — run `serialtap mesh pair` **on that node** and `mesh approve <id>`,
  or set `mesh_auto_approve: true` on it. Approval takes effect within one
  knock cycle (≤30 s); revocation is immediate.
- **Mesh peers don't discover each other**: check `serialtap mesh status` on
  both sides. In order: (1) same `mesh_key` (fingerprints differ → beacons are
  ignored silently); (2) the port open in the firewall for **both TCP and UDP**
  (`ufw allow 8802`); (3) router **AP/client isolation** off (it blocks all
  station-to-station traffic — mesh cannot work across it, configure static
  `mesh_peers` only if routing still works); (4) macOS 15+ asks once for
  *Local Network* permission — grant it; (5) clocks within ±10 min
  (`mesh 握手失败` with a skew hint means NTP is off on one side).
- **Proxy flash reports "port busy"**: usually another serialtap instance
  (e.g. a demo daemon started from an old checkout) holds the port. Find it
  with `Get-CimInstance Win32_Process -Filter "name='serialtap.exe'" | select ProcessId,CommandLine`
  and end it; this daemon's collector takes the port back with its usual
  backoff.
- **Proxy endpoint connects but no device data arrives**: TCP dial returns
  before accept/attachment completes, so device→client bytes emitted before
  attachment aren't mirrored (client→device is kernel-buffered and
  unaffected). Do an application-level handshake first (e.g. homepulse's
  sense_start/ack) to avoid it.
- **`open failed: ... device busy`** — the port is held by another process
  (EBUSY; details in the events log). `lsof /dev/ttyUSB0` to find the holder,
  or `release` the port properly.
- **Startup refuses: control socket held by another serialtap instance** — a
  live instance is running (anti-hijack protection). Check
  `systemctl --user status serialtap`; for parallel instances give each a
  different `control_socket` (`run --sock`) and log `root`.
- **`flash`/`release`/`status` can't reach the daemon** — `serialtap run`
  isn't running, or the socket path differs (resolution rules in the
  [control protocol](docs/en/control-protocol.md); on Windows the default is
  `%LOCALAPPDATA%\serialtap\`).
- **Windows `release` says idle auto re-acquire unsupported** — expected:
  Windows has no /proc/lsof holder detection. Use `release RE --for 5m`, or
  `resume` after flashing.
- **Regex matches no device** — field shapes differ per platform (see the
  platform table); run `serialtap list` to see the real values.
- **Device name carries a `-xxxx` hash suffix** — same-name collision
  (same-model boards with serial-less by-id, e.g. two native USB-JTAG). The
  suffix derives from the stable identity and survives restarts — anchor it
  when two such boards are attached, or split them via config `names` on
  by-id serial/MAC.
- **Paused and never capturing** — `cat <root>/PAUSED` to see the list;
  `serialtap resume` (no argument) clears everything.
- **Board keeps rebooting** — check whether the `silent_reopen_s` watchdog is
  on; it reset-loops legitimately quiet boards. Keep it at the default `0`.
- **`analyze` doesn't see custom signatures** — known limitation: offline
  scanning uses the built-in set only; `signatures_extra` affects the live
  event stream only.
- **Device plugged in but not captured** — user not in `dialout`/`uucp`
  (Linux), or an `exclude` regex matches (check tty/by-id/name).

## Development

See [contributing](CONTRIBUTING.md) and the [architecture doc](docs/en/architecture.md).

```bash
make test      # go test -race
make cover     # coverage (CI gates at 80%)
make lint      # golangci-lint (config in .golangci.yml)
make fmt       # gofmt
```

Code layout (`internal/` packages, one-way dependencies, clean boundaries):

```
main.go                    # thin entry (os.Exit(cli.Run(...)) only)
internal/cli/              # subcommand dispatch & flag parsing (orchestration)
internal/config/           # config definition & loading
internal/device/           # discovery & stable identity (Linux sysfs/by-path, Windows registry, macOS ioreg)
internal/collector/        # per-device collector (open-once-and-hold, injectable Port seam)
internal/logstore/         # dual-channel log writing / rotation / retention sweep
internal/signature/        # error signature engine
internal/pause/            # flash pause list
internal/daemon/           # hot-plug daemon loop (enumerate diff + collector lifecycle + release/flash/reopen/reset orchestration)
internal/flash/            # proxy flashing (esptool orchestration + flasher_args.json parsing)
internal/ctl/              # control unix socket (JSON line protocol)
internal/web/              # observation & operations panel (SSE live tail, flash upload, cmd forwarding)
   └── internal/mesh/          # opt-in LAN mesh: UDP beacon discovery + PSK-encrypted channel; peers become a third front-end over the same ctl handler
internal/tray/             # tray/menu bar (darwin+cgo embedded in run; Windows systray process; stubs elsewhere)
internal/analyze/          # offline analysis (signature tally + addr2line decoding)
internal/testutil/         # cross-package test helpers (fake serial ports, …)
```

- TDD; fake-port injection drives the full collector chain offline + socat
  PTY end-to-end
- Dependencies vendored: `go.bug.st/serial` (the arduino-cli serial library,
  pure Go)
- Go 1.27+

## License

GPL-3.0-or-later — see [LICENSE](LICENSE).
