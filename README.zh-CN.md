# serialtap

**面向 Linux / Windows / macOS 的零操心 USB 串口中间层。** 插上设备——serialtap
自动识别、一次性打开端口、持续滚动带时间戳的日志并附带错误签名事件流。
为 ESP32 机群调试而生，任何 USB 串口设备都能用（CH340/CH343/CP210x/FTDI/
原生 USB-CDC…）。

单二进制 Go 程序：插上设备自动识别 → 持续采集 → 双通道日志（全量 + 错误
事件）→ 离线分析（签名汇总 / Backtrace addr2line 解码）。同一个守护进程还能
把串口透传成 TCP 端点给业务程序、编排 esptool 刷机绝不抢口、并内置一个
**所有操作**（含上传镜像刷机）都能在浏览器里完成的 Web 面板。

[![CI](https://github.com/mickeyzzc/serialtap/actions/workflows/ci.yml/badge.svg)](https://github.com/mickeyzzc/serialtap/actions/workflows/ci.yml)

[English](README.md) | **简体中文**

## 特性

- **热插拔自动采集**：轮询发现 USB 串口（1s），每设备一个采集协程，插上即采、拔走即停
- **托盘 / 菜单栏**：macOS 上 `run` 默认进驻菜单栏；Windows 上 `serialtap tray`
  常驻系统托盘——实时设备状态、按设备暂停/恢复、打开日志、打开 Web 面板（见下）
- **身份稳定**：以 USB 物理口为设备身份（Linux by-path / macOS locationID /
  Windows 实例 ID）——重枚举换 ttyUSB 号不影响；同型号适配器（by-id 无序列号
  的 CH340）也不撞车
- **双通道日志**：`serial-日期.log` 全量（毫秒级逐行时间戳）+ `events-日期.log`
  事件流（错误签名命中 + 采集器生命周期），按日 + 按大小轮转，保留期自动清理
- **错误签名引擎**：内置 ESP-IDF 常见故障行（`rst:0x` 复位 banner、`E (` 错误级日志、
  lwIP `accept (n)`、Guru Meditation、WDT、Backtrace…），正则可自由扩展
- **Backtrace 解码**：`decode-backtrace` 提取地址帧交给 addr2line 翻译成
  `源文件:行号`，自动发现 ESP-IDF 工具链
- **刷写安全门**：`pause`/`resume` 暂停清单、`release` 临时让口（空闲自动回采）、
  `flash` 代理刷固件（守护进程经控制通道编排：让口 → esptool → 回采，esptool
  绝不与常驻采集器抢口把 ESP32 楔进 ROM 下载模式）；失败自动重试吸收
  Windows CDC 复位后首开瞬时失败
- **透明 USB 代理**：`proxy` 为设备开一个 127.0.0.1 TCP 端点，业务程序
  （如 homepulse 感知引擎）经它直接读写板子串口——对程序等同直连；端口不重开
  （无复位脉冲），采集照常（tap 模式双向日志），可用 `proxy_tap_exclude`
  剔除高频遥测行；单客户端语义，设备拔出/守护退出自动收口
- **软断开重连（两层）**：`reopen`（串口层——立即关口、跳过退避立即重开）与
  `reset`（USB 层——Windows `pnputil` 重启设备节点，等效软件拔插），不动
  手拔插即可从端口/驱动僵死里自愈
- **零丢失**：跨读取块行拼装，端口关闭时的残余半行以 `…partial` 标记落盘
- **Web 面板操作全覆盖**：暂停/恢复、代理开停、让口、软重连、USB 重置，以及
  **上传镜像刷机 + SSE 实时进度**——远程/无终端场景完全不需要 CLI
- **纯 Go 静态二进制**：无 CGO（macOS 托盘可选开）、无 libudev 依赖，vendor
  已含全部依赖，离线可构建，交叉编译即拷即用
- **三平台**：Linux / macOS / Windows 全功能可用（平台差异见下节），
  Windows 设备发现走注册表、控制通道走 AF_UNIX（Win10 1803+）

## 平台支持

| 能力 | Linux | macOS | Windows |
|---|---|---|---|
| run/attach/list/analyze/decode-backtrace | ✓ | ✓ | ✓ |
| pause / resume / status / proxy / flash | ✓ | ✓ | ✓（Win10 1803+） |
| `reopen` 串口层软重连 | ✓ | ✓ | ✓ |
| `reset` USB 层软重枚举 | ✗ | ✗ | ✓（pnputil，Win10+，需管理员/UAC） |
| 设备身份（稳定 key） | by-path 物理口 | `cu.*` 设备名（位置/序列号编码） | USB 实例 ID（注册表） |
| release 空闲自动回采 | ✓（/proc） | ✓（lsof） | ✗ —— 用 `--for` 限时回采或 `resume` 手动回采 |
| 控制 socket 默认路径 | `$XDG_RUNTIME_DIR/serialtap.sock` → `/tmp/serialtap-<uid>.sock` | 同左（/tmp 回退） | `%LOCALAPPDATA%\serialtap\serialtap.sock` |

匹配正则（pause/release/flash/exclude/names）在各平台都作用于
tty / 设备名 / key / by-id 四个字段，但**字段形态不同**：
Linux 的 by-id 是 `usb-Espressif_USB_JTAG_...`，Windows 是 `USB\VID_303A&PID_1001\...`
实例路径，macOS 是 `usbmodem2101` 一类设备名 —— 用 `serialtap list` 看实际值再写正则。
内置 VID:PID 规则（ch340/ch343/esp32s3-jtag）三平台通用。

## 托盘与菜单栏

- **macOS**：`run` 默认内嵌菜单栏托盘（`--no-tray` 关闭）——实时设备状态、
  暂停/恢复全部采集、打开日志目录、打开 Web 面板、退出。需要 CGO 构建
  （装 Xcode Command Line Tools 即可）；`CGO_ENABLED=0` 构建自然无托盘、
  无头运行
- **Windows**：`serialtap tray` 是独立常驻的系统托盘进程，与守护进程只经
  控制 socket 通信（托盘退出不影响采集）：
  - **每台设备一个菜单项，勾选框即"接入开关"** —— 点击即暂停/恢复该设备
  - 设备子菜单：**查看串口日志**（系统默认编辑器打开最新全量日志）、
    **打开日志目录**
  - 全部暂停 / 全部恢复；守护进程未运行时菜单可一键启动（后台无窗口）
  - **打开 Web 面板**；悬停提示实时设备数；图标变灰 = 守护进程未连接

```powershell
serialtap tray --root <日志根> --sock <控制socket>   # 与 run 的参数保持一致即可
```

Linux 无托盘（systray 需 libappindicator）——用 Web 面板替代。

## Web 观测面板

`serialtap run` 内置观测面板（默认 **http://127.0.0.1:8801/**，`--web off` 关闭），
守护进程经手的一切可视化查看，且操作全覆盖：

- **设备卡**：接入状态（collecting/paused/suspended/flashing）、代理会话徽标、
  实时写入速率（由日志大小差分）、全量/事件日志体积与留存总量
- **实时日志**：全量 / 事件流双 tab，SSE 尾随（700ms 增量推送，自动跟随
  日轮转与大小轮转），自动滚动可暂停、可清屏；**设备下拉 + 点设备卡整卡
  切换**要查看的板子（多板并存时从这里选 COM）
- **最近事件**：签名命中（复位 banner、Guru Meditation、WDT…）+ 采集器
  生命周期 + 代理刷机记录，跨设备分组，命中行高亮
- **操作——全覆盖**：按设备暂停/恢复、代理开/停、让口（5 分钟）、串口软重连、
  USB 重置（UAC 确认），以及**浏览器里直接刷机**——上传镜像+偏移（或
  `flasher_args.json`），esptool 进度经 SSE 实时回放。以上全部不需要 CLI。

面板是只读展示 + 既有 ctl 操作的转发，经与 ctl socket **同一条处理路径**
执行——不引入第二套控制逻辑、**不做任何业务逻辑**（serialtap 定位不变）；
仅监听本机回环，与 ctl socket 同信任域。

## 快速开始

```bash
# 方式一：Releases 页下载（打 v* 标签自动构建）
#   Windows: serialtap-setup-<版本>.exe（安装包，开始菜单/桌面图标直进托盘）
#            + 便携版 zip；macOS: .dmg（拖入应用程序，双击=菜单栏托盘）；
#            Linux: tar.gz（binary + systemd 用户服务示例，amd64/arm64）
# 方式二：源码构建
git clone https://github.com/mickeyzzc/serialtap && cd serialtap
make build                 # 或: go build .
./serialtap list           # 看当前设备: tty / 名字 / VID:PID / by-id / 物理口
./serialtap run            # 守护模式（macOS 默认带菜单栏托盘）
```

macOS 本地构建托盘版需要 clang（装 Xcode Command Line Tools 即可），
`go build .` 默认 CGO 开；`CGO_ENABLED=0` 构建得到无托盘版（枚举/采集不受影响，
发布的 Linux 二进制始终是无 CGO 纯静态）。
Windows 上是 `serialtap.exe list`（设备形如 `COM3`）、单口采集 `serialtap attach COM3`。

## 安装包与 CI

- **CI**（`.github/workflows/ci.yml`）：lint + 覆盖率报告 + 三平台（ubuntu/
  windows/macos）全量测试 + 纯 Go 交叉编译冒烟（linux/windows；darwin 托盘
  需 cgo，由 macos 原生任务覆盖）
- **Release**（`.github/workflows/release.yml`，打 `v*` 标签触发）：
  - **Windows 安装包**：Inno Setup（`packaging/windows/serialtap.iss`），
    按用户安装免管理员；开始菜单/桌面「serialtap 托盘」= `serialtap tray`
    （FreeConsole 无黑窗），装完勾选即启动；另出便携版 zip
  - **macOS**：universal 二进制（amd64+arm64 lipo）→ `.app`（LSUIElement
    隐藏 Dock，双击=菜单栏托盘）→ DMG（`packaging/macos/make-app.sh`，
    图标由 `assets/logo/` 产物经 iconutil 生成）
  - **Linux**：tar.gz 含 binary + README + systemd 用户服务示例 + INSTALL.md
  - 版本号经 `-ldflags -X ...cli.Version=<tag>` 注入，产物附 sha256sums
- **Wave·Tap Logo**（UART 方波 + 低电平中点向下的抽头——串口电平的通用
  符号 × serialtap 的"在线分接"本职）全场景使用：面板 favicon/头部、托盘
  图标、安装包资产；`assets/logo/gen.py` 一键从 SVG 再生成全部尺寸

## 命令

| 命令 | 作用 |
|---|---|
| `run` | 守护模式。轮询发现 USB 串口，每设备一个采集协程；macOS 默认进驻菜单栏托盘（`--no-tray` 关闭） |
| `attach TTY [--name N]` | 单口采集（手动围观/测试，可接 socat PTY） |
| `list` | 列出当前设备与身份 |
| `pause [RE]` / `resume [RE]` | 暂停/恢复采集（省略 = 全部） |
| `proxy RE [--stop]` | **透明 USB 代理**：为匹配设备开 TCP 端点透传串口 —— 对业务程序等同直连，期间采集照常（可配 `proxy_tap_exclude` 剔除高频遥测落盘） |
| `release RE [--for 5m]` | **临时让出串口**给外部工具：默认端口空闲 3 秒自动回采，或限时自动回采 |
| `flash RE <bin>[@0x10000]...` | **代理刷固件**：让口 → esptool → 自动回采，进度流式回传；失败自动重试（`--retries`，默认 3 次 × `--retry-wait` 5s——Windows 上 USB-CDC 设备复位后首次 open/SetCommState 常瞬时失败，esptool 自身不重试）；`--args-file build/flasher_args.json` 一键刷 IDF 全套；`--dry-run` 预演将执行的命令。RE 为正则，**匹配多台时默认拒绝**（防误刷在测设备——多板同芯片时未锚定正则会把别的板拖进刷写序列，列出匹配设备并要求锚定），确要逐台刷给 `--all`，精确刷一台用锚定（如 `^board$`）。远程刷写见[控制协议 · SSH 隧道](docs/zh-CN/control-protocol.md#远程使用ssh-隧道) |
| `reopen RE [--all]` | **串口层软断开重连**：立即关口 → 跳过退避立即重开。端口疑似卡死（读空转/驱动状态怪异）时的快速自愈；不改变所有权与暂停语义（与 `release` 不同）。注意会打断进行中的透传会话（客户端重连即可），且 open/close 各带一拍复位脉冲（见[复位语义](#复位语义重要)——对 CH340/乐鑫原生 USB 口等于顺带软重启了板子）。多台门禁同 flash（`--all`） |
| `reset RE [--all]` | **USB 层软拔插**（仅 Windows）：让口 → `pnputil /restart-device`（禁用+启用设备节点，等效软件层面的拔插）→ 用自身枚举器确认重枚举 → 回采。作用于设备的串口接口节点，JTAG 等兄弟接口不受影响。适用于设备在总线但驱动/端口僵死（打不开、僵尸句柄）。需管理员权限：非提权守护进程自动弹 UAC 提权重试（可取消）。设备整个消失在总线上时无解（只能物理重插）。多台门禁同 flash（`--all`） |
| `status` | 守护进程与设备实时状态（collecting/paused/suspended/flashing） |
| `tray`（Windows） | 托盘常驻：接入状态、按设备暂停/恢复、打开日志，见上节（macOS 无独立 `tray` 子命令，`run` 自带菜单栏） |
| `analyze LOG...` | 离线签名扫描：计数 / 首末时间 / 样本行汇总表 |
| `decode-backtrace LOG` | `Backtrace:` 地址帧 addr2line 解码 |
| `version` | 打印版本号 |

flags（`--root/--baud/--config/--poll-ms/--exclude/--sock`）可写在位置参数前后任意位置。

## 文档

- [架构](docs/zh-CN/architecture.md) —— 包分层与依赖规则、采集器状态机、
  open-once-and-hold、release/flash/reopen 编排、可测性 seam 一览
- [CLI 参考](docs/zh-CN/cli-reference.md) —— 全部命令与 flag
- [配置参考](docs/zh-CN/configuration.md) —— 全部字段与默认值、设备命名链、
  PAUSED 文件、内置签名表、日志轮转
- [控制协议](docs/zh-CN/control-protocol.md) —— ctl socket 的 JSON 行协议完整
  语义（脚本化集成、SSH 隧道远程使用）
- [部署指南](docs/zh-CN/deployment.md) —— 安装包、systemd/launchd/任务计划、
  磁盘管理
- [贡献指南](CONTRIBUTING.md) —— 构建/测试/lint、TDD 与注入 seam、平台注意事项、发布流程

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
- `reopen`（串口层软重连）是**显式的手动例外**：使用者主动要求关口重开，
  这一拍复位是特性的一部分（对 CH340/乐鑫原生 USB 口等于软重启板子），
  而不是自动行为 —— 自动路径（断线重开循环）依旧遵守退避纪律

## 日志布局

```
<root>/<设备名>/serial-YYYYMMDD.log     # 全量，每行 [YYYY-MM-DD HH:MM:SS.mmm] 前缀
<root>/<设备名>/serial-YYYYMMDD.001.log # 超 rotate_max_mb 后轮转
<root>/<设备名>/events-YYYYMMDD.log     # 事件流：签名命中 + 采集器生命周期
<root>/PAUSED                            # 暂停清单（每行一个正则，mtime 热重载）
```

## 设备命名

优先级：配置 `names`（by-id 正则）→ 内置规则（`ch340` / `ch343` / `esp32s3-jtag`）
→ by-id 基名 → tty 名。同名设备（如两只乐鑫原生 USB-JTAG 都是
`303a:1001` → 都叫 `esp32s3-jtag`）自动加**身份派生后缀** `-<token>`：
token 是设备稳定身份（key/by-id，Windows 实例路径内嵌 MAC）的 4 位散列，
同一块板无论第几个接入、跨守护重启后缀都一致；裸基名先到先得。
**双板并存时请锚定后缀名**（如 `^esp32s3-jtag-1x2y$`），或用 by-id 里的
序列号/MAC 在配置 `names` 里给板子起语义名（见 `config.example.json`）。

配置文件默认路径 `~/.config/serialtap/config.json`（不存在则全默认值），
所有字段见 `config.example.json` 与[配置参考](docs/zh-CN/configuration.md)。

## macOS 说明

- **设备身份**：Linux 走 sysfs by-path；macOS 解析 `ioreg`（IOKit 注册表）——
  以 USB `locationID`（物理口）为 key，`usb-<vid>_<pid>[-<序列号>]` 为 by-id。
  by-id 风格与 Linux 对齐，配置里的 `names` 规则可跨平台复用；ioreg 只在
  端口集合变化（热插拔）时执行，稳态轮询零开销。只枚举 `/dev/cu.usb*`
  （蓝牙/wlan-debug 等本机串口天然滤除；采集用 cu.*，tty.* 在 mac 上 open 会
  等载波阻塞）
- **菜单栏托盘**：默认 CGO 构建包含托盘（fyne.io/systray）：设备实时状态、
  暂停/恢复全部、打开日志目录（Finder）、打开 Web 面板（默认
  http://127.0.0.1:8801/，面板关闭自动隐藏该项）、退出。`run --no-tray`
  走无头模式（SSH 远程 mac 场景）；`CGO_ENABLED=0` 构建自动无托盘
- **控制 socket**：默认 `/tmp/serialtap-$UID.sock`（BSD 的 unix socket 路径
  上限 104 字节，路径过长会明确报错）

## 离线分析

```bash
./serialtap analyze logs/esp32s3-jtag/serial-*.log
# SIGNATURE            COUNT  FIRST                   LAST                     SAMPLE
# reset-banner             3  2026-09-20 11:13:34.229 2026-09-20 11:11:21.165 ...

./serialtap decode-backtrace logs/esp32s3-jtag/serial-20260920.log
# addr2line: ~/.espressif/tools/.../xtensa-esp32s3-elf-addr2line
# elf:       由配置 elf_map 按设备名自动选取（也可 --elf 显式指定）
```

## 常驻部署

Linux（systemd 用户级服务）：

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
macOS / Windows 无需权限配置——launchd agent 与 Windows 安装包/任务计划
路径见[部署指南](docs/zh-CN/deployment.md)。

## 故障排查

- **代理刷写报"端口忙"**：多半是另一个 serialtap 实例（比如旧检出目录里
  起的 demo 守护）占着口。`Get-CimInstance Win32_Process -Filter "name='serialtap.exe'" | select ProcessId,CommandLine`
  找到后结束它；本守护的采集器会按退避自动接管。
- **代理端点连上但收不到设备数据**：TCP Dial 在 accept/挂接完成前就返回，
  设备→客户端方向在挂接前的字节不镜像（客户端→设备方向有内核缓冲不受影响）。
  应用层先握手（如 homepulse 的 sense_start/ack）即可规避。
- **`open failed: ... device busy`** | 串口被其他进程占用（EBUSY），错误详情在 events 日志。`lsof /dev/ttyUSB0` 找占用者，或用 `release` 正规让口
- **启动报"控制 socket 已被另一个 serialtap 实例占用"** | 已有活实例在跑（防抢占保护）。查 `systemctl --user status serialtap`；确要并行实例，配不同 `control_socket`（`run --sock`）与不同日志 `root`
- **`flash`/`release`/`status` 连不上守护进程** | `serialtap run` 未运行，或 socket 路径不对（解析规则见[控制协议](docs/zh-CN/control-protocol.md)；Windows 默认在 `%LOCALAPPDATA%\serialtap\`）
- **Windows 上 `release` 报"不支持空闲自动回采"** | 预期行为：Windows 无 /proc/lsof 占用检测。用 `release RE --for 5m` 限时回采，或刷完后 `resume`
- **正则匹配不到设备** | 各平台的 tty/key/by-id 形态不同（见"平台支持"节），先 `serialtap list` 看实际字段值
- **设备名带 `-xxxx` 散列后缀** | 同名设备撞车（同型号板 by-id 无序列号，如两只原生 USB-JTAG）。后缀从设备稳定身份派生、跨重启不变——双板并存请锚定后缀名，或用配置 `names` 按 by-id 序列号/MAC 细分命名
- **`pause` 后一直不采集** | `cat <root>/PAUSED` 看清单内容；`serialtap resume`（无参）清空全部
- **板子反复重启** | 检查是否开了 `silent_reopen_s` 看门狗 —— 合法安静的设备会被它复位循环，保持默认 `0`
- **`analyze` 看不到自定义签名** | 已知限制：离线扫描只统计内置签名表，`signatures_extra` 仅影响在线事件流
- **插上设备却没被采集** | 用户不在 `dialout`（Debian 系）/`uucp`（Arch）组；或被 `exclude` 正则命中（检查 tty/by-id/名字）

## 开发

详见[贡献指南](CONTRIBUTING.md)与[架构文档](docs/zh-CN/architecture.md)。

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
internal/device/           # 设备发现与稳定身份（Linux sysfs/by-path、Windows 注册表、macOS ioreg）
internal/collector/        # 单设备采集器（open-once-and-hold、可注入 Port seam）
internal/logstore/         # 双通道日志写入 / 轮转 / 保留期清理
internal/signature/        # 错误签名引擎
internal/pause/            # 刷写暂停清单
internal/daemon/           # 热插拔守护循环（枚举 diff + 起停采集器 + release/flash/reopen/reset 编排）
internal/flash/            # 代理刷固件（esptool 编排 + flasher_args.json 解析）
internal/ctl/              # 控制 unix socket（JSON 行协议）
internal/web/              # 观测与操作面板（SSE 实时尾随、刷机上传、命令转发）
internal/tray/             # 托盘/菜单栏（darwin+cgo 内嵌于 run；Windows 托盘进程；其余平台桩）
internal/analyze/          # 离线分析（签名汇总 + addr2line 解码）
internal/testutil/         # 跨包测试助手（假串口等）
```

- TDD 开发，假端口注入驱动采集器全链路离线测试 + socat PTY 端到端
- 依赖已 `go mod vendor`：`go.bug.st/serial`（arduino-cli 同款串口库，纯 Go）
- Go 1.27+

## License

GPL-3.0-or-later，见 [LICENSE](LICENSE)。
