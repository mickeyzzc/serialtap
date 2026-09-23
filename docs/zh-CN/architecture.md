# 架构

[English](../en/architecture.md) | [简体中文](architecture.md)

serialtap 是一个 Linux 守护进程，把"一台主机上插着一堆 USB 串口板"变成
"一个目录下持续采集、带时间戳、可轮转的日志" —— 并且从不与其他工具抢串口。
本文说明它是怎么建的，更重要的是那些麻烦的地方*为什么*长这样。

## 包结构

依赖单向，自编排层向下：

```
main.go                    薄入口：os.Exit(cli.Run(...))
   │
internal/cli/              子命令分发、flag 解析、命令编排
   ├── internal/config/        配置结构体 + JSON 加载
   ├── internal/device/        设备发现 + 稳定身份（by-path key、by-id 命名、sysfs VID:PID）
   ├── internal/daemon/        热插拔循环：枚举 diff、起停采集器、release/flash 编排
   │     ├── internal/collector/   每设备一协程：open-once-and-hold、行拼装、挂起/恢复
   │     ├── internal/logstore/    双通道文件、轮转、保留期清理
   │     ├── internal/signature/   错误签名引擎
   │     ├── internal/flash/       esptool 命令构建/执行、flasher_args.json 解析
   │     └── internal/pause/       PAUSED 文件 + 模式状态
   ├── internal/ctl/           控制 unix socket 服务端/客户端（JSON 行）
   └── internal/analyze/       离线：签名汇总 + addr2line 解码

internal/testutil/          跨包测试助手（假串口、等待/读文件等）
```

## 数据流（守护模式）

```
每 poll_interval_ms:
  Enumerate()                       ← /dev/serial/by-{id,path} + sysfs VID:PID
    与在线采集器按 by-path key 求 diff
      新 key     → 建 DeviceWriter + 采集器协程
      消失的 key → 停采集器、释放设备名
  PAUSED 文件 mtime 变化则热重载
  处理 release 到期（限时/空闲检测）

采集器协程（每设备一个）:
  open 端口（仅一次）→ 释放 DTR/RTS → 读循环
    以 '\n' 结束的行 → WriteLine 进全量通道 + 签名匹配
    签名命中          → WriteEvent 进事件通道
  出错/暂停/挂起时：落盘半行、关端口、退避、重试
```

## 稳定身份：by-path，而不是 by-id 或 tty 名

tty 编号按枚举顺序分配 —— 设备重插或邻居消失都会变。by-id 只有在适配器
有序列号时才稳定：两只同型号 CH340 的 by-id 字节级相同，光靠 by-id 分不出
同型号的两块板。

因此 serialtap 以 **by-path**（物理 USB 口）作为设备 key。设备身份在重枚举
后不变，插在不同口的两只同型号适配器是两个不同的 key。by-id 仍作*展示/命名*
身份（人类可读，可能内嵌序列号/MAC）。设备发现要求 by-path 条目存在，这同时
滤掉了非 USB 串口（主板 `ttyS*`）。

VID:PID 从 sysfs 读取：从 `/sys/class/tty/<tty>/device` 向上最多走 4 层，
找到含 `idVendor`/`idProduct` 的目录。

## 复位语义（open-once-and-hold）

**open 或 close USB 串口设备都会给板子的复位线一拍脉冲。** 适配器芯片
（CH340/CH343…）的驱动在 open 时断言复位线；ESP32-S3 原生 USB_SERIAL_JTAG
外设在硅内实现了同样的自动复位语义（实测：open 后 3ms 出现
`rst:0x15 (USB_UART_CHIP_RESET)`，close 后设备重启）。这是宿主侧 CDC 握手
行为，用户态拦不住（pyserial 的 `rts=False/dtr=False` 也没用）。

采集器把这些结论代码化：

1. **每物理设备恰好 open 一次并长期持有。** 绝不周期性重开。热插拔接入时
   设备刚上电，这一拍无感；刷机前 `pause` 会复位板子 —— esptool 本来就要
   复位，无妨。
2. **open 后立即释放 DTR/RTS。** CH340 接线的板上 RTS 接 EN；不释放的话板子
   被按在复位态，0 字节输入。其他板无副作用。
3. **静默看门狗默认关。** fd 挂死的一种形态是 `read` 永远返回 0 且不报错；
   看门狗在静默 N 秒后强制重开能救回来 —— 但它也会每 N 秒把合法安静的板子
   复位一次。所以 `silent_reopen_s = 0`，除非你确定设备有周期输出
   （如 30s 心跳）。
