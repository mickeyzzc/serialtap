# 配置参考

serialtap 的全部可配置项。配置文件是 JSON,默认路径 `~/.config/serialtap/config.json`
(不存在则全默认,见 `config.example.json`)。

**优先级**:命令行 flag > 配置文件 > 内置默认值(JSON 里写零值的字段回填默认)。

## 字段一览

| 字段 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `root` | string | `"logs"` | 日志根目录。每设备一个子目录,`root/PAUSED` 暂停清单也在这里 |
| `baud` | int | `115200` | 采集波特率。USB-JTAG 口(如 ESP32-S3 原生 CDC)无实际意义但无害 |
| `poll_interval_ms` | int | `1000` | 热插拔轮询间隔(毫秒)。同时是 release 空闲检测与 PAUSED 热重载的粒度 |
| `silent_reopen_s` | int | `0`(关) | 静默看门狗:read 连续 N 秒 0 字节则强制重开端口。**默认关闭,慎开** |
| `reopen_min_s` | int | `5` | 断线重开退避下限(秒) |
| `reopen_max_s` | int | `60` | 断线重开退避上限(秒),指数 ×2 封顶 |
| `rotate_max_mb` | int | `64` | 单文件大小轮转阈值(MB),serial 与 events 通道同阈值 |
| `retention_days` | int | `14` | 日志保留天数;`<= 0` 永久保留。启动时与每小时清理 |
| `exclude` | []string | `[]` | 忽略设备的正则,匹配 tty / by-path / by-id / 设备名任一即忽略。命令行 `--exclude` 与配置**合并** |
| `names` | []NameRule | `[]` | 设备命名规则,见下 |
| `signatures_extra` | []string | `[]` | 追加事件签名正则,签名名依次为 `extra-0`、`extra-1`… |
| `elf_map` | map | `{}` | 设备名 → 固件 `.elf` 路径,`decode-backtrace` 按日志目录名自动选取 |
| `control_socket` | string | `""` | 控制 unix socket 路径;空 = 平台默认(见下)。`run --sock` 可临时覆盖 |
| `esptool_cmd` | string | `""` | 代理刷固件的 esptool 命令;空 = 自动发现(PATH → `~/.espressif/python_env` glob → Windows pip 目录,`IDF_TOOLS_PATH` 可重定位)。`flash --esptool` 可临时覆盖 |
| `flash_baud` | int | `0` | 代理刷波特率;`0` = esptool 默认。`flash --baud` 可临时覆盖 |
| `flash_timeout_s` | int | `600` | 单台设备刷写超时(秒):超时强制杀掉 esptool 并自动回采,防挂死进程永远持有串口。**显式写 `0` = 不限时**(该字段不做零值回填) |
| `proxy_tap_exclude` | string | `""` | 透传代理会话期间**不落全量日志**的行正则。空 = 全部落盘(tap 模式零丢失);感知类高频遥测建议 `"^#S1 "`(签名匹配不受影响,事件仍记录)。正则仅作用于有活跃代理客户端时 |
| `web_addr` | string | `"127.0.0.1:8801"` | Web 观测面板监听地址(见 README「Web 观测面板」)。仅本机回环;`"off"` 关闭。`run --web` / `tray --web` 可临时覆盖 |

### 关于 `silent_reopen_s` 的警告

