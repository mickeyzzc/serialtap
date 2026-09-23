# CLI 参考

[English](../en/cli-reference.md) | [简体中文](cli-reference.md)

所有命令都在唯一的 `serialtap` 二进制里。不带参数运行 `serialtap` 会打印
内置 usage 帮助（其中也含版本号）。

## 约定

**退出码**

| 码 | 含义 |
|---|---|
| `0` | 成功 |
| `1` | 运行时错误（错误信息打到 stderr，前缀 `错误:`） |
| `2` | 用法错误：缺少/未知子命令 |

**flag 可写在任意位置。** Go 的 `flag` 包遇到第一个位置参数就停止解析，
serialtap 循环剥离位置参数绕开了这一点，所以 `attach /dev/ttyACM0 --name x`
和 `attach --name x /dev/ttyACM0` 都可用。

**配置解析**（对接受 `--config`/`--root`/`--baud` 的命令）：

1. 给了 `--config FILE` 就用它；否则用 `~/.config/serialtap/config.json`，
   且仅当文件存在时才加载
2. 文件不存在则全部走内置默认值
3. `--root` / `--baud` 命令行参数覆盖配置文件中的对应值

各字段详见[配置参考](configuration.md)。

**设备匹配模式（`RE`）是正则。** 凡是接受设备模式的命令，模式都是 Go 正则，
与设备的任一身份做匹配：tty 路径（`/dev/ttyUSB0`）、设备名（`ch340`、
`board-a`…）、by-path key（物理 USB 口）、by-id 字符串。**未锚定的正则按
子串匹配** —— `usb` 会命中所有 ttyUSB 设备。要精确匹配一台请加锚定
（`^board-a$`）。对 `flash` 尤其重要：它会逐台刷写所有命中的设备。

---

## `run` —— 守护模式

```
serialtap run [--config F] [--root DIR] [--baud N] [--exclude RE]... [--poll-ms N]
              [--sock PATH] [--web ADDR] [--no-tray]
```

每 `poll_interval_ms` 轮询一次 USB 串口，每设备起一个采集协程，拔走即停。
同时负责：

- 提供**控制 socket**，服务 `status`/`pause`/`resume`/`release`/`flash`/
  `proxy`/`reopen`/`reset`（见[控制协议](control-protocol.md)）
- 启动 **Web 观测面板**（默认 `127.0.0.1:8801`，见 `--web`）
- 启动时与每小时清扫过期日志（`retention_days`）
- `PAUSED` 文件 mtime 变化时热重载

flags：

| Flag | 含义 |
|---|---|
| `--config F` | 配置文件（解析顺序见上） |
| `--root DIR` | 日志根目录（覆盖配置） |
| `--baud N` | 波特率（覆盖配置） |
| `--exclude RE` | 忽略匹配 RE 的设备；可重复；**追加到**配置的 `exclude` 列表之后 |
| `--poll-ms N` | 设备轮询间隔毫秒（覆盖配置） |
| `--sock PATH` | 控制 socket 路径。默认：配置 `control_socket`，否则 `$XDG_RUNTIME_DIR/serialtap.sock`，再否则 `/tmp/serialtap-<uid>.sock` |
| `--web ADDR` | Web 观测面板监听地址（覆盖配置 `web_addr`；`off` 关闭） |
| `--no-tray` | 不进驻托盘/菜单栏（macOS 默认进驻；Linux/Windows 恒无嵌入托盘） |

若 socket 路径已被**活着的** serialtap 实例持有，启动会被拒绝（第二个守护
进程抢不走控制通道）。崩溃残留的死 socket 文件会被自动清理接管。

`SIGINT`/`SIGTERM` 停止全部采集器（关闭串口）后干净退出。

## `attach TTY` —— 单口采集

```
serialtap attach TTY [--name N] [--config F] [--root DIR] [--baud N]
```

