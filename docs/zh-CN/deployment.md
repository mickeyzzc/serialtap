# 部署指南

[English](../en/deployment.md) | [简体中文](deployment.md)

## 安装

三平台发布产物由 CI 在打 `v*` 标签时自动构建（附 `sha256sums.txt`），
从 [Releases](https://github.com/mickeyzzc/serialtap/releases) 页下载：

| 平台 | 产物 | 安装 |
|---|---|---|
| Windows | `serialtap-setup-<版本>.exe`（Inno 安装包）+ 便携版 zip | 安装包按用户安装免管理员；开始菜单/桌面「serialtap 托盘」直进托盘，装完勾选即启动。便携版解压即用 |
| macOS | `serialtap-<版本>.dmg`（universal：amd64+arm64） | 拖入"应用程序"；双击 = 菜单栏托盘（LSUIElement，不占 Dock） |
| Linux | `serialtap-linux-<版本>-<arch>.tar.gz`（amd64/arm64） | 含 binary + README + systemd 用户服务示例 + INSTALL.md |

Linux 命令行安装示例：

```bash
curl -LO https://github.com/mickeyzzc/serialtap/releases/download/v0.1.0/serialtap-linux-amd64
sha256sum -c sha256sums.txt --ignore-missing   # 可选的完整性校验
chmod +x serialtap-linux-amd64
./serialtap-linux-amd64 version
```

### 源码构建

要求：Go **1.27+**。其余什么都不用 —— 依赖全部 vendor；Linux/Windows
无 CGO，完全离线可构建（macOS 托盘版需 clang，见下）：

```bash
git clone https://github.com/mickeyzzc/serialtap && cd serialtap
make build        # 或: go build .
```

交叉编译同理，比如给 ARM 机器：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o serialtap .
```

macOS 托盘版本地构建需要 Xcode Command Line Tools（clang，CGO 默认开）；
`CGO_ENABLED=0` 构建得到无托盘的无头版（枚举/采集不受影响）。

## 串口权限

用户需要对 tty 设备的读写权限。把自己加进对应组并重新登录（或 `newgrp`）：

| 发行版 | 组 |
|---|---|
| Arch、Fedora | `uucp` |
| Debian、Ubuntu 系 | `dialout` |

```bash
sudo usermod -aG dialout "$USER"   # Debian 系示例
```

用 `ls -l /dev/ttyUSB0` 与 `groups` 验证。

## 首次运行

```bash
./serialtap list      # 冒烟检查：设备、名字、key 都列出来
./serialtap run       # 前台守护；Ctrl-C 停止
```

日志落在配置的 `root` 下（默认 `./logs`）。满意后配置配置文件与常驻服务。

## 配置

```bash
mkdir -p ~/.config/serialtap
cp config.example.json ~/.config/serialtap/config.json
$EDITOR ~/.config/serialtap/config.json
```

至少把 `root` 改成日志应落的位置；按需加 `names` / `elf_map` / `exclude`
—— 全部字段见[配置参考](configuration.md)。

## systemd 用户级服务

仓库带了一份 unit 文件 [deploy/serialtap.service](../../deploy/serialtap.service)：

```ini
[Service]
ExecStart=%h/.local/bin/serialtap run --config %h/.config/serialtap/config.json
Restart=on-failure
RestartSec=3
```

安装并启用：

```bash
mkdir -p ~/.local/bin ~/.config/systemd/user
cp serialtap ~/.local/bin/
cp deploy/serialtap.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now serialtap
```

验证：

```bash
systemctl --user status serialtap
journalctl --user -u serialtap -f     # 跟运行日志
serialtap status                      # 每设备实时状态
```

**不登录也开机自启**（无头机器需要）：

```bash
loginctl enable-linger "$USER"
```

系统级 unit 也可以（改路径、给服务用户加串口组），但用户级是实测过的
部署形态。

## Windows 常驻

- **安装包形态（推荐）**：setup 装好后「serialtap 托盘」快捷方式启动
  `serialtap tray`（托盘与守护经控制 socket 通信，托盘菜单可一键拉起
  守护）。开机自启用任务计划程序：登录时触发，动作指向
  `serialtap.exe tray`，工作目录任意
- **开发调试**：终端直接 `serialtap run`，或用 Web 面板
  （http://127.0.0.1:8801/）完成暂停/让口/软重连/USB 重置/上传刷机等
  全部操作。COM 口无需任何权限配置

## macOS 常驻

- **.app 形态（推荐）**：DMG 拖入"应用程序"，双击即 `serialtap run`
  （内嵌菜单栏托盘，LSUIElement 不占 Dock）
- **launchd agent**：写 `~/Library/LaunchAgents/com.mickeyzzc.serialtap.plist`
  （`ProgramArguments` 指向二进制与 `run`，`RunAtLoad=true`），
  `launchctl load` 生效——开机自启用
- 无需串口权限组

## 磁盘用量管理

- `rotate_max_mb`（默认 64）封顶单文件；轮转加 `.001`、`.002`… 后缀
- `retention_days`（默认 14）在启动时与每小时删除超出窗口的文件；
  `<= 0` 永不删除 —— 这种情况盯好自己的磁盘
- `events-*` 通道相比全量很小（只有签名命中与生命周期行），但同样受大小
  轮转兜底，复位循环的板子也撑不爆它

## 故障排查

| 症状 | 原因 / 处理 |
|---|---|
| `连不上守护进程…未运行 serialtap run？` | 守护没在跑（或 socket 路径不同 —— 传 `--sock`，或设 `control_socket`）。`systemctl --user status serialtap`。 |
| `run` 报 `控制 socket 已被另一个 serialtap 实例占用` 退出 | 已有活守护持有 socket。要么跟它通信，要么给第二个实例 `--sock` 换路径。 |
| 打开 tty `Permission denied` | 用户不在 `dialout`/`uucp` 组，或加组后没重新登录。 |
| `list` 里没有某设备 | 它没有 `/dev/serial/by-path` 条目（非 USB 串口），或被 `exclude` 命中。查 `ls /dev/serial/by-path/`。 |
| serialtap 一接板子就复位 | 预期行为 —— 见[README 的复位语义](../../README.zh-CN.md#复位语义重要)。serialtap 恰好 open 一次并持有；每插拔/暂停周期一拍。 |
| `找不到 esptool` / `找不到 addr2line` | ESP-IDF 环境不在 PATH。`source ~/esp/esp-idf/export.sh`，或用 `--esptool` / `--addr2line`（或配置 `esptool_cmd`、环境变量 `ESP_ADDR2LINE`）指到二进制。 |
| `flash` 报 `端口让出超时` | 采集器 10 秒内没能关掉端口 —— 查守护日志（`journalctl --user -u serialtap`）；楔死的 fd 通常重插即愈。 |
| 安静板子的日志一直是空的 | 合法现象。只有确定板子周期性输出才开 `silent_reopen_s` —— 否则等于自造复位循环。 |
| 两只同型号板子，日志进了同一个目录 | 同型号适配器 by-id 分不出来，后到者自动加身份派生的 4 位散列后缀（跨重启不变）。要语义命名，在 `names` 里按序列号/MAC 匹配（ESP32-S3 USB-JTAG 的 by-id 内嵌 MAC）—— 见[配置参考](configuration.md#设备命名)。 |

## 升级

替换二进制（`~/.local/bin/serialtap`），然后
`systemctl --user restart serialtap`。日志格式与布局稳定；轮转从既有后缀
编号续起，不会覆盖任何东西。配置目前只增不删字段 —— 旧文件继续可用。
