# 用户教程

从零到"插上板子就看见日志、能在浏览器里刷机"的完整路径。命令与字段的
完整参考见 [CLI 参考](cli-reference.md) / [配置](configuration.md)；
为什么这样设计见[架构](architecture.md)。

## 1. 五分钟上手

**安装**（[Releases](https://github.com/mickeyzzc/serialtap/releases) 页下载，
打标签自动构建）：

| 平台 | 产物 | 做法 |
|---|---|---|
| Windows | `serialtap-setup-<版本>.exe` | 一路下一步；开始菜单/桌面出现「serialtap 托盘」 |
| Windows | `*-portable.zip` | 解压即用，双击 `serialtap.exe` |
| macOS | `*.dmg` | 拖入应用程序，双击 = 菜单栏托盘 |
| Linux | `*.tar.gz` | 解压得二进制 + systemd 示例，见[部署](deployment.md) |

**打开**：Windows 双击图标 / 托盘快捷方式——托盘出现即自动启动守护进程
（多开有单实例守卫，再点只提示不重复）。也可以命令行 `serialtap run`。

**看日志**：浏览器打开 **http://127.0.0.1:8801/** ——插上任何 USB 串口板，
1 秒内出现设备卡并开始滚动带毫秒时间戳的日志。日志同时落盘
`~/serialtap-logs/<设备名>/serial-YYYYMMDD.log`（全量）与
`events-YYYYMMDD.log`（错误签名 + 生命周期事件）。

![仪表盘](../images/panel-dashboard.png)

> Windows 杀软对无签名 Go 二进制偶有误报：把 `serialtap.exe` 加入信任区
> 即可（误报可向厂商提交）。

## 2. 面板导览

三个视图（顶栏切换，快捷键 `1/2/3`）：

- **日志**：多设备并排窗格（含 mesh 远程设备）；`/` 聚焦搜索、`F` 冻结、
  自动滚动开关、按行数裁窗、导出。设备卡四钮：暂停 / 软重连 / 刷机 / 更多
  （让口、USB 重置、设备身份、波形都在「更多」菜单）。
- **波形**：`key=数值` 类日志行自动成通道（`T=28.1C`、`score=0.42` 都认），
  多设备多通道叠加；自定义提取正则见[波形指南](waveform-guide.md)。
- **仪表盘**：KPI + 每设备状态卡 + 运行配置速览；`c` 或「⚙ 配置」打开
  只读配置弹窗（密钥只显指纹）。

![日志视图](../images/panel-logs.png)

![波形视图](../images/panel-waveform.png)

![配置速览](../images/panel-config.png)

## 3. 日常操作

- **暂停/恢复**：设备卡 ⏸（面板勾选、托盘菜单、`serialtap pause <RE>` 三
  路同义；省略正则 = 全部）。
- **软重连**：串口层关开一轮（`serialtap reopen <RE>` / 面板 ⟳），不拆
  采集器——治"板子假死"。
- **USB 重置**：Windows 上经 pnputil 重启 USB 设备节点（需 UAC），治
  "换固件后端口消失"。
- **让口**：`serialtap release <RE> --for 5m` 把串口让给外部工具（超时
  自动回采；Linux/macOS 还支持 `--until-idle` 空闲自动回采）。

**刷机**（面板或 `serialtap flash`）：面板「刷机」→ 弹窗里选**镜像文件**
（多个 .bin 各配 offset）或 **ESP-IDF 构建产物**（flasher_args.json 自动
展开）→ 确认清单 → 开始。守护编排「让口 → esptool → 回采」全程不打架，
输出实时流回面板。命令行先 `--dry-run` 预览将执行的完整命令：

```bash
serialtap flash ch343 firmware.bin@0x10000 --dry-run
serialtap flash ch343 --args-file build/flasher_args.json
```

![刷机弹窗](../images/panel-flash.png)

## 4. 设备身份与开机体检

- **身份**：设备卡身份行（型号 · 固件 · flash · PSRAM…）自动从日志/固件
  自述/深度识别积累；点开详情表看每条事实的证据行。深度识别一键 esptool
  探测（会复位板子）。

![设备身份](../images/panel-identity.png)

- **selftest**：`serialtap selftest [RE]` 聚合各板最近一次开机的
  `SELFTEST:` 自检行（板型/flash/PSRAM/传感器/WiFi 扫描），附异常提示——
  传感器未检出、PSRAM 没起来、空口全聋（aps=0）一眼可见。固件侧规范见
  各固件仓 AGENTS.md。

## 5. 多机 mesh

几台 PC 各插几块板，任一台面板看全网：

```bash
serialtap mesh keygen                      # 生成口令（所有机器同一个）
# 每台机器 config.json:
#   "mesh_enabled": true, "mesh_key": "<口令>", "mesh_name": "bench-a"
serialtap mesh status                      # 看发现的节点与授权态
serialtap mesh approve <节点ID>            # 对端敲门后批准（或面板 ✓）
serialtap status --peer bench-a            # 查看对端设备
serialtap flash --peer bench-a <镜像>      # 远程刷机（镜像自动加密上传）
serialtap at --peer bench-a <RE>           # 远程串口控制台
```

节点可声明 `"mesh_access": "ro"` 只读接入——远端只能看，写命令/隧道/上传
一律拒。信任边界与配对语义见[架构·安全模型](architecture.md#安全模型总览)。

![mesh 视图](../images/panel-mesh.png)

## 6. 常见问题速查

| 症状 | 处置 |
|---|---|
| Windows 打开后"没反应" | 看右下角托盘；杀软误报则加信任区 |
| 反复打开出现多枚图标 | v0.2.1 起有单实例守卫；旧版升级即可 |
| `run` 报"控制 socket 已被占用" | 已有实例在跑——`status` 即可；换 socket 用 `--sock` |
| 端口打不开（EBUSY） | 谁占着：`release` 让口/`reopen` 软重连/USB 重置 |
| mesh 节点互不可见 | 同一 `mesh_key`？AP 隔离用 `mesh_peers` 静态种子 |
| 日志在哪 | `<root>/<设备名>/`，默认 `~/serialtap-logs` |

更多：[部署（自启/磁盘）](deployment.md) · [控制协议（脚本集成）](control-protocol.md) ·
[贡献指南](../../CONTRIBUTING.md)