前台采集指定一个口 —— 手动围观或测试用（指到 socat PTY 上可以无硬件测试）。
在 `<root>/<name>/` 下写同样的双通道日志。设备名默认取 tty 基名
（`ttyACM0`），`--name` 可指定。与守护进程一样遵守 `PAUSED` 文件。
`Ctrl-C` 停止。

## `list` —— 列出设备

```
serialtap list [--config F] [--root DIR]
```

枚举当前 USB 串口设备并打印：

```
TTY             NAME            VID:PID   BY-ID                                       BY-PATH(key)
/dev/ttyUSB0    ch340           1a86:7523 usb-1a86_USB_Serial-if00-port0             pci-0000:00:14.0-usb-0:2:1.0-port0
```

BY-PATH 列即设备稳定 key；写匹配模式时可用它（或设备名）。非 USB 串口
（如主板 `ttyS*`）不会列出 —— serialtap 只管理有 `/dev/serial/by-path`
条目的口。

## `status` —— 守护状态

```
serialtap status [--sock PATH]
```

经控制 socket 向运行中的守护进程查询每设备实时状态：

```
NAME             TTY            STATE      KEY
board-a          /dev/ttyACM0   collecting usb-Espressif_USB_JTAG_...-if00
```

状态：`collecting` | `suspended`（release/PAUSED 让口中）| `flashing`
（代理刷写中）| `paused`（被 PAUSED 条目命中）。守护进程未运行时报连接错误。

## `pause [RE]` / `resume [RE]` —— 暂停清单

```
serialtap pause [RE] [--sock PATH] [--root DIR]
serialtap resume [RE] [--sock PATH] [--root DIR]
```

暂停会让匹配的采集器关掉串口并等待，直到模式移除（这是外部工具刷机前的
安全门）。清单即文件 `<root>/PAUSED`，每行一个正则（允许 `#` 注释），
守护进程按 mtime 变化热重载。

- `pause` 省略模式 → 暂停**全部**设备（写入 `.*`）
- `resume` 省略模式 → 清空整个清单（删除文件）
- `resume RE` 从清单中移除与 `RE` 相等的条目

守护进程在运行时，命令走控制 socket（立即生效，文件语义相同）；不在时
直接编辑 `PAUSED` 文件（守护进程之后启动会照常读取）。

## `proxy RE` —— 透明 USB 代理

```
serialtap proxy RE [--stop] [--sock PATH]
```

为匹配设备各开一个 **TCP 端点透传串口**：业务程序把端点当串口用（telnet /
自有 TCP 客户端 / socat 转 PTY），对设备而言与直连无异，**期间采集照常**
（透传数据同时落全量日志）。`--stop` 停止匹配设备的透传。高频遥测可用配置
`proxy_tap_exclude`（行正则）剔除出全量日志。

## `release RE` —— 临时让口

```
serialtap release RE [--for 5m] [--sock PATH]
```

请求守护进程挂起匹配的采集器，并**等到端口真正关闭**才返回 —— 此后外部
工具（idf.py monitor、minicom、自己的脚本…）可独占打开该口。需要守护进程。

- 默认：守护进程监视 `/proc/*/fd`，端口连续**空闲 3 秒**（外部工具已关掉）
  后自动回采
- `--for 5m`：改为限时回采（`90s`、`12h`… 任意 Go duration）
- `resume` 会立即撤销未到期的 release

release 不需要杀守护进程；采集器只是让口等待。

## `flash RE ...` —— 代理刷固件

```
serialtap flash RE <镜像>[@<偏移>]... [--args-file F] [--esptool CMD] [--baud N] [--chip C] [--sock PATH] [--config F]
```

一条命令走完全程 —— 守护进程对每台命中设备**逐台**执行：挂起采集器并等
端口关闭 → 运行 esptool（输出逐行流式回传）→ 恢复采集。任一时刻只有
esptool 一方持有端口，绝不与采集器抢口。

镜像指定（二选一）：

