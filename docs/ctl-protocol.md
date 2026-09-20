# 控制协议(ctl)

守护进程(`serialtap run`)在本地暴露一个 unix socket 控制通道,供 CLI 子命令
(`status` / `pause` / `resume` / `release` / `flash`)与守护进程通信。
本文描述该协议的完整语义,供脚本化集成使用。

## 连接

- 路径:配置 `control_socket`;未配置时按平台取默认 ——
  Linux/macOS:`$XDG_RUNTIME_DIR/serialtap.sock`(回退 `/tmp/serialtap-<uid>.sock`);
  Windows:`%LOCALAPPDATA%\serialtap\serialtap.sock`(目录自动创建)。
  三平台都是 unix socket(Windows 10 1803+ 的 AF_UNIX,Go 原生支持);权限 0600,同用户专用
- 协议:**JSON 行协议** —— 客户端发一行 JSON 请求,服务端回一行或多行 JSON 响应,
  每行一个 JSON 对象;单行上限 1 MiB
- 一个连接可发多个请求(逐行处理);客户端断开即结束该连接

## 请求(Request)

| 字段 | 类型 | 说明 |
|---|---|---|
| `cmd` | string | `status` \| `pause` \| `resume` \| `release` \| `flash` |
| `pattern` | string | 设备匹配正则,匹配 tty / 设备名 / key(by-path)/ by-id 任一。除 `status` 外的命令都要 |
| `for_ms` | int | 仅 `release`:限时自动回采的毫秒数 |
| `until_idle` | bool | 仅 `release`:端口空闲后自动回采 |
| `spec` | object | 仅 `flash`:刷写参数,见下 |

`pattern` 是**未锚定**正则(`ch340` 会匹配所有名字含 ch340 的设备);
要精确匹配一台请锚定,如 `^board-a$`。

### flash 的 spec

| 字段 | 类型 | 说明 |
|---|---|---|
| `bins` | [{path, offset}] | 待刷镜像列表,offset 为十六进制字符串如 `"0x10000"` |
| `args_file` | string | ESP-IDF `build/flasher_args.json` 路径;与 `bins` 同时给时以它为准 |
| `esptool` | string | esptool 命令;空 = 自动发现(PATH > `~/.espressif/python_env` glob > Windows pip 目录) |
| `baud` | int | 刷写波特率;0 = esptool 默认 |
| `chip` | string | 芯片类型如 `esp32s3`;省略 = 自动(args_file 提供时会从中取 `--chip`) |
| `dry_run` | bool | 只预演:守护进程侧解析并回显将执行的 esptool 命令,不动端口、不执行、不改状态 |

**并发语义**:`flash` / `release` / `resume` 三者互斥 —— 一个 `flash` 进行中时,
并发的 `flash`/`release`/`resume` 立即返回错误(fail-fast,不排队),防止两个
esptool 抢同一口或 resume 在刷写中途抢回口。`flash` 开始时会撤销匹配设备的
pending release。单台刷写超时由配置 `flash_timeout_s` 兜底(默认 600s,超时杀
esptool 进程并回采)。

## 响应(Response)

| 字段 | 类型 | 说明 |
|---|---|---|
| `ok` | bool | 是否成功 |
| `error` | string | 失败原因 |
| `event` | string | `flash-log` \| `flash-done`(仅 flash 流式响应) |
| `line` | string | flash-log 的输出行;release 成功时为让出口数 |
| `devices` | [{name, tty, key, state}] | 仅 status:`state` ∈ `collecting` \| `paused` \| `suspended` \| `flashing` |
| `code` | int | 保留字段,当前恒未设置 |

## 各命令语义

### status

```json
→ {"cmd": "status"}
← {"ok":true,"devices":[{"name":"esp32s3-jtag","tty":"/dev/ttyACM0","key":"pci-0000:00:14.0-usb-0:2:1.0","state":"collecting"}]}
```

状态含义:`collecting` 采集中;`paused` PAUSED 文件命中;`suspended` 被让出
(release 进行中);`flashing` 代理刷写进行中。

### pause / resume

```json
→ {"cmd": "pause", "pattern": "^board-a$"}
← {"ok":true}
→ {"cmd": "resume"}            // pattern 省略 = 恢复全部(清空 PAUSED + 撤销所有 release)
← {"ok":true}
```

`pause` 写入 PAUSED 文件,采集器在下一个检查点(≤ 1s 读超时 + 轮询间隔)关口等待;
`resume` 清 PAUSED 条目并撤销全部 release,立即回采。

### release(临时让口)

```json
→ {"cmd": "release", "pattern": "luatos", "until_idle": true}
← {"ok":true,"line":"1"}       // 让出了 1 个口
```

两种自动回采模式(二选一):

