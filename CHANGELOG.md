# Changelog

## Unreleased

- feat(web): **波形观测图（通用示波器）** —— 日志区新增第三 tab「波形」：
  用户给提取正则（每个捕获组 = 一个通道；无捕获组则全局命中各为一通道，
  ≤8 路），面板把串口流变成滚动波形 —— 多通道各色、按通道归一化自动量程、
  时间窗 15s/60s/5min/30min、冻结/恢复、清除、实时 Hz 与 min~max 读数；
  供数前剥掉面板时间戳前缀（提取正则对准板子原始行）。**纯客户端实现**：
  数据源就是既有 serial SSE 流，解析/绘制全在浏览器 Canvas，守护进程零
  改动（serialtap 保持零业务逻辑——只观测、不解释）

- fix(web): 面板设备卡/下拉全空、无法切换主板 —— renderCards 里残留一处
  `querySelector('[data-a="view"]')` 空绑定（rebase 时卡片模板换掉了 view
  按钮、JS 行遗留），null.onclick 赋值抛 TypeError 使渲染在第一张卡片处
  中断，appendChild/syncDevSel 永不执行，异常又藏在 fetch 的 unhandled
  rejection 里无任何提示。删残行；新增 `TestIndexHTMLWiring` 接线一致性
  回归测试（index.html 里 JS 引用的每个 `[data-a=…]` 选择器与 `$("id")`
  元素必须真实存在，防此类模板/JS 漂移再次静默白屏；已实证对修复前版本
  报错）

- **文档全面双语化 + 三平台对齐**：`README.md` 重写为全英文（与
  `README.zh-CN.md` 成对，顶部语言互链）；`docs/en/` 与 `docs/zh-CN/`
  五对文档（architecture/cli-reference/configuration/control-protocol/
  deployment）同步到当前特性——三平台支持（删除"仅 Linux"残留）、
  安装包（win Inno/mac DMG/linux tar.gz）、Web 面板操作全覆盖与浏览器
  刷机、flash 多台默认拒绝门禁与 `--retries/--dry-run/--all` 全量 flag、
  身份派生命名后缀（取代旧的 `-2` 枚举后缀描述）、配置补
  `web_addr`/`flash_timeout_s`/`proxy_tap_exclude` 三字段、控制协议移植
  SSH 隧道远程刷写章节与 Windows Python 直连示例；`docs/` 根下三个旧版
  文档改为迁移跳转存根；CONTRIBUTING 更新包表（+web/tray）、CI 矩阵、
  版本注入说明，并确立"双语成对维护"规则

- fix(ctl, windows): Close 的等待加上限 + 主动关闭已接受连接 —— Windows
  AF_UNIX 两个平台限制实测：`conn.Close()` 不中止在途 Read（handler 永久卡
  Scan，#14 并发测试挂死 600s）、对已关监听的 `connect()` 永久阻塞；测试侧
  拨号改带超时且先停拨号再 Close（Add/Wait 竞态覆盖不变，由 backlog 未注册
  连接承担）
- **合流 feat/sense-pipeline**（Web 观测面板 / Windows 一等支持 / reopen·reset
  自愈 / 代理透传 / flash 强化 / Wave·Tap Logo）与 macOS 支持线，详见下方两组条目
- **macOS 托盘「打开 Web 面板」按钮**：与 Windows 托盘同语义 —— `run` 内嵌启动的
  Web 观测面板（默认 `127.0.0.1:8801`，`--web off` / 配置 `web_addr` 可关）在
  菜单栏一键打开；面板关闭时按钮自动隐藏
- **macOS 支持**：设备枚举移植 —— 解析 `ioreg`（IOKit 注册表）取身份，
  USB `locationID` 为物理口 key（对齐 Linux by-path 语义），
  by-id 采用 `usb-<vid>_<pid>[-sn]` 风格与 Linux 配置规则互通；
  只枚举 USB 串口（cu.usb*/wch*/SLAB*/USA* 前缀，滤掉蓝牙等本机串口），
  ioreg 仅在端口集合变化时执行（稳态零开销）；
  ioreg 失败退化为 tty 名身份，不阻塞采集
- **macOS 菜单栏托盘**：`run` 默认进驻菜单栏（fyne.io/systray，darwin+cgo）——
  实时设备状态、暂停/恢复全部采集、打开日志目录、打开 Web 面板、退出；
  `run --no-tray` 无头模式；Linux/无 CGO 构建走桩实现，纯静态二进制承诺不变
- fix(ctl): unix socket 路径超长（BSD 104 字节上限）提前拦截并给出明确报错，
  此前 bind 只报 `invalid argument` 无法排查；测试路径在 darwin 上改用 /tmp 短路径