4. **读错误立即放弃 fd。** USB 重新枚举表现为读错误；恋战死 fd 会错过设备
   重启 —— 最贵的一类诊断数据丢失。重连按指数退避（`reopen_min_s` 翻倍到
   `reopen_max_s`）；任何成功 open 过的会话结束后退避复位，一次偶发断连
   不会把退避棘轮到上限。

## 零丢失行拼装

串口数据按任意分块到达，不看行边界。采集器的行拼装器把尾部缓冲跨读取保留，
只在 `\n` 到达时吐出一行（剥掉 `\r`）。端口关闭时若还有半行未结束，尾部以
`…partial` 前缀落盘，绝不静默丢弃。连续 64 KiB 无换行的异常流会整块作为
一行吐出，而不是无界缓冲。

## 双通道日志、轮转、保留期

每设备在 `<root>/<name>/` 下：

- `serial-YYYYMMDD.log` —— 全量，每行前缀 `[YYYY-MM-DD HH:MM:SS.mmm]`
- `events-YYYYMMDD.log` —— 签名命中（`[签名] 前 200 字符`）+ 采集器生命
  周期事件（`[collector <name>] …`）

轮转：日期变更开新文件对；超过 `rotate_max_mb` 加 `.001`、`.002`… 后缀
（两个通道都有）。守护进程重启后，续写当日既有文件，且后缀计数从既有最大
编号续起，重启绝不与早先的轮转撞车。保留期清扫（启动 + 每小时）按 mtime
删除早于 `retention_days` 的 `*.log`。

## pause / release / flash 编排

三层"不采集"状态，都为了让外部工具能安全用口：

1. **PAUSED 文件**（`<root>/PAUSED`，每行一个正则）。模式级、跨重启持久、
   mtime 变化热重载。采集器关闭串口并轮询等待，直到模式移除。
2. **release**（采集器上的 `Suspend`/`Resume`）。程序化、按设备、经控制
   socket。`Suspend` 直到端口*真正关闭*（或超时）才返回，调用方即可独占
   打开。回采要么限时（`--for`），要么空闲触发：守护进程扫描 `/proc/*/fd`
   找该 tty 的其他持有者，连续 3 秒无人持有即恢复 —— 扫描只读符号链接、
   绝不碰端口本身，探测动作不会发出复位脉冲。
3. **flash** —— 守护进程对每台命中设备串起上述原语，逐台执行：
   `Suspend`（10 秒预算）→ 标记 `flashing` → 运行 esptool（其 stdout 与
   stderr 经控制 socket 流式回传；`\r` 也算行界，进度条能透过来）→
   `Resume`。esptool 失败时采集器同样恢复。

`resume`（控制命令）一次清掉匹配的 PAUSED 条目*并*撤销未到期的 release。

## 控制 socket

unix 流式 socket，双向每行一个 JSON 对象；见
[控制协议](control-protocol.md)。socket 权限 0600 —— 仅同用户。有活实例
持有 socket 时第二个 `serialtap run` 拒绝启动；崩溃残留的死文件自动清理。

## 测试策略

- **端口 seam**：`collector.OpenPort` 是包级变量；测试换成
  `testutil.FakePort`（脚本化的数据块、close/DTR/RTS 跟踪），整条
  读取→拼装→写盘→签名链路离线跑通，无需硬件。
- **注入边界**：守护进程的枚举函数与日志器是构造参数 —— 测试喂合成的设备
  清单；logstore 测试在临时目录上覆盖轮转与保留期。
- **模糊测试**：行拼装器与 Backtrace 地址解析器有 fuzz 目标。
- **端到端**：socat PTY 对在 Linux 上让真实采集器跑真实（虚拟）串口。
- CI 门禁：golangci-lint、`go test -race`、80% 覆盖率。

## 平台支持

设备身份建立在 sysfs 与 `/dev/serial/by-path` 之上，所以守护（`run`/`list`）
与端口持有者检测**仅 Linux** —— 非 Linux 平台显式报错，而不是返回空设备表。
代码可交叉编译到任何平台（CI 构建 darwin/windows 作冒烟检查），`attach`/
`analyze`/`decode-backtrace` 也无 Linux 特有依赖，但官方支持与发布二进制
仅 Linux amd64/arm64。
