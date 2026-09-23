# Changelog

## Unreleased

- **macOS 支持**：设备枚举移植 —— 解析 `ioreg`（IOKit 注册表）取身份，
  USB `locationID` 为物理口 key（对齐 Linux by-path 语义），
  by-id 采用 `usb-<vid>_<pid>[-sn]` 风格与 Linux 配置规则互通；
  只枚举 `/dev/cu.usb*`，ioreg 仅在端口集合变化时执行（稳态零开销）；
  ioreg 失败退化为 tty 名身份，不阻塞采集
- **macOS 菜单栏托盘**：`run` 默认进驻菜单栏（fyne.io/systray，darwin+cgo）——
  实时设备状态、暂停/恢复全部采集、打开日志目录、退出；`run --no-tray` 无头模式；
  Linux/无 CGO 构建走桩实现，纯静态二进制承诺不变
- fix(ctl): unix socket 路径超长（BSD 104 字节上限）提前拦截并给出明确报错，
  此前 bind 只报 `invalid argument` 无法排查；测试路径在 darwin 上改用 /tmp 短路径

## v0.1.0 (2026-09-20)

首个公开发布。

### 核心功能

- 热插拔自动采集：轮询发现 USB 串口（by-path 物理口身份，重枚举/同型号适配器不串台），
  每设备一个采集协程，插上即采、拔走即停
- 双通道日志：`serial-YYYYMMDD.log` 全量（毫秒级逐行时间戳）+
  `events-YYYYMMDD.log` 事件流（签名命中 + 采集器生命周期），按日 + 按大小轮转，
  保留期自动清理
- 错误签名引擎：ESP-IDF 常见故障行内置（`rst:0x`、`E (`、lwIP `accept (n)`、
  Guru Meditation、WDT、Backtrace 等），`signatures_extra` 正则扩展
- 刷写安全门：`pause`/`resume` 暂停清单，USB 刷机前让出串口
- **代理刷固件**：`flash` 一条命令完成让口 → esptool → 自动回采，进度流式回传；
  支持 `bin@offset` 多镜像与 ESP-IDF `flasher_args.json`
- **临时让口**：`release`（端口空闲 3s 自动回采 / `--for` 限时回采），
  经守护进程 unix socket 控制通道；`status` 查实时设备状态
- 离线分析：`analyze` 签名汇总（计数/首末时间/样本）、
  `decode-backtrace` addr2line 解码（自动发现 ESP-IDF 工具链，`elf_map` 按设备名配）
- 零丢失：跨块行拼装，端口关闭时残余半行以 `…partial` 标记落盘

### 设计纪律

- **open-once-and-hold**：open/close 串口都会给 CDC 设备一拍复位脉冲
  （含 ESP32-S3 原生 USB-JTAG，`rst:0x15` 实测），采集器每设备只 open 一次并持有
- **静默看门狗默认关**：只对保证有周期日志的设备显式开启，避免安静设备被复位循环

### 质量

- 测试覆盖率 87.3%，`-race` 干净；假端口注入驱动采集器全链路离线测试
- CI：golangci-lint + 测试（80% 覆盖门禁）+ 构建
- 实机验证：CH340 / CH343 / ESP32-S3 原生 USB-JTAG 三类适配器，
  systemd 用户级服务长期运行
