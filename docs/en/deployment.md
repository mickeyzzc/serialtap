# Deployment

[English](deployment.md) | [简体中文](../zh-CN/deployment.md)

## Install

### Prebuilt binary (recommended)

Grab a binary from the
[Releases](https://github.com/mickeyzzc/serialtap/releases) page
(`serialtap-linux-amd64` or `serialtap-linux-arm64`, plus `sha256sums.txt`;
built automatically on `v*` tags):

```bash
curl -LO https://github.com/mickeyzzc/serialtap/releases/download/v0.1.0/serialtap-linux-amd64
sha256sum -c sha256sums.txt --ignore-missing   # optional integrity check
chmod +x serialtap-linux-amd64
./serialtap-linux-amd64 version
```

### Build from source

Requirements: Go **1.27+**. Nothing else — all dependencies are vendored, no
CGO, so it builds fully offline:

```bash
git clone https://github.com/mickeyzzc/serialtap && cd serialtap
make build        # or: go build .
```

Cross-compile the same way, e.g. for an ARM box:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o serialtap .
```

The daemon requires Linux (device identity is built on sysfs and
`/dev/serial/by-path`). Other platforms compile but `run`/`list` refuse with
an explicit error.

## Serial port permissions

Your user needs read/write access to the tty devices. Add yourself to the
right group and re-login (or `newgrp`):

| Distribution | Group |
|---|---|
| Arch, Fedora | `uucp` |
| Debian, Ubuntu & derivatives | `dialout` |

```bash
sudo usermod -aG dialout "$USER"   # Debian-family example
```

Verify with `ls -l /dev/ttyUSB0` and `groups`.

## First run

```bash
./serialtap list      # sanity check: devices show up with names and keys
./serialtap run       # foreground daemon; Ctrl-C to stop
```

Logs land under the configured `root` (default `./logs`). Once happy, set up
the config file and the service.

## Configuration

```bash
mkdir -p ~/.config/serialtap
cp config.example.json ~/.config/serialtap/config.json
$EDITOR ~/.config/serialtap/config.json
```

At minimum set `root` to wherever logs should live; add `names` /
`elf_map` / `exclude` as needed — every field is documented in
[configuration.md](configuration.md).

## systemd user service

The repo ships a unit file, [deploy/serialtap.service](../../deploy/serialtap.service):

```ini
[Service]
ExecStart=%h/.local/bin/serialtap run --config %h/.config/serialtap/config.json
Restart=on-failure
RestartSec=3
```

Install and enable:

```bash
mkdir -p ~/.local/bin ~/.config/systemd/user
cp serialtap ~/.local/bin/
cp deploy/serialtap.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now serialtap
```

Verify:

```bash
systemctl --user status serialtap
journalctl --user -u serialtap -f     # follow runtime logs
serialtap status                      # per-device live state
```

**Start at boot without anyone logged in** (machines that run headless):

```bash
loginctl enable-linger "$USER"
```

A system-level unit works too if you prefer (adjust paths and add the serial
group to the service user), but the user service is the tested setup.

## Disk usage management

- `rotate_max_mb` (default 64) caps each file; rotations suffix `.001`,
  `.002`…
- `retention_days` (default 14) deletes files older than the window at
  startup and hourly; `<= 0` disables deletion — watch your disk in that case.
- The `events-*` channel is tiny by comparison (signature hits and lifecycle
  lines only), but rotates under the same cap so a reset-loop board cannot
  grow it unbounded.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `连不上守护进程…未运行 serialtap run？` | The daemon isn't running (or the socket path differs — pass `--sock`, or set `control_socket`). `systemctl --user status serialtap`. |
| `run` exits with `控制 socket 已被另一个 serialtap 实例占用` | A live daemon already owns the socket. Talk to it instead of starting a second one, or run the second instance with `--sock` on a different path. |
| `Permission denied` opening a tty | Your user isn't in `dialout`/`uucp`, or you haven't re-logged-in since adding yourself. |
| Device missing from `list` | It has no `/dev/serial/by-path` entry (not a USB serial port), or an `exclude` pattern matches it. Check `ls /dev/serial/by-path/`. |
| Board resets whenever serialtap attaches | Expected — see reset semantics in the [README](../../README.md#reset-semantics-read-this). serialtap opens exactly once and holds; the pulse happens once per plug/pause cycle. |
| `找不到 esptool` / `找不到 addr2line` | The ESP-IDF environment isn't in PATH. `source ~/esp/esp-idf/export.sh`, or point `--esptool` / `--addr2line` (or config `esptool_cmd`, env `ESP_ADDR2LINE`) at the binaries. |
| `flash` reports `端口让出超时` | The collector could not close the port in 10 s — check daemon logs (`journalctl --user -u serialtap`); a wedged fd usually clears after replug. |
| Logs stay empty for a quiet board | That's legal. Only enable `silent_reopen_s` if the board is *guaranteed* to log periodically — otherwise you create a reset loop. |
| Two identical boards, logs land in one directory | Same-model adapters can't be told apart by by-id; they get `-2` suffixes in enumeration order. For stable names, match on serial/MAC in `names` (ESP32-S3 USB-JTAG by-id embeds the MAC) — see [configuration.md](configuration.md#device-naming). |

## Upgrading

Replace the binary (`~/.local/bin/serialtap`), then
`systemctl --user restart serialtap`. Log format and layout are stable;
rotation continues from existing suffix numbering, so nothing is overwritten.
Config gained only additive fields so far — old files keep working.