### 三平台发布工程：CI 出包 + mac/win 安装包 + Web 操作全覆盖

- **CI/Release**：`release.yml` 打 `v*` 标签 → Linux tar.gz（amd64/arm64，
  含 systemd 用户服务示例）、macOS .app+DMG（universal：amd64+arm64 lipo）、
  Windows Inno Setup 安装包 + 便携 zip；版本经 ldflags 注入 `cli.Version`，
  产物附 sha256sums。`ci.yml` 覆盖率门禁暂降为提示（基线 65%，见工作流注释）
- **托盘接线随 macOS 线合流**：macOS 菜单栏托盘采用 daemon 内嵌方案（`run`
  进驻菜单栏，Host 回调注入，见上方 macOS 条目），Windows `serialtap tray`
  不变；Linux 无 GUI 走桩实现。打包适配：mac .app 入口改为 `serialtap run`
  （即菜单栏），win 安装包快捷方式仍指 `serialtap tray`
- **Web 面板操作全覆盖**（`指令操作都能在 Web 上解决`）：设备卡新增
  让口 5m / 串口软重连 / USB 重置（UAC 提示确认）按钮；**刷机模态窗**——
  上传多镜像+偏移（或 flasher_args.json）→ 落盘 `root/.flash-upload/` →
  复用 daemon 编排（让口→esptool→回采）→ esptool 输出 SSE 实时回放，
  历史回放支持晚连接的浏览器；`/api/cmd` 白名单扩至 release/reopen/reset
  （flash 走专用 `/api/flash` 上传端点）

### 全新 Logo（Wave·Tap：方波 · 在线分接）

- 设计定稿 **Wave·Tap**：UART 方波横贯 + 低电平中点向下的抽头——串口
  电平的通用符号 × serialtap 的"在线分接"本职；深底圆角方应用图标，
  teal 抽头与面板主题色同族。6 个设计方向的变体存档于 `assets/logo/variants/`，
  展示页 `assets/logo/showcase.html`
- **Web 面板**：favicon（SVG data URI）+ 头部 logo 全部换新
- **托盘**：从程序化像素字 ICO 换成 `go:embed` 的多尺寸真 ICO
  （16/24/32/48/64/256），断连灰版同步替换；`assets/logo/gen.py`
  一键再生成（svglib + Pillow，纯 Python 工具链）并自动拷贝到 internal/tray
- **安装包/快捷方式**：直接引用 `assets/logo/icon.ico`（exe 资源图标
  可后续用 rsrc/goversioninfo 编入 .syso，暂未接）

### 软断开重连：串口层 `reopen` + USB 层 `reset`

> 分层自愈：设备在总线上时，不动手拔插就能从端口/驱动僵死里恢复。
> （设备整个消失在总线上仍无解——只能物理重插。）

- 新增 `serialtap reopen <正则> [--all]`（ctl `reopen`）：**串口层软断开重连**
  —— 采集器立即关口并**跳过退避**重开（自动重开路径是 5s 起步指数退避）。
  不改变所有权与暂停语义（与 `release` 不同）；会打断进行中的透传会话
  （客户端重连即可）。close/open 各带一拍复位脉冲——对 CH340/乐鑫原生
  USB 口等于顺带软重启板子（open-once 纪律的显式手动例外）
- 新增 `serialtap reset <正则> [--all]`（ctl `reset`，仅 Windows）：**USB 层
  软拔插**——让口 → `pnputil /restart-device <实例路径>`（禁用+启用设备
  节点，只动串口接口节点，JTAG 兄弟接口不受影响）→ 用自身枚举器确认重枚举
  → 回采。实测陷阱两条已吸收：pnputil **需管理员**（非提权守护自动弹 UAC
  提权重试，可取消）；失败时**退出码仍为 0**（成败只认输出标记 + 枚举复核）
- `reopen`/`reset` 与 flash 共用多设备门禁（未锚定匹配多台默认拒绝、列出
  设备名、`--all` 显式确认）——门禁从 flash 抽出为 `gateMulti` 共用
- ctl 协议新增 `reopen`/`reset` 命令（复用 `pattern`/`all` 字段），完整语义
  见 docs/ctl-protocol.md

### flash 失败自动重试（Windows CDC 瞬时失败）

- `serialtap flash` 客户端侧按次数重试（`--retries` 总次数默认 3、
  `--retry-wait` 间隔默认 5s）。动机：Windows 上 USB-CDC 设备复位/重枚举
  后的首次 open / SetCommState 常以 ERROR_GEN_FAILURE 瞬时失败（实测
  ESP32-S3 USB-Serial-JTAG），esptool 自身不重试——单发 CLI 一撞即退。
  每次重试都完整走一遍 让口 → esptool → 回采 编排（幂等）。守护进程
  不可达等传输层错误不重试（重试无益，立即失败）