- `for_ms > 0`:限时回采(与 `until_idle` 互斥,给了 for_ms 就该置 `until_idle:false`)
- `until_idle: true`:守护进程每轮巡检扫描占用(Linux `/proc/*/fd`、macOS `lsof`),
  该口**连续 3 秒**没有其他进程持有即回采(给外部工具留下反应时间;3 秒是编译期常量 `idleQuietS`)。
  **Windows 不支持此模式**——无 /proc/lsof,试开端口又会给设备复位脉冲,
  服务端显式拒绝并提示改用 `for_ms`

服务端等端口**真正关闭**(Suspend 确认,超时 5s)后才回 ok,返回后其他工具立即可用该口。
注意:release 期间那拍 close 会复位板子(与 pause 相同,通常无害,见
[架构文档](architecture.md#open-once-and-hold最核心的纪律))。

### flash(代理刷固件)

**流式响应**:多个 `flash-log` 事件行(esptool 的 stdout/stderr 逐行转发),
最后一个 `flash-done` 事件行收尾 —— 客户端应持续读行直到收到 `flash-done`。

```json
→ {"cmd":"flash","pattern":"^board-a$","spec":{"args_file":"/src/hello/build/flasher_args.json"}}
← {"ok":true,"event":"flash-log","line":"esptool v4.8.1"}
← {"ok":true,"event":"flash-log","line":"Chip is ESP32-S3"}
← ... (进度条以 \r 刷新,已按行拆分)
← {"ok":true,"event":"flash-done"}
```

失败形态:让口超时或 esptool 非零退出时,最后一行为
`{"ok":false,"event":"flash-done","error":"..."}`。匹配多个设备时**逐台刷**,
中途失败即停止(已刷完的保持完成状态,失败设备之后的不再刷)。

完整编排:让口(等端口真关,超时 10s)→ esptool(`--port <tty> [--chip] [--baud]
write_flash <offset> <bin>...`)→ 自动回采。

## 示例:命令行直连

```bash
# 状态一行流(Linux/macOS)
echo '{"cmd":"status"}' | socat - UNIX-CONNECT:"${XDG_RUNTIME_DIR}/serialtap.sock"

# 让出口 90 秒
printf '%s\n' '{"cmd":"release","pattern":"^board-a$","for_ms":90000}' \
  | socat - UNIX-CONNECT:"${XDG_RUNTIME_DIR}/serialtap.sock"
```

Windows 没有 socat,用 Python(或直接用 `serialtap status` 等子命令):

```powershell
python -c "import socket,json; s=socket.socket(socket.AF_UNIX); s.connect(r'$env:LOCALAPPDATA\serialtap\serialtap.sock'); s.sendall(b'{\"cmd\":\"status\"}\n'); print(s.recv(65536).decode())"
```

Python 示例:

```python
import json, socket

sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.connect(f"{__import__('os').environ['XDG_RUNTIME_DIR']}/serialtap.sock")
sock.sendall(b'{"cmd":"status"}\n')
print(json.loads(sock.recv(65536)))   # {"ok": true, "devices": [...]}
```

## 远程使用(SSH 隧道)

板子接在工位机/树莓派上常驻采集、从笔记本远程刷写的场景:ctl 是 unix socket,
用 SSH 把远端 socket 转发到本地即可,零额外代码:

```bash
# 笔记本上(Linux/macOS):把远端 socket 映到本地路径
ssh -nN -L /tmp/serialtap-remote.sock:/run/user/1000/serialtap.sock user@bench &

serialtap status --sock /tmp/serialtap-remote.sock
serialtap flash '^board-a$' --args-file build/flasher_args.json --sock /tmp/serialtap-remote.sock
```

注意事项:

- **镜像与 args 文件路径在守护进程一侧(远端机器)解析** —— 先把产物放到远端
  (或用远端可访问的构建目录),再发 flash 请求
- 远端 socket 路径用 `serialtap status` 在远端确认(`$XDG_RUNTIME_DIR/serialtap.sock`
  或 `/tmp/serialtap-<uid>.sock`)
- Windows 客户端的 OpenSSH 对 unix socket 本地转发支持不完整;可用 WSL 里的 ssh,
  或远端暴露 TCP 端口经 `socat TCP-LISTEN:7332,fork UNIX-CONNECT:...` 中转
  (仅限可信网络,无认证)

## socket 防抢占

`run` 启动时若 socket 路径已存在,会先 dial 探测:**活实例持有 → 拒绝启动**
(防止第二个守护进程偷走控制通道);只有残留死文件(上次异常退出、无人监听)才清理接管。
需要并行跑多个守护进程时,给每个实例配不同的 `control_socket`(或 `run --sock`)
与不同的日志 `root`。