open/close 串口都会给板子一拍复位脉冲(CDC 握手线行为,详见
[架构文档](architecture.md#open-once-and-hold最核心的纪律))。看门狗只在设备
"保证有周期性日志输出"(如 30s 心跳)时才应开启;对可能合法安静的设备开启,
会把板子打进复位循环。所以默认是 0(关)。

## 设备命名

设备名 = 日志目录名。命中链(先命中先用):

1. 配置 `names`:by-id 正则匹配 → 指定名
2. 内置 by-id 规则:`USB_Serial-if00` → `ch340`、`USB_Single_Serial` → `ch343`、
   `Espressif_USB_JTAG` → `esp32s3-jtag`(Linux by-id 形态)
3. 内置 VID:PID 规则:`1a86:7523` → `ch340`、`1a86:7522`/`1a86:55d3` → `ch343`、
   `303a:1001` → `esp32s3-jtag`(三平台通用;Windows 枚举层提供 VID:PID)
4. by-id 基名(平台形态见下)
5. tty 名(如 `ttyUSB0` / `COM3` / `usbmodem2101`)

```json
"names": [
  { "match": "Espressif_USB_JTAG_serial_debug_unit_XX:XX:XX:XX:XX:XX", "name": "board-a" },
  { "match": "USB_Single_Serial_XXXXXXXXXXXX", "name": "board-b" }
]
```

### 各平台的匹配字符串形态

`names`/`exclude` 等正则匹配的是 by-id 字符串,**形态随平台不同**——
先跑 `serialtap list` 看 BY-ID 列的实际值再写正则:

| 平台 | by-id 形态示例 | 说明 |
|---|---|---|
| Linux | `usb-Espressif_USB_JTAG_serial_debug_unit_54:32:...-if00` | udev by-id 基名,含序列号/MAC |
| Windows | `USB\VID_303A&PID_1001\48:27:E2:...` | 注册表 PNP 实例路径(VID/PID + 实例 ID) |
| macOS | `usbmodem2101` / `usbserial-1420` | `cu.*` 设备名去前缀;无 VID:PID 途径 |

- 同一守护进程内撞名自动加 `-2`、`-3` 后缀
- 名字统一过规范化:非法字符 → `-`,最长 64 字符,空回退 `dev`
- ESP32-S3 原生 USB-JTAG 的 by-id 含 MAC(序列号),seeed 与 n16r8 同芯片无法区分 ——
  要精确到板子就在 `names` 里按 MAC 细分,见 `config.example.json`

### 控制 socket 默认路径

| 平台 | 路径 |
|---|---|
| Linux / macOS | `$XDG_RUNTIME_DIR/serialtap.sock`(未设时 `/tmp/serialtap-<uid>.sock`) |
| Windows | `%LOCALAPPDATA%\serialtap\serialtap.sock`(目录自动创建;AF_UNIX 需 Win10 1803+) |

## 内置签名表

命中任一签名的行会额外记入 `events-YYYYMMDD.log`(签名名 + 截断到 200 字符的行内容)。
"(i)" = 大小写不敏感。

| 签名名 | 正则 | 覆盖 |
|---|---|---|
| `reset-banner` | `rst:0x` | bootloader 复位 banner,重启检测的根依据 |
| `boot-mode` | `boot:0x` | 启动模式行 |
| `restart-call` | `esp_restart` | 软件重启调用 |
| `esp-log-error` | `E \(` | ESP_LOG 错误级日志 |
| `guru-meditation` | `Guru Meditation` | 致命异常 |
| `backtrace` | `Backtrace:` | 崩溃回溯(可接 `decode-backtrace` 解码) |
| `panic` (i) | `panic` | panic |
| `watchdog` (i) | `WDT\|watchdog` | 看门狗 |
| `abort` (i) | `abort` | abort |
| `assert` (i) | `assert` | 断言失败 |
| `lwip-accept-err` | `accept \(-?\d+\)` | lwIP 插座耗尽(EMFILE 家族) |
| `probe-failed` (i) | `probe failed` | 探测失败 |
| `pausing` (i) | `pausing` | 应用层暂停 |
| `reboot` (i) | `reboot` | 重启 |

`signatures_extra` 追加的签名只影响**在线采集**的事件流;
`analyze` 离线扫描目前只统计内置签名表(已知限制)。

## PAUSED 暂停清单文件

`root/PAUSED`,每行一个正则(`#` 开头为注释),匹配 tty / by-path / by-id / 设备名
任一即暂停该设备的采集(采集器主动关口等待,直到模式移除):

```
# serialtap 暂停清单 — 每行一个正则(匹配 tty/by-path/by-id/设备名)
ch340
^board-a$
```

- `serialtap pause [RE]` 追加(`.*` = 全部),`serialtap resume [RE]` 移除(无参 = 清空整个文件)
- 守护进程按 mtime 热重载,粒度为一个轮询间隔(默认 1s)
- 也可以手编此文件,效果相同
- `attach` 单口模式只在启动时读一次,不热重载

## 日志布局与轮转

```
<root>/<设备名>/serial-YYYYMMDD.log       全量,每行 [YYYY-MM-DD HH:MM:SS.mmm] 前缀
<root>/<设备名>/serial-YYYYMMDD.001.log   超 rotate_max_mb 后轮转(.001/.002/…)
<root>/<设备名>/events-YYYYMMDD.log       事件流(签名命中 + 采集器生命周期)
<root>/PAUSED                              暂停清单
```

- 跨日自动开新文件;重启后续写从当日既有最大轮转编号接起(不覆盖、不撞车)
- 端口关闭时未凑齐换行的残余半行以 `…partial ` 前缀落盘(零丢失)
- 生命周期事件(如 `[collector esp32s3-jtag] serial opened (115200 baud)`)
  同时进 events 文件与守护进程 stdout(systemd 下看 `journalctl`)

## 环境变量

| 变量 | 作用 |
|---|---|
| `XDG_RUNTIME_DIR` | 控制 socket 默认路径(`$XDG_RUNTIME_DIR/serialtap.sock`,Linux/macOS) |
| `LOCALAPPDATA` | Windows 上控制 socket 的默认根(`%LOCALAPPDATA%\serialtap\`) |
| `ESP_ADDR2LINE` | `decode-backtrace` 的 addr2line 可执行文件,优先于 PATH 自动发现 |

> 配置文件默认路径三平台统一为 `~/.config/serialtap/config.json`
> (Windows 即 `%USERPROFILE%\.config\serialtap\config.json`,风格统一优先)。