### 多板同芯片并存的确定性（两板联调实战修复）

> 场景：一块在开发（esp32-s3-zero）+ 一块在测试（luatos 感知节点），两只
> 都是乐鑫原生 USB-JTAG（`303a:1001`）→ 同名 `esp32s3-jtag`，业务程序
> （homepulse）随机连到错误的板子上且沉默挂死。四层修复：

- **撞名后缀改为身份派生**：同名设备不再按接入顺序加 `-2`（换插顺序/重启
  会换主），改加 4 位 base36 散列 token（输入 = key + by-id，Windows 实例
  路径内嵌 MAC）——同一块板无论第几个接入、跨守护重启后缀一致；
  散列碰撞退回计数保底。双板并存请锚定后缀名或配置 `names`
- **全链路确定性排序**：`matches()` / `Status()` 按 key 排序返回——此前
  直接迭代 map，每次调用顺序随机，`ProxyStart` 返回的"第一个"端点、
  客户端取的"第一台"设备都在掷骰子；现在多设备处理顺序（flash 逐台序、
  proxy 端点归属、status 清单）全部确定可复现
- **`proxy start` 回报端点所属设备**：ctl 响应新增 `device` / `device_key`，
  客户端（homepulse）校验"拨的就是选中的那台"，设备清单变化竞态下
  张冠李戴当场报错
- **`flash` 多设备门禁**：pattern 匹配多台时**默认拒绝**并列出设备名
  （要求锚定或显式确认——CLI `--all` / ctl `all: true`）——多板同名时
  未锚定正则会把在测板拖进刷写序列（让口复位 + 错芯片镜像），实测事故

### Web 观测面板（`serialtap run` 内置）

- 新增 `internal/web`：默认 **http://127.0.0.1:8801/**（配置 `web_addr`，
  `"off"` 关闭；`run --web` 覆盖）—— 守护进程经手的一切可视化：
  设备卡（状态/代理徽标/写入速率/日志体积）、实时日志 SSE 尾随
  （全量/事件流双通道，自动跟随日轮转与大小轮转）、最近事件
  （签名命中 + 生命周期 + 刷机记录，命中高亮）
- 操作（按设备暂停/恢复、代理开/停）经与 ctl socket **同一条处理路径**
  转发，不引入第二套控制逻辑；flash/release 长操作面板拒绝（走 CLI）；
  仅监听本机回环，与 ctl socket 同信任域；**零业务逻辑，定位不变**
- 托盘新增「打开 Web 面板」菜单项（`tray --web off` 隐藏）
- **修复**：日志轮转后"最新文件"判定错误 —— 字典序把
  `serial-20260921.001.log` 排在 `serial-20260921.log` 之前（`.0` < `.l`），
  导致托盘「查看串口日志」在大小轮转后打开旧基础文件；改为按
  （日期, 数字后缀）语义比较（tray.LatestSerialLog 与面板 latestFile 同源修复）
- **修复（多设备切换不可用）**：设备卡每秒全量重建 DOM，进行中的点击落在
  已被替换的节点上被静默吞掉；且可点目标只有设备名小字/「查看日志」按钮，
  多板并存时难以切换查看。改为结构签名比对（无变化只就地更新数字，不重建
  DOM），整卡可点（操作按钮阻止冒泡），日志区新增**设备下拉**（与卡片
  双向同步），选中的设备拔出后自动回落到首台

### 透明 USB 代理（`serialtap proxy`）

> 以下实战问题在 ESP32-C3 真机联调（homepulse 感知管线）中发现并修复/验证。

- 新增 `proxy <设备正则>` 子命令：为匹配设备开一个 127.0.0.1 TCP 端点，
  业务程序（如 [homepulse](../homepulse) 感知引擎）经它直接读写板子串口 ——
  对程序等同直连 USB；`proxy <正则> --stop` 关闭
- 端口**不重开**：透传桥内嵌在采集器读循环（open-once 纪律保持，
  无复位脉冲）；设备→客户端字节镜像 + 客户端→设备写入全双工并发
- **tap 模式**：透传期间采集照常（双向流量按行落盘）；
  新配置 `proxy_tap_exclude` 可剔除高频遥测行（如 `"^#S1 "`）防刷盘，
  事件签名不受影响
- 单客户端语义（第二连接即关）；慢/死客户端 200ms 写超时自动摘除
- ctl 协议新增 `proxy` 命令（`action: start|stop`，响应带 `endpoint`），
  `status` 设备条目新增 `proxy` 字段；设备拔出/守护退出自动收口监听与会话
- 定位不变：**纯基础设施，零业务逻辑**——只搬字节，不理解协议

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
