# 架构

本文面向贡献者与想深入理解 serialtap 的用户,描述模块划分、数据流与关键设计决策。
用户视角的用法见 [README](../README.md);配置项详见[配置参考](configuration.md);
控制 socket 的 JSON 行协议详见[控制协议](ctl-protocol.md)。

## 总览

```
                      serialtap run(守护进程)
    ┌────────────────────────────────────────────────────────┐
    │                                                        │
    │  ┌────────┐ 枚举 diff,起停  ┌───────────────────────┐  │       /dev/ttyUSB*
────┼─►│ daemon │───Suspend/─────►│ collector(每设备1协程) │──┼──────► USB 串口设备
    │  │        │    Resume       │ open-once-and-hold    │  │
    │  └──┬─────┘                 └──────────┬────────────┘  │
    │     │ tickReleases                     │ 双通道写入     │
    │     │ PAUSED 热重载               ┌────▼─────┐          │
    │  ┌──▼───┐  status/pause/          │ logstore │──────────┼─────► logs/<name>/
    │  │ ctl  │  resume/release/        └──────────┘          │        serial-*.log
    │  │server│  flash                                        │        events-*.log
    │  └──────┘                                              │
    └────────────────────────────────────────────────────────┘
     ▲
     │ unix socket + JSON 行协议
   CLI 子命令(flash / release / status / pause / resume)
```

纯 Go、无 CGO,唯一的非标准库依赖是 `go.bug.st/serial`(已 vendor;
`golang.org/x/sys` 为直接依赖,Windows 注册表枚举用)。**三平台支持**(Linux/macOS/Windows),
平台差异全部收敛在 `internal/device` 与 `internal/ctl` 的 build-tag 分文件里:

| 平台 | 设备发现(Enumerate) | 稳定 Key | 端口占用检测(PortHolders) |
|---|---|---|---|
| Linux | sysfs + `/dev/serial/by-{id,path}` 符号链接 | by-path 基名(物理口) | 扫 `/proc/*/fd` |
| Windows | `SERIALCOMM` 活口清单 + 注册表 `Enum\USB` 树(VID/PID/实例) | USB 实例 ID(有序列号=序列号;无序列号=位置哈希,同型号不撞车) | 不支持(见下) |
| macOS | `/dev/cu.*` 设备名前缀过滤(无纯 Go IOKit 途径) | `cu.*` 基名(无序列号设备按 USB 位置编号,有序列号设备内嵌序列号) | `lsof -t` |

控制通道在三个平台都是 unix socket(Windows 10 1803+ 的 AF_UNIX;Go 原生支持),
默认路径按平台解析:`$XDG_RUNTIME_DIR/serialtap.sock` → `/tmp/serialtap-<uid>.sock`(Linux/macOS)、
`%LOCALAPPDATA%\serialtap\serialtap.sock`(Windows)。

**Windows 无空闲检测的原因**:查"谁开着这个口"需要 /proc(Linux)或 lsof(macOS);
Windows 上唯一的纯 Go 途径是试开端口 —— 而 open 会给 CDC 设备一拍复位脉冲,
违反本项目最核心的纪律。因此 `IdleDetectSupported()` 在 Windows 返回 false,
`release` 的 until_idle 模式被显式拒绝(用 `--for` 限时回采),绝不静默误判。

## 包分层与依赖规则

`main.go` 只做 `os.Exit(cli.Run(...))`。全部实现都在 `internal/` 下,依赖单向、无环:

| 包 | 职责 | 依赖 |
|---|---|---|
| `cli` | 子命令分发、flag 解析、各命令编排(唯一知道"全部包"的层) | 几乎全部 |
| `config` | 配置定义与加载(默认值 → JSON 覆盖 → 零值回填) | — |
| `device` | 设备发现与稳定身份(by-path key、命名链、VID:PID) | config |
| `collector` | 单设备采集 goroutine、行拼装、暂停/让出响应 | config, device, logstore, pause, signature |
| `logstore` | 双通道日志写入、按日/大小轮转、保留期清理 | — |
| `signature` | 错误签名引擎(内置 ESP-IDF 故障行 + 配置追加) | — |
| `pause` | PAUSED 暂停清单文件(读、写、热替换) | device |
| `daemon` | 热插拔守护循环、release/flash 编排 | collector, device, logstore, pause, signature, ctl, flash |
| `flash` | esptool 命令组装与流式执行、flasher_args.json 解析 | — |
| `ctl` | 控制通道:unix socket 服务端与客户端(JSON 行协议) | flash |
| `analyze` | 离线分析:签名汇总、Backtrace addr2line 解码 | config, signature |
| `testutil` | 跨包测试助手(FakePort 等);**只被测试导入** | — |

