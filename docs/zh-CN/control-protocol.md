# 控制协议

[English](../en/control-protocol.md) | [简体中文](control-protocol.md)

守护进程（`serialtap run`）提供一个本地 unix 流式 socket，CLI 子命令
`status` / `pause` / `resume` / `release` / `flash` 都走它。你也可以直接
说这门协议 —— 做仪表盘、CI 任务或刷写脚本 —— 任何能往 socket 写一行的
语言都行。

## 传输

- **路径解析**（取第一个非空）：`run --sock` 参数 → 配置 `control_socket`
  → `$XDG_RUNTIME_DIR/serialtap.sock` → `/tmp/serialtap-<uid>.sock`。
  客户端同样接受 `--sock`，默认走同一套解析。
- **权限**：socket 文件以 mode `0600` 创建 —— 仅同用户可连。
- **帧**：双向每行一个 JSON 对象（UTF-8、`\n` 结尾）。
- **会话**：一条连接可发多个请求，按序应答。`flash` 在最终响应前会流式
  回传多行。
- **单实例**：socket 被活守护持有时，第二个 `run` 拒绝启动。无监听者的
  残留 socket 文件（崩溃遗留）会在启动时清掉重建。
- 请求行 JSON 非法时返回 `{"ok":false,"error":"bad request: …"}`，连接
  不断。

## 请求

```json
{
  "cmd": "status | pause | resume | release | flash | proxy | reopen | reset",
  "action": "stop",          // 仅 proxy：停止透传（缺省 = 开启）
  "pattern": "正则，匹配 tty / 设备名 / by-path key / by-id",
  "for_ms": 300000,
  "until_idle": true,
  "spec": { "刷写参数，仅 cmd=flash" }
}
```

| 字段 | 使用者 | 含义 |
|---|---|---|
| `cmd` | 全部 | 命令名。未知命令得到 `ok:false` 与 `unknown cmd`。 |
| `pattern` | pause/resume/release/flash/proxy/reopen/reset | 设备正则。pause/resume 省略 = 全部设备。其余命令必填且须命中至少一个在线采集器。 |
| `action` | proxy | `stop` = 停止匹配设备的透传；缺省 = 开启透传。 |
| `all` | flash/reopen/reset | 模式匹配多台时仍逐台执行（默认拒绝并列出设备名，防误伤在测设备）。 |
| `for_ms` | release | 这些毫秒后回采（替代空闲检测）。 |
| `until_idle` | release | 端口连续空闲（无其他进程持有）3 秒后回采。`for_ms` 缺省时使用。 |
| `spec` | flash | 见下。 |

### flash 的 `spec`

```json
{
  "esptool": "/path/to/esptool",
  "chip": "esp32s3",
  "baud": 921600,
  "bins":   [ { "path": "build/app.bin", "offset": "0x10000" } ],
  "args_file": "build/flasher_args.json"
}
```

`args_file` 优先于 `bins`。偏移是十六进制字符串。`esptool` 空 = 自动发现
（PATH 上 `esptool` → `esptool.py`）；`chip` 空 = 自动识别（esptool，或
`flasher_args.json` 的 `extra_esptool_args["--chip"]`）。

## 响应

```json
{
  "ok": true,
  "error": "仅失败时出现",
  "event": "flash-log | flash-done，仅 flash",
  "line": "esptool 输出行，仅 flash-log 事件",
  "devices": [ { "name", "tty", "key", "state" } ]
}
```

| 字段 | 含义 |
|---|---|
| `ok` | 本条响应的成功与否。 |
| `error` | 人类可读的失败原因（守护进程消息为中文）。 |
| `event` | `flash-log`：每行流式回传的 esptool 输出一条（内容在 `line`）；命令以恰好一条 `flash-done` 收尾。 |
| `devices` | 仅 `status`：每设备 `{name, tty, key, state}`，state 为 `collecting` / `paused` / `suspended` / `flashing`。 |
| `line` | `release` 成功时把让出的采集器数量以字符串放在这里。 |

## 命令详解

### `status`

返回 `devices`。无设备时该字段整体省略。

### `pause` / `resume`

与 CLI 编辑同一份 `PAUSED` 文件语义：`pause` 不带模式 = 全部暂停（`.*`）；
`resume` 不带模式 = 清空文件 —— 同时撤销未到期的 `release`。带模式时，
pause 追加该条；resume 移除与其相等的条目。对运行中的守护立即生效。

### `release`

挂起匹配的采集器并**等端口真正关闭**后才应答，`ok` 且数量在 `line`。此后
外部工具即可打开该口。回采发生在 `for_ms` 到期，或（`until_idle` 时）该
tty 连续 3 秒无其他进程持有之后。模式命中不到任何采集器则失败。

### `flash`

对每台命中设备依次：挂起采集器 → 跑 esptool → 恢复。流式回传 `flash-log`
响应（esptool 每输出一行一条，`\r` 分行让进度条透过来），最后一条
`flash-done` 的 `ok` 反映整体结果。esptool 失败时采集器同样恢复。多台命中
逐台刷 —— 要精确刷一台发锚定模式（`^board$`）。

### `proxy`

开启时对每台命中设备挂起独占、建立 TCP 端点（`endpoint` 返回地址，
`device`/`device_key` 标识端点所属设备 —— 多板同名时以它为准），透传期间
采集照常。`action:"stop"` 停止，`line` 返回停止数量。

### `reopen`

对每台命中设备：立即关闭端口 → 采集循环读错误退出 → **跳过退避**立即重开。
`line` 返回触发台数。会打断进行中的透传会话。

### `reset`（仅 Windows）

对每台命中设备：让口 → pnputil 重启串口接口节点 → 枚举确认重枚举 → 回采。
需要管理员权限（非提权守护自动弹 UAC）。

## 示例

用 `socat` 查状态（任何支持 unix socket 的 netcat 也行）：

```bash
echo '{"cmd":"status"}' | socat - UNIX-CONNECT:"$XDG_RUNTIME_DIR/serialtap.sock"
# {"ok":true,"devices":[{"name":"board-a","tty":"/dev/ttyACM0","key":"usb-Espressif_...-if00","state":"collecting"}]}
```

让口 10 分钟：

```bash
echo '{"cmd":"release","pattern":"^board-a$","for_ms":600000}' | socat - UNIX-CONNECT:"$XDG_RUNTIME_DIR/serialtap.sock"
# {"ok":true,"line":"1"}
```

刷写并跟进流（最后一行带 `"event":"flash-done"`）：

```bash
echo '{"cmd":"flash","pattern":"^board-a$","spec":{"bins":[{"path":"build/app.bin","offset":"0x10000"}],"chip":"esp32s3"}}' \
  | socat - UNIX-CONNECT:"$XDG_RUNTIME_DIR/serialtap.sock"
# {"ok":true,"event":"flash-log","line":"esptool.py v4.8"}
# {"ok":true,"event":"flash-log","line":"Chip is ESP32-S3"}
# ...
# {"ok":true,"event":"flash-done"}
```