- 位置参数：`firmware.bin@0x10000 bootloader.bin@0x0` —— 省略 `@偏移` 时
  默认 `0x0`；偏移为十六进制
- `--args-file build/flasher_args.json`：ESP-IDF 生成的参数文件；刷写其中
  全部 `flash_files` 镜像（路径相对该文件目录解析，按偏移排序）。优先于
  位置参数镜像

flags：

| Flag | 含义 |
|---|---|
| `--esptool CMD` | esptool 可执行文件。解析顺序：flag → 配置 `esptool_cmd` → PATH 上的 `esptool` → `esptool.py` |
| `--baud N` | 刷写波特率。解析顺序：flag → 配置 `flash_baud` → esptool 默认 |
| `--chip C` | 芯片类型（如 `esp32s3`）；省略 → 取 `flasher_args.json` 中的值，再省略则 esptool 自动识别 |
| `--sock PATH` | 控制 socket 路径 |

输出持续流式打印直到 `✓ 刷写完成`（或失败信息）；失败时采集器同样会恢复。
多台命中逐台刷 —— 精确刷一台请锚定模式（`^board$`）。

## `reopen RE` —— 串口层软断开重连

```
serialtap reopen RE [--all] [--sock PATH]
```

立即关闭匹配设备的端口并**跳过退避**重开（自动重开路径是 5s 起步指数退避）。
端口疑似卡死（读空转、驱动状态怪异）时的快速自愈。不改变所有权与暂停语义
（与 `release` 不同）；会打断进行中的透传会话（客户端重连即可）。close/open
各带一拍复位脉冲（见 README 复位语义）。多台命中默认拒绝并列出设备名，
`--all` 显式确认后逐台执行。

## `reset RE` —— USB 层软拔插（仅 Windows）

```
serialtap reset RE [--all] [--sock PATH]
```

让口 → `pnputil /restart-device` 禁用+启用设备的**串口接口节点**（等效软件
拔插，JTAG 兄弟接口不受影响）→ 用自身枚举器确认重枚举 → 回采。适用于设备
在总线但驱动/端口僵死（打不开、僵尸句柄）。需要管理员：非提权守护进程自动
弹 UAC 提权重试（可取消）。设备整个消失在总线上时无解，只能物理重插。
多台门禁同 `reopen`。

## `tray` —— Windows 托盘常驻

```
serialtap tray [--config F] [--root DIR] [--sock PATH] [--poll-ms N]
```

独立常驻进程（与守护进程仅经 ctl socket 通信）：接入状态查看、按设备
暂停/恢复、打开日志、打开 Web 面板、启动守护进程。macOS 无此子命令
（`run` 自带菜单栏）。

## `analyze LOG...` —— 离线签名汇总

```
serialtap analyze LOG... [--lines]
```

用**内置**签名集（离线分析不应用配置 `signatures_extra`）扫描一个或多个
日志文件，按签名输出汇总：命中数、首末时间戳、一条样本行，按命中数降序。
`--lines` 额外打印每条命中行。首末时间跨文件取真正的 min/max，与参数顺序
无关。

## `decode-backtrace LOG` —— addr2line 解码

```
serialtap decode-backtrace LOG [--elf F] [--addr2line BIN] [--config F]
```

提取每行 `Backtrace:` 的地址帧（`pc:sp` 对加上 `|<-PC` 标记），经
`addr2line -pfiaC` 翻译为 `函数 / 文件:行号`。

- ELF 解析：`--elf` → 配置 `elf_map[<设备名>]`，设备名取日志的父目录
  （`logs/esp32s3-jtag/serial-….log` → `esp32s3-jtag`）。都没有则报错
- addr2line 解析：`--addr2line` → `$ESP_ADDR2LINE` → PATH 上的
  `xtensa-esp32{,s3}-elf-addr2line` / `riscv32-esp-elf-addr2line` →
  `~/.espressif/tools/` 下 glob（取最新匹配）

## `version`

打印 `serialtap <版本号>`（当前 0.1.0）。