两条隐含边界规则:

- `collector` 不反向依赖 `daemon`。程序化让出/收回是 collector 自己暴露的
  `SuspendResume` 接口(`Suspend/Resume/Held/SetFlashing/State`),daemon 调用它。
- "按正则匹配设备"的语义(tty / 设备名 / key / by-id 任一命中)在
  `daemon.matches`、`pause.PauseState.Matches`、`device.matchAny` 三处保持一致,
改动任一处必须同步另外两处。

## 设备身份模型

`device.DeviceInfo` 是身份的唯一权威:

| 字段 | 含义 | 用途 |
|---|---|---|
| `Key` | `/dev/serial/by-path` 基名(**物理 USB 口**) | 采集器的 map key —— 重枚举换 tty 号不变、同型号适配器不撞车 |
| `Name` | 日志目录名 | 命名链决定(见下) |
| `ByID` | `/dev/serial/by-id` 基名 | 可读身份;CH340 无序列号,同型号会重复,只能展示/命名用 |
| `Tty` | `/dev/ttyUSB0` 等 | 实际打开的设备路径,枚举顺序变化会漂移 |

为什么 Key 用 by-path(Linux):tty 编号随枚举顺序漂移;CH340 的 by-id 不含序列号,
插两只一模一样的适配器 by-id 完全相同;只有物理口是恒定且唯一的。
Windows 的 USB 实例 ID 与 macOS 的 `cu.*` 命名达到同样的"同型号不撞车、换口即换身份"
语义(Windows 有序列号的设备换口不换身份,日志续写 —— 语义上的小增益)。
没有 USB 身份的口(主板 `ttyS*`、PCIe 串口、非 USB 串口)在各平台都被枚举过滤。

命名链(先命中先用):配置 `names`(by-id 正则)→ 内置 by-id 规则
(`USB_Serial-if00`→`ch340`、`USB_Single_Serial`→`ch343`、`Espressif_USB_JTAG`→`esp32s3-jtag`)
→ 内置 VID:PID 规则(`1a86:7523`→`ch340`、`1a86:7522`/`1a86:55d3`→`ch343`、
`303a:1001`→`esp32s3-jtag`;Windows 枚举层提供 VID:PID,是三平台通用的定型途径)
→ by-id 基名 → tty 名。守护进程内同名设备自动加 `-2`、`-3` 后缀。
所有名字最后过 `device.SanitizeName`(非法字符→`-`、最长 64、空回退 `dev`),
它是"设备名 → 目录名"的唯一规范化入口。

## 采集器(collector)

每设备一个 goroutine,由 daemon 启动并在移除/退出时停止(WaitGroup 追踪防泄漏)。

### open-once-and-hold(最核心的纪律)

**open 和 close 串口都会给板子一拍复位脉冲**,这是宿主侧 CDC 握手线行为,用户态无法避免:

- CH340/CH343 适配器驱动在 open() 时断言复位线,pyserial 的 `rts=False/dtr=False` 拦不住
- ESP32-S3 原生 USB-JTAG 的 USB_SERIAL_JTAG 外设在硅内实现了同款语义
  (实测 open 后 3ms 出现 `rst:0x15 (USB_UART_CHIP_RESET)`),close 后设备重启

因此采集器的纪律是**每物理设备恰好 open 一次并长期持有**,绝不周期性重开:

