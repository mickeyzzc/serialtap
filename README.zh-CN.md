# serialtap

**Linux 上的零操作 USB 串口日志采集器。** 插上设备 —— serialtap 自动发现它、
只打开一次端口，持续滚动保存带时间戳的日志，并输出错误签名事件流。
为 ESP32 机群调试而生，兼容任何 USB 串口设备
（CH340/CH343/CP210x/FTDI/原生 USB-CDC…）。

单二进制 Go 程序：插上设备自动识别 → 持续采集 → 双通道日志（全量 + 错误事件）→
离线分析（签名汇总 / Backtrace addr2line 解码）。

[![CI](https://github.com/mickeyzzc/serialtap/actions/workflows/ci.yml/badge.svg)](https://github.com/mickeyzzc/serialtap/actions/workflows/ci.yml)

[English](README.md) | **简体中文**

## 文档

| 文档 | 内容 |
|---|---|
| [CLI 参考](docs/zh-CN/cli-reference.md) | 全部子命令、flag、退出码、设备匹配正则语义 |
| [配置参考](docs/zh-CN/configuration.md) | 全部配置字段与默认值、命名规则、`elf_map`、`esptool_cmd` |
| [架构](docs/zh-CN/architecture.md) | 包结构与设计决策（open-once-and-hold、by-path 身份、零丢失拼装）、测试策略 |
| [控制协议](docs/zh-CN/control-protocol.md) | 守护进程的 JSON 行 unix socket API —— 自己脚本化调 `status`/`release`/`flash` |
| [部署指南](docs/zh-CN/deployment.md) | 安装、串口权限、systemd 用户级服务、故障排查 |

## 特性

- **热插拔自动采集**：轮询发现 USB 串口（1s），每设备一个采集协程，插上即采、拔走即停
- **身份稳定**：以 USB 物理口（by-path）为设备身份 —— 重枚举换 ttyUSB 号不影响；
  同型号适配器（by-id 无序列号的 CH340）也不撞车
- **双通道日志**：`serial-日期.log` 全量（毫秒级逐行时间戳）+ `events-日期.log`
  事件流（错误签名命中 + 采集器生命周期），按日 + 按大小轮转，保留期自动清理
- **错误签名引擎**：内置 ESP-IDF 常见故障行（`rst:0x` 复位 banner、`E (` 错误级日志、
  lwIP `accept (n)`、Guru Meditation、WDT、Backtrace…），正则可自由扩展
- **Backtrace 解码**：`decode-backtrace` 提取地址帧交给 addr2line 翻译成
  `源文件:行号`，自动发现 ESP-IDF 工具链
- **刷写安全门**：`pause`/`resume` 暂停清单、`release` 临时让口（空闲自动回采）、
  `flash` 代理刷固件（守护进程经 unix socket 控制通道编排：让口 → esptool → 回采，
  防止 esptool 与常驻采集器抢口把 ESP32 楔进 ROM 下载模式）
- **零丢失**：跨读取块行拼装，端口关闭时的残余半行以 `…partial` 标记落盘
- **纯 Go 静态二进制**：无 CGO、无 libudev 依赖，vendor 已含全部依赖，
  离线可构建，交叉编译即拷即用

## 快速开始

```bash
# 方式一：Releases 页下载预编译二进制（linux-amd64/arm64，tag 触发构建）
# 方式二：源码构建
git clone https://github.com/mickeyzzc/serialtap && cd serialtap
make build                 # 或: go build .
./serialtap list           # 看当前设备: tty / 名字 / VID:PID / by-id / 物理口
./serialtap run            # 守护模式
```

## 命令

| 命令 | 作用 |
|---|---|
| `run` | 守护模式。轮询发现 USB 串口，每设备一个采集协程 |
| `attach TTY [--name N]` | 单口采集（手动围观/测试，可接 socat PTY） |
| `list` | 列出当前设备与身份 |
| `pause [RE]` / `resume [RE]` | 暂停/恢复采集（省略 = 全部） |
| `release RE [--for 5m]` | **临时让出串口**给外部工具：默认端口空闲 3 秒自动回采，或限时自动回采 |
| `flash RE <bin>[@0x10000]...` | **代理刷固件**：让口 → esptool → 自动回采，进度流式回传；`--args-file build/flasher_args.json` 一键刷 IDF 全套。RE 为正则，多设备会**逐台刷**，精确刷一台用锚定（如 `^board$`） |
| `status` | 守护进程与设备实时状态（collecting/paused/suspended/flashing） |
| `analyze LOG...` | 离线签名扫描：计数 / 首末时间 / 样本行汇总表 |
| `decode-backtrace LOG` | `Backtrace:` 地址帧 addr2line 解码 |

flags（`--root/--baud/--config/--poll-ms/--exclude`）可写在位置参数前后任意位置。
完整说明见 [CLI 参考](docs/zh-CN/cli-reference.md)。

## 复位语义（重要）

**open 和 close 串口设备都会给板子一拍复位脉冲**，包括 ESP32-S3 原生
USB-JTAG 口（USB_SERIAL_JTAG 外设在硅内实现了与 CH340 一致的自动复位语义，
实测 open 后 3ms 出现 `rst:0x15 (USB_UART_CHIP_RESET)`，close 后设备重启）。
这是宿主侧 CDC 握手线行为，用户态无法避免。

因此 serialtap 的纪律是**每物理设备恰好 open 一次并长期持有**，绝不周期性重开：

- 热插拔接入：设备刚上电，这一拍复位无感
- `pause`（刷机前）：会复位设备 —— 无妨，esptool 本来就要复位
- 静默看门狗（`silent_reopen_s`）**默认关**：只对保证有周期日志输出的设备
  （如 30s 心跳）显式开启；否则合法的安静设备会被复位循环

## 日志布局

```
<root>/<设备名>/serial-YYYYMMDD.log     # 全量，每行 [YYYY-MM-DD HH:MM:SS.mmm] 前缀
<root>/<设备名>/serial-YYYYMMDD.001.log # 超 rotate_max_mb 后轮转
<root>/<设备名>/events-YYYYMMDD.log     # 事件流：签名命中 + 采集器生命周期
<root>/PAUSED                            # 暂停清单（每行一个正则，mtime 热重载）
```

## 设备命名

优先级：配置 `names`（by-id 正则）→ 内置规则（`ch340` / `ch343` / `esp32s3-jtag`）
→ by-id 基名 → tty 名。同名设备自动加 `-2` 后缀。用 by-id 里的序列号/MAC
可在配置里精确区分同芯片的不同板子（见 `config.example.json`）。

配置文件默认路径 `~/.config/serialtap/config.json`（不存在则全默认值），
所有字段见[配置参考](docs/zh-CN/configuration.md)。

## 离线分析

```bash
./serialtap analyze logs/esp32s3-jtag/serial-*.log
# SIGNATURE            COUNT  FIRST                   LAST                     SAMPLE
# reset-banner             3  2026-09-20 11:11:21.165 2026-09-20 11:13:34.229 ...

./serialtap decode-backtrace logs/esp32s3-jtag/serial-20260920.log
# addr2line: ~/.espressif/tools/.../xtensa-esp32s3-elf-addr2line
# elf:       由配置 elf_map 按设备名自动选取（也可 --elf 显式指定）
```

## systemd 部署（用户级服务）

```bash
mkdir -p ~/.local/bin ~/.config/serialtap ~/.config/systemd/user
cp serialtap ~/.local/bin/
cp config.example.json ~/.config/serialtap/config.json   # 按需改 root/names/elf_map
cp deploy/serialtap.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now serialtap
journalctl --user -u serialtap -f          # 看运行日志
loginctl enable-linger $USER               # 开机自启（不登录也跑）
```

串口权限：Linux 上用户需在 `uucp`（Arch）或 `dialout`（Debian 系）组。
完整指南见[部署指南](docs/zh-CN/deployment.md)。

## 开发

```bash
make test      # go test -race
make cover     # 覆盖率（CI 有 80% 门禁）
make lint      # golangci-lint（配置见 .golangci.yml）
make fmt       # gofmt
```

代码结构（`internal/` 分包，依赖单向、边界清晰）：

```
main.go                    # 薄入口（仅 os.Exit(cli.Run(...))）
internal/cli/              # 子命令分发与 flag 解析（编排层）
internal/config/           # 配置定义与加载
internal/device/           # 设备发现与稳定身份（by-path key / by-id 命名 / sysfs）
internal/collector/        # 单设备采集器（open-once-and-hold、可注入 Port seam）
internal/logstore/         # 双通道日志写入 / 轮转 / 保留期清理
internal/signature/        # 错误签名引擎
internal/pause/            # 刷写暂停清单
internal/daemon/           # 热插拔守护循环（枚举 diff + 起停采集器 + release/flash 编排）
internal/flash/            # 代理刷固件（esptool 编排 + flasher_args.json 解析）
internal/ctl/              # 控制 unix socket（JSON 行协议）
internal/analyze/          # 离线分析（签名汇总 + addr2line 解码）
internal/testutil/         # 跨包测试助手（假串口等）
```

- TDD 开发，假端口注入驱动采集器全链路离线测试 + socat PTY 端到端
- 依赖已 `go mod vendor`：`go.bug.st/serial`（arduino-cli 同款串口库，纯 Go）
- Go 1.27+

设计决策详解见[架构](docs/zh-CN/architecture.md)。

## License

GPL-3.0-or-later，见 [LICENSE](LICENSE)。
