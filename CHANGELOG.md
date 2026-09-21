# Changelog

## Unreleased

### Windows 托盘常驻（`serialtap tray`）

- 每台设备一个菜单项，**勾选框即"接入开关"**（勾选 = 采集中，点击即暂停/恢复该设备）
- 设备子菜单：查看串口日志（默认编辑器打开最新全量日志）、打开日志目录
- 全部暂停 / 全部恢复；守护进程未运行时菜单一键启动（后台无窗口）
- 悬停实时设备数；图标变灰 = 守护未连接；状态轮询带 3s 超时防被挂死守护拖死
- 与守护进程只经 ctl socket 通信；图标为程序化生成的 ICO（无外部资源文件）
- 新增依赖 `getlantern/systray`（Windows 侧纯 Win32 调用，无 CGO）

### 跨平台（真机实测修复）

- **复合设备识别**：ESP32 原生 USB（ESP32-C3 实测）的串口挂在注册表
  `VID_x&PID_y&MI_00` 接口子键下而非设备键下 —— 枚举已支持复合形态，
  且 Key 优先取父设备实例（MAC 序列号），换口不换身份、日志续写

### 代理刷固件强化

- **esptool 自动发现**：PATH → `~/.espressif/python_env` glob（尊重 `IDF_TOOLS_PATH`，
  多命中取最新）→ **eim 安装管理器布局**（解析 `eim_config.toml` 的工具根，实测
  `C:\Espressif\python_env`）→ Windows pip --user 目录 —— 未 source ESP-IDF 环境也能刷
- **并发互斥**：`flash`/`release`/`resume` 三者互斥，进行中立即报错（fail-fast）——
  修复两个 flash 或 flash+release 同时到达时双双让口、两个 esptool 抢同一口的竞态；
  resume 在刷写中途抢回口也被拒绝
- **撤销 pending release**：刷写开始时撤销匹配设备的限时 release，防止到期中途抢口
- **刷写超时兜底**：`flash_timeout_s`（默认 600s，显式 0 关闭）超时杀 esptool 进程
  并自动回采，挂死进程不再永远持有串口
- **`flash --dry-run`**：守护进程侧解析并回显每台匹配设备将执行的 esptool 命令，
  不动端口 —— 多设备正则刷写前预演
- **ctl 错误退出码**：`status`/`release`/`pause`/`resume` 收到服务端 ok:false 时
  退出码改为非零（此前只打 stderr 却退出 0，脚本化会误判成功）；`pause`/`resume`
  被服务端拒绝时不再回退文件直改模式、不再谎报成功
- events 审计：每次刷写记录完整 esptool 命令行（flash plan）
- 文档：远程刷写（SSH 隧道转发 ctl socket）用法

### 跨平台：Windows / macOS 支持

- **设备发现**：Windows 走注册表（`SERIALCOMM` 活口清单 +
  `Enum\USB` 树的 VID/PID/实例 ID），macOS 走 `cu.*` 设备名；
  稳定身份语义与 Linux by-path 一致（同型号不撞车、换口即换身份）
- **内置 VID:PID 命名规则**（ch340/ch343/esp32s3-jtag）三平台通用，
  by-id 规则未命中时套用
- **控制通道**：三平台统一 unix socket（Windows 10 1803+ AF_UNIX）；
  Windows 默认路径 `%LOCALAPPDATA%\serialtap\serialtap.sock`（父目录自动创建）
- **release 空闲自动回采**：Linux（/proc）与 macOS（lsof）支持；
  Windows 显式拒绝并提示改用 `--for` 限时回采（试开端口会给设备复位脉冲，不做）
- **测试全面跨平台**：假外部命令改为编译型（shell 脚本在 Windows 不可执行）；
  修复 5 个包的 Windows 失败用例；CI 增加三平台测试矩阵
- `flash` 刷写失败现在返回非零退出码（此前只打 stderr，脚本化会误判成功）
- Release 产物扩展到 linux/darwin/windows × amd64/arm64

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