- 热插拔接入:设备刚上电,这一拍复位无感
- `pause` / release / flash 让口:反正 esptool 本来就要复位,无害
- 静默看门狗(`silent_reopen_s`)**默认关**:read 空转不报错是 fd 挂死的一种形态,
  看门狗在静默超阈值时强制重开 —— 但只对保证有周期日志输出的设备(如 30s 心跳)开启,
  否则合法的安静设备会被复位循环打死

配套细节:

- open 后立即 `SetDTR(false)` + `SetRTS(false)`:CH340 的 RTS 接 EN,不释放则板子被
  按在复位态,0 字节输入;对其他板无副作用
- 1s 读超时:作为检查点,让读循环能响应 stop / 暂停 / 看门狗
  (注意 `go.bug.st/serial` v1.8 的 API 是 `time.Duration`,传裸数字会变成纳秒级忙轮询)
- **读错误立即放弃 fd**:USB 重新枚举(拔出)在 read 抛错,恋战死 fd 会错过
  "设备为何重启"这段最贵的诊断数据

### 读循环与零丢失

```
port.Read(4096B 块)
  → lineAssembler.feed()        跨块拼行;残余半行留在内部
  → logstore.WriteLine(line)    全量通道,加毫秒时间戳
  → signature.Match(line)       命中 → logstore.WriteEvent("[签名名] 行内容")
```

- 无换行的二进制泥石流保护:拼装缓冲超过 64 KiB 时整块吐出一行,不无限膨胀
- 端口关闭(stop / 暂停 / 读错误 / 看门狗)时,残余半行以 `…partial ` 前缀落盘 ——
  这是"零丢失"承诺的最后一环

### 退出原因与重开退避

| 退出原因 | 触发 | 后续 |
|---|---|---|
| `stopped` | 设备拔走被 daemon Stop / 进程退出 | 采集器结束 |
| `paused` | PAUSED 文件或程序化让出生效 | 外层循环等待恢复 |
| `open-failed` | open 报错(已拔走、EBUSY 被占) | 指数退避重试,错误详情进事件流 |
| `read-error` | read 报错(通常为拔出) | 退避后重开 |
| `silent-watchdog` | 静默超阈值(默认关) | 退避后重开 |

退避从 `reopen_min_s`(默认 5s)起,每次 ×2 封顶 `reopen_max_s`(默认 60s);
**任何成功 open 过的会话退出都把退避复位回下限**(issue #4:否则长期运行中偶发断连
会把退避棘轮到上限,之后每次瞬断都白等)。

### 四个展示态

collector 内部维护三态 `collecting | suspended | flashing`(程序化让出通道,
`SuspendResume`);PAUSED 文件命中是叠加在 collecting 上的第四展示态 `paused`。
`daemon.Status()` 的最终输出:state 为 collecting 且 PAUSED 命中 → 报 `paused`。

`Suspend(timeout)` 会**等到端口真正关闭**才返回 —— 调用方(esptool 等)拿到返回即可
独占端口。release 场景超时 5s,flash 场景超时 10s。

## 守护循环(daemon)

`run` 命令的主循环每 `poll_interval_ms`(默认 1s)执行一轮 `Tick`:

1. **枚举 diff**:`device.Enumerate`(注入点,测试可替换)→ 新 Key 起采集器、
   消失的 Key 停采集器并释放设备名
2. **PAUSED 热重载**:`root/PAUSED` 的 mtime 变了才重读、热替换模式表;
   文件被删除则清空(`attach` 单口模式不热重载,只在启动时读一次)
3. **tickReleases**:处理 release 的自动回采 —— 限时(`for`)到期回采;
   空闲模式连续 3s 没有其他进程持有该口则回采(占用检测:Linux 扫 `/proc/*/fd`,
   macOS 走 `lsof`,均只读、绝不碰端口本身;Windows 不支持空闲模式,
   `Release` 在入口即拒绝)

`ResumeAll`(即 `resume` 命令):清 PAUSED 匹配条目(无模式 = 清空整个文件,
与无参 pause 对称)+ 撤销全部 release 并立即 Resume。

保留期清理(`SweepRetention`)在启动时和每小时执行:删除 `root/*/*.log` 中
mtime 早于 `retention_days` 的文件;`retention_days <= 0` 永久保留。

