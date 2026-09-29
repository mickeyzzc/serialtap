# 配置参考

[English](../en/configuration.md) | [简体中文](configuration.md)

serialtap 用一个 JSON 文件配置。仓库根目录带了一份带注释的示例
[config.example.json](../../config.example.json)。

## 文件解析

- 默认路径：`~/.config/serialtap/config.json` —— **存在才加载**；否则全部
  走内置默认值。完全不带配置也能跑。
- 命令行 `--config F` 指向别处。
- `--root` / `--baud` 命令行参数在本次调用中覆盖配置值。
- 文件里留零的数值字段回填默认值（比如省略 `rotate_max_mb` 得到 64，不是 0）
  —— **但** `retention_days`（显式 `0` = 永久保留）与 `silent_reopen_s`
  （显式 `0` = 关闭）例外，零本身就是有意义的取值。

## 字段

| 字段 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `root` | string | `"logs"` | 日志根目录。其下每设备一个子目录，另有 `PAUSED` 文件。 |
| `baud` | int | `115200` | 串口波特率。对原生 USB-CDC 设备（如 ESP32-S3 USB-JTAG 口）无意义但无害。 |
| `poll_interval_ms` | int | `1000` | 守护循环的热插拔轮询间隔。 |
| `silent_reopen_s` | int | `0`（关） | 静默看门狗：连续这些秒零字节则强制重开端口。**只对保证有周期输出的设备开启**（如 30s 心跳）—— 否则合法的安静板会被复位循环。见[架构](architecture.md#复位语义open-once-and-hold)。 |
| `reopen_min_s` | int | `5` | 断线重开退避下限（秒）。 |
| `reopen_max_s` | int | `60` | 断线重开退避上限。连续失败时从 min 翻倍到 max；任何成功 open 过的会话结束后复位。 |
| `rotate_max_mb` | int | `64` | 两个日志通道的单文件大小轮转阈值。 |
| `retention_days` | int | `14` | 删除 `<root>/*/*.log` 中 mtime 早于该天数的文件，启动时 + 每小时执行。`<= 0` 永久保留。 |
| `exclude` | [string] | `[]` | 整体忽略的设备正则（匹配 tty / key / by-id / 名字任一）。坏正则跳过并告警。 |
| `names` | [{match, name}] | `[]` | by-id 正则 → 设备目录名规则。见下。 |
| `signatures_extra` | [string] | `[]` | 追加的事件签名正则。见下。 |
| `elf_map` | {name: path} | `{}` | 设备名 → 固件 `.elf`，`decode-backtrace` 用。见下。 |
| `control_socket` | string | `""` | 控制 socket 路径。空 = 平台默认：Linux/macOS `$XDG_RUNTIME_DIR/serialtap.sock`（回退 `/tmp/serialtap-<uid>.sock`），Windows `%LOCALAPPDATA%\serialtap\serialtap.sock`。`run --sock` 参数优先。 |
| `web_addr` | string | `""` | Web 观测面板监听地址。空 = `127.0.0.1:8801`；`"off"` = 关闭。`run --web` 参数优先。 |
| `esptool_cmd` | string | `""` | `flash` 用的 esptool 可执行文件。空 = 自动发现（PATH 上 `esptool` → `esptool.py`）。`--esptool` 参数优先。 |
| `flash_baud` | int | `0` | `flash` 的波特率。`0` = esptool 默认。`--baud` 参数优先。 |
| `flash_timeout_s` | int | `0` | 单台设备刷写超时（秒；超时杀 esptool 并回采）。`0` = 不限时。 |
| `proxy_tap_exclude` | string | `""` | 透传会话期间不落全量日志的行正则（如 `^#S1 ` 剔除高频遥测）。空 = 全落。 |

## 设备命名

每设备在 `root` 下有一个目录名。解析顺序：

1. **配置 `names`** —— 第一条 `match` 正则命中设备 by-id 字符串的规则生效
2. **内置规则**：

   | by-id 含 | 名字 |
   |---|---|
   | `USB_Serial-if00` | `ch340`（1a86:7523，安信可系板） |
   | `USB_Single_Serial` | `ch343`（1a86:7522/55d3，合宙系板） |
   | `Espressif_USB_JTAG` | `esp32s3-jtag`（原生 USB-JTAG，seeed/n16r8…） |

3. by-id 基名，再不行取 tty 名

名字会规范化为 `[A-Za-z0-9-_.]`（其他字符变 `-`，截断 64 字符）。两台在线
设备解析出同名时，后到者自动加**身份派生后缀** `-<token>`：token 是设备
稳定身份（key/by-id，Windows 实例路径内嵌 MAC）的 4 位十六进制散列——
同一块板无论第几个接入、跨守护重启后缀都一致；裸基名先到先得。双板并存时
请锚定后缀名（如 `^esp32s3-jtag-1x2y$`）。

**区分同型号板子：** 同型号适配器的 by-id 往往完全相同（CH340 不暴露序列号），
只能靠物理口区分（与改名无关）。Espressif 原生 USB-JTAG 的 by-id 内嵌 MAC ——
匹配它即可精确到板：

```json
"names": [
  { "match": "Espressif_USB_JTAG_serial_debug_unit_AA:BB:CC:DD:EE:FF", "name": "board-a" },
  { "match": "Espressif_USB_JTAG_serial_debug_unit_11:22:33:44:55:66", "name": "board-b" }
]
```

## 错误签名

命中签名的行会被复制（截断 200 字符）进该设备的 `events-*.log` 通道，
带 `[签名名]` 标记。内置集覆盖 ESP-IDF 常见故障行：

| 名字 | 模式（i = 大小写不敏感） |
|---|---|
| `reset-banner` | `rst:0x` |
| `boot-mode` | `boot:0x` |
| `restart-call` | `esp_restart` |
| `esp-log-error` | `E \(` |
| `guru-meditation` | `Guru Meditation` |
| `backtrace` | `Backtrace:` |
| `panic` | `panic`（i） |
| `watchdog` | `WDT\|watchdog`（i） |
| `abort` | `abort`（i） |
| `assert` | `assert`（i） |
| `lwip-accept-err` | `accept \(-?\d+\)` |
| `probe-failed` | `probe failed`（i） |
| `pausing` | `pausing`（i） |
| `reboot` | `reboot`（i） |

`signatures_extra` 追加自己的正则，命名为 `extra-0`、`extra-1`… 坏正则
跳过（打日志），不致命。每行取第一个命中的签名。注意：离线 `analyze`
只用内置集。

## `elf_map`

`decode-backtrace` 需要与日志设备匹配的固件 ELF：

```json
"elf_map": {
  "board-a": "/path/to/firmware/build/app.elf"
}
```

键是设备目录名（即日志的父目录名）。`--elf` 参数可对单次运行覆盖该映射。