## 代理刷固件(daemon/flash.go + flash 包)

`flash` 命令经 ctl socket 到守护进程,**避免 esptool 与常驻采集器抢口把 ESP32
楔进 ROM 下载模式**。对每个匹配设备(正则,多设备逐台刷):

```
opMu.TryLock            与 Release/ResumeAll 互斥(fail-fast,不排队)
撤销 pending release    防限时到期在 esptool 工作中途抢回口
flash.Plan              解析 Spec → 完整 esptool argv,进 events 审计
Suspend(10s)            等端口真正关闭
SetFlashing(true)       状态展示 → flashing
flash.Run(esptool...)   执行 esptool(flash_timeout_s 超时兜底,挂死即杀),
                        stdout/stderr 按行流式回传(\r 与 \n 都算行界,
                        esptool 进度条用 \r 刷新;两路扫描协程经互斥锁串行化防交错)
SetFlashing(false)
Resume()                自动回采
```

`--dry-run`(Spec.DryRun)只走 Plan 分支:守护进程侧解析并回显将执行的命令,
不动端口、不执行、不改状态 —— 用于多设备正则刷写前预演"会刷哪几台、命令是什么"。

esptool 命令行组装:`esptool --port <tty> [--chip <c>] [--baud <n>] write_flash
<offset> <bin>...`。`--args-file` 指向 ESP-IDF `build/flasher_args.json` 时:
`flash_files` 的 offset→路径按 offset 升序展开(相对路径基于 args 文件所在目录),
`extra_esptool_args` 只取 string 值(如 `--chip`);布尔/数字值(stub/trace)走默认。
esptool 本体发现顺序:显式指定 > PATH 里的 `esptool`/`esptool.py` >
`~/.espressif/python_env/*/bin|Scripts/esptool(.exe)` glob(尊重 `IDF_TOOLS_PATH`,
多命中取排序最后一个)> Windows 的 pip --user 目录。

## 控制通道(ctl)

unix socket + JSON 行协议,详见[控制协议](ctl-protocol.md)。要点:

- 默认路径 `$XDG_RUNTIME_DIR/serialtap.sock`,回退 `/tmp/serialtap-<uid>.sock`;
  权限 0600,同用户专用
- **socket 防抢占**:路径已存在时先 dial 探测 —— 活实例持有则拒绝启动第二个
  `run`(防止偷走控制通道);只有死文件(上次异常退出)才清理接管
- `pause`/`resume` 子命令走"socket 优先":守护进程在 → 经 socket(立即生效);
  不在 → 回退为直接编辑 PAUSED 文件(历史行为)

## 可测性设计(seam 一览)

| seam | 注入方式 | 用途 |
|---|---|---|
| `collector.OpenPort` | 包级变量,测试整体替换 | `testutil.FakePort` 驱动采集器全链路离线测试 |
| `daemon.New(enum, logf)` | 构造参数 | 假枚举函数注入守护循环 |
| `device.buildDevices(...)` | 纯函数参数 | 端口清单 / by-id / by-path / VID:PID 全部注入 |
| `device.usbSerialMetaAt(rootKey, path)`(Windows) | 根键+路径参数 | HKCU 假 Enum 树测注册表扫描 |
| `device.PortHoldersAt(procRoot, ...)`(Linux) | procRoot 参数 | 假 `/proc` 树测空闲检测 |
| `ctl.Server.Serve(Handler)` | Handler 回调 | 不起真 socket 测协议分发 |
| `logstore` 时间 | `ensureDayLocked(now)` 入参 | 跨日轮转测试 |
| `testutil.FakeTool()` | 编译型假外部命令 | esptool/addr2line 替身(shell 脚本在 Windows 不可执行),行为由 FAKE_EXIT/FAKE_OUT 环境变量驱动 |

另有 fuzz 测试:`collector` 的行拼装器(`assembler_fuzz_test.go`)与
`analyze` 的 Backtrace 地址提取(`backtrace_fuzz_test.go`)。
端到端用 socat PTY 模拟真串口(Linux)。
