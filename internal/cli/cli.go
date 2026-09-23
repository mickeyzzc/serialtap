// Package cli 实现命令行界面：子命令分发、flag 解析与各命令编排。
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mickeyzzc/serialtap/internal/analyze"
	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/daemon"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/flash"
	"github.com/mickeyzzc/serialtap/internal/logstore"
	"github.com/mickeyzzc/serialtap/internal/pause"
	"github.com/mickeyzzc/serialtap/internal/signature"
	"github.com/mickeyzzc/serialtap/internal/tray"
	"github.com/mickeyzzc/serialtap/internal/web"
)

const Version = "0.1.0"

func usage() {
	fmt.Fprintf(os.Stderr, `serialtap v%s — USB 串口持续采集器

用法:
  serialtap run                          守护模式：轮询发现 USB 串口，自动起采集器
      [--config F] [--root DIR] [--baud N] [--exclude RE]... [--poll-ms N]
      [--sock F] [--no-tray]              （macOS 默认进驻菜单栏托盘；--no-tray 关闭）
  serialtap attach TTY [--name N]        单口采集（手动/测试，可接 socat PTY）
      [--config F] [--root DIR] [--baud N]
  serialtap list [--config F]            列出当前设备与身份
  serialtap analyze LOG... [--lines]     离线签名扫描汇总
  serialtap decode-backtrace LOG         Backtrace addr2line 解码
      [--elf F] [--addr2line BIN] [--config F]
  serialtap pause [RE]...                暂停采集（省略=全部）
  serialtap resume [RE]...               恢复采集（省略=全部）
  serialtap tray                         Windows 托盘常驻：状态/按设备暂停恢复/开日志
      [--config F] [--root DIR] [--sock PATH] [--poll-ms N]
  serialtap proxy RE [--stop]            透明 USB 代理：为匹配设备开 TCP 端点透传串口
                                         （对业务程序等同直连；期间采集照常，可配 proxy_tap_exclude）
  serialtap release RE [--for 5m]        临时让出串口给外部工具（默认空闲 3s 自动回采）
  serialtap flash RE <bin>[@0x10000]...  代理刷固件：让口 → esptool → 自动回采
      [--args-file F] [--esptool CMD] [--baud N] [--chip C] [--dry-run] [--all]
                                         （RE 匹配多台时默认拒绝，防误刷在测设备；批量刷给 --all）
  serialtap reopen RE [--all]            串口层软断开重连：立即关口→跳过退避重开
                                         （端口疑似卡死时自愈；会打断透传会话，客户端重连即可）
  serialtap reset RE [--all]             USB 层软拔插：让口 → pnputil 重启设备节点 → 回采
                                         （设备在总线但驱动/端口僵死时；需管理员——非提权守护自动弹 UAC）
  serialtap status                       查看守护进程与设备实时状态
  serialtap version

日志布局: <root>/<设备名>/serial-YYYYMMDD.log（全量）+ events-YYYYMMDD.log（事件）
暂停清单: <root>/PAUSED（每行一个正则，匹配 tty/by-path/by-id/设备名）
`, Version)
}

// loadCfgMerged: --config 显式指定 > 默认路径（存在才读）> 纯默认。
func loadCfgMerged(configPath, rootFlag string, baud int) (config.Config, error) {
	path := configPath
	if path == "" {
		if def := config.DefaultConfigPath(); fileExists(def) {
			path = def
		}
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return cfg, err
	}
	if rootFlag != "" {
		cfg.Root = rootFlag
	}
	if baud != 0 {
		cfg.Baud = baud
	}
	return cfg, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func stdoutLog(format string, args ...any) {
	fmt.Printf("["+logstore.Stamp(time.Now())+"] "+format+"\n", args...)
}

func discardLog(string, ...any) {}

// Run: 命令分发入口（返回退出码）。
func Run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = cmdRun(args[1:])
	case "attach":
		err = cmdAttach(args[1:])
	case "list":
		err = cmdList(args[1:])
	case "analyze":
		err = cmdAnalyzeCLI(args[1:])
	case "decode-backtrace":
		err = cmdDecodeCLI(args[1:])
	case "status":
		err = cmdStatus(args[1:])
	case "proxy":
		err = cmdProxy(args[1:])
	case "release":
		err = cmdRelease(args[1:])
	case "flash":
		err = cmdFlash(args[1:])
	case "reopen":
		err = cmdReopen(args[1:])
	case "reset":
		err = cmdReset(args[1:])
	case "pause":
		err = cmdPauseSocket(args[1:], true)
	case "resume":
		err = cmdPauseSocket(args[1:], false)
	case "tray":
		err = cmdTray(args[1:])
	case "version":
		fmt.Println("serialtap " + Version)
	default:
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
	return 0
}

// parseFlags: Go flag 遇到第一个位置参数就停止解析，而自然写法是
// "attach /dev/ttyACM0 --name x"。这里循环剥离位置参数再继续解析，
// 让 flag 可出现在任意位置。返回全部位置参数。
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件 JSON")
	root := fs.String("root", "", "日志根目录")
	baud := fs.Int("baud", 0, "波特率")
	pollMs := fs.Int("poll-ms", 0, "轮询间隔 ms")
	var exclude multiFlag
	fs.Var(&exclude, "exclude", "忽略设备正则（可多次）")
	sockFlag := fs.String("sock", "", "控制 socket 路径（默认自动；被占用时启动会被拒绝）")
	noTray := fs.Bool("no-tray", false, "不进驻托盘/菜单栏（macOS 默认进驻）")
	webFlag := fs.String("web", "", "Web 观测面板地址（默认 127.0.0.1:8801；off = 关闭）")
	parseFlags(fs, args)

	cfg, err := loadCfgMerged(*cfgPath, *root, *baud)
	if err != nil {
		return err
	}
	if *webFlag != "" {
		cfg.WebAddr = *webFlag
	}
	if *pollMs > 0 {
		cfg.PollMs = *pollMs
	}
	exclPats := append([]string{}, cfg.Exclude...)
	exclPats = append(exclPats, exclude...)
	excl, bad := device.CompilePatterns(exclPats)
	for _, b := range bad {
		stdoutLog("[watch] 忽略坏正则: %s", b)
	}

	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return err
	}
	d, err := daemon.New(cfg, excl, nil, stdoutLog)
	if err != nil {
		return err
	}

	if n, err := logstore.SweepRetention(cfg.Root, cfg.RetentionDays); err == nil && n > 0 {
		stdoutLog("[watch] 保留期清理: 删除 %d 个旧日志", n)
	}
	stdoutLog("[watch] serialtap run v%s root=%s poll=%dms", Version, cfg.Root, cfg.PollMs)

	// 控制 socket（flash/release/status/pause/resume 的服务端）
	sockPath := cfg.ControlSocket
	if *sockFlag != "" {
		sockPath = *sockFlag
	}
	if sockPath == "" {
		sockPath = ctl.DefaultSocketPath()
	}
	ctlSrv, err := ctl.Listen(sockPath)
	if err != nil {
		return err
	}
	defer ctlSrv.Close()
	handler := func(req ctl.Request, respond func(ctl.Response)) {
		switch req.Cmd {
		case "status":
			respond(ctl.Response{OK: true, Devices: d.Status()})
		case "pause":
			pats := []string{}
			if req.Pattern != "" {
				pats = []string{req.Pattern}
			}
			if err := pause.PauseCLI(cfg.Root, true, pats); err != nil {
				respond(ctl.Response{OK: false, Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true})
		case "resume":
			n, err := d.ResumeAll(req.Pattern)
			if err != nil && n == 0 {
				respond(ctl.Response{OK: false, Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true})
		case "proxy":
			if req.Action == "stop" {
				n, err := d.ProxyStop(req.Pattern)
				if err != nil {
					respond(ctl.Response{OK: false, Error: err.Error()})
					return
				}
				respond(ctl.Response{OK: true, Line: fmt.Sprintf("%d", n)})
				return
			}
			ep, devName, devKey, err := d.ProxyStart(req.Pattern)
			if err != nil {
				respond(ctl.Response{OK: false, Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true, Endpoint: ep, Device: devName, DeviceKey: devKey})
		case "release":
			forDur := time.Duration(req.ForMs) * time.Millisecond
			n, err := d.Release(req.Pattern, forDur, req.UntilIdle)
			if err != nil {
				respond(ctl.Response{OK: false, Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true, Line: fmt.Sprintf("%d", n)})
		case "flash":
			err := d.Flash(req.Pattern, req.All, req.Spec, func(line string) {
				respond(ctl.Response{OK: true, Event: "flash-log", Line: line})
			})
			if err != nil {
				respond(ctl.Response{OK: false, Event: "flash-done", Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true, Event: "flash-done"})
		case "reopen":
			n, err := d.Reopen(req.Pattern, req.All)
			if err != nil {
				respond(ctl.Response{OK: false, Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true, Line: fmt.Sprintf("%d", n)})
		case "reset":
			if err := d.Reset(req.Pattern, req.All); err != nil {
				respond(ctl.Response{OK: false, Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true})
		default:
			respond(ctl.Response{OK: false, Error: "unknown cmd: " + req.Cmd})
		}
	}
	go ctlSrv.Serve(handler)
	stdoutLog("[ctl] 控制通道: %s", sockPath)

	// Web 观测面板：状态/实时日志/事件只读展示 + 暂停/恢复/代理操作。
	// 操作经 commander 桥到上面同一条 ctl 处理路径 —— 面板不引入第二套控制逻辑。
	webCmd := func(req ctl.Request) (ctl.Response, error) {
		switch req.Cmd {
		case "status", "pause", "resume", "proxy":
			var resp ctl.Response
			handler(req, func(r ctl.Response) { resp = r })
			return resp, nil
		}
		return ctl.Response{}, fmt.Errorf("面板不支持该命令（走 CLI）: %s", req.Cmd)
	}
	webSrv := web.Start(cfg.WebAddr, cfg.Root, d.Status, webCmd, stdoutLog)
	defer webSrv.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	stop := make(chan struct{})
	var stopOnce sync.Once

	// watch: 守护主循环。托盘可用时在 goroutine 里跑（macOS UI 占主线程），
	// 否则就是主循环本身。任何一侧退出（菜单"退出" / 终端信号）都经 stop/sigCh 汇合。
	watch := func() {
		tick := time.NewTicker(time.Duration(cfg.PollMs) * time.Millisecond)
		defer tick.Stop()
		sweepTick := time.NewTicker(time.Hour)
		defer sweepTick.Stop()
		for {
			d.Tick()
			select {
			case <-sigCh:
				stdoutLog("[watch] 退出信号 — 停止 %d 个采集器", d.Collectors())
				return
			case <-stop:
				return
			case <-tick.C:
			case <-sweepTick.C:
				if n, err := logstore.SweepRetention(cfg.Root, cfg.RetentionDays); err == nil && n > 0 {
					stdoutLog("[watch] 保留期清理: 删除 %d 个旧日志", n)
				}
			}
		}
	}

	if tray.Supported() && !*noTray {
		stdoutLog("[tray] 菜单栏模式 — 托盘图标可暂停/恢复/打开日志/退出")
		tray.Run(trayHost(d, cfg, func() { stopOnce.Do(func() { close(stop) }) }), watch)
	} else {
		watch()
	}
	d.Shutdown()
	return nil
}

// stateZH: ctl 状态 → 托盘展示文案。
func stateZH(s string) string {
	switch s {
	case "collecting":
		return "● 采集中"
	case "paused":
		return "⏸ 已暂停"
	case "suspended":
		return "↩ 让口中"
	case "flashing":
		return "⚡ 刷写中"
	}
	return s
}

func trayHost(d *daemon.Daemon, cfg config.Config, quit func()) tray.Host {
	panelURL := "" // 与 Windows 托盘同语义：PickAddr 归一化，"off"/空地址 → 不显示按钮
	if addr := web.PickAddr(cfg.WebAddr); addr != "" {
		panelURL = "http://" + addr + "/"
	}
	return tray.Host{
		Version:  Version,
		PanelURL: panelURL,
		Status: func() []string {
			devs := d.Status()
			if len(devs) == 0 {
				return nil
			}
			lines := make([]string, 0, len(devs))
			for _, s := range devs {
				lines = append(lines, fmt.Sprintf("%s · %s", s.Name, stateZH(s.State)))
			}
			return lines
		},
		PauseAll: func() error {
			if err := pause.PauseCLI(cfg.Root, true, nil); err != nil {
				return err
			}
			d.Tick() // 立即生效（PAUSED 热重载也走这）
			return nil
		},
		ResumeAll: func() error {
			if _, err := d.ResumeAll(""); err != nil {
				return err
			}
			return nil
		},
		OpenLogs: func() error {
			return exec.Command("open", cfg.Root).Start()
		},
		Quit: quit,
		Logf: stdoutLog,
	}
}

func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件 JSON")
	root := fs.String("root", "", "日志根目录")
	baud := fs.Int("baud", 0, "波特率")
	name := fs.String("name", "", "设备名（默认取 tty 基名）")
	pos := parseFlags(fs, args)
	if len(pos) < 1 {
		return fmt.Errorf("attach 需要 TTY 参数，如 attach /dev/ttyACM0")
	}
	tty := pos[0]
	cfg, err := loadCfgMerged(*cfgPath, *root, *baud)
	if err != nil {
		return err
	}
	devName := *name
	if devName == "" {
		devName = filepath.Base(tty)
	}
	w, err := logstore.NewDeviceWriter(cfg.Root, devName, cfg.RotateMB)
	if err != nil {
		return err
	}
	pause, _, err := pause.LoadPauseFile(cfg.Root)
	if err != nil {
		return err
	}
	dev := device.DeviceInfo{Tty: tty, Key: "attach:" + tty, Name: device.SanitizeName(devName)}
	c := collector.NewCollector(dev, cfg, w, signature.New(cfg.ExtraSigs), pause, stdoutLog)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		c.Stop()
	}()
	c.Run()
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件 JSON")
	root := fs.String("root", "", "")
	parseFlags(fs, args)
	cfg, err := loadCfgMerged(*cfgPath, *root, 0)
	if err != nil {
		return err
	}
	excl, _ := device.CompilePatterns(cfg.Exclude)
	infos, err := device.Enumerate(excl, cfg.Names)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Println("（无 USB 串口设备）")
		return nil
	}
	fmt.Printf("%-14s %-16s %-9s %-44s %s\n", "TTY", "NAME", "VID:PID", "BY-ID", "BY-PATH(key)")
	for _, d := range infos {
		fmt.Printf("%-14s %-16s %-9s %-44s %s\n",
			d.Tty, d.Name, d.VID+":"+d.PID, d.ByID, d.Key)
	}
	return nil
}

func cmdAnalyzeCLI(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	lines := fs.Bool("lines", false, "同时打印命中的行")
	pos := parseFlags(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("analyze 至少一个日志路径")
	}
	return analyze.Logs(os.Stdout, pos, *lines)
}

func cmdDecodeCLI(args []string) error {
	fs := flag.NewFlagSet("decode-backtrace", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件 JSON")
	elf := fs.String("elf", "", "固件 .elf（省略则按设备名查配置 elf_map）")
	a2l := fs.String("addr2line", "", "addr2line 可执行文件（默认自动查找）")
	pos := parseFlags(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("decode-backtrace 需要日志路径")
	}
	cfg, err := loadCfgMerged(*cfgPath, "", 0)
	if err != nil {
		return err
	}
	return analyze.DecodeBacktrace(pos[0], *elf, *a2l, cfg)
}

func cmdPauseCLI(args []string, pauseMode bool) error {
	fs := flag.NewFlagSet("pause/resume", flag.ExitOnError)
	root := fs.String("root", "", "日志根目录")
	fs.String("sock", "", "（socket 路径；文件直改模式忽略）")
	pos := parseFlags(fs, args)
	r := *root
	if r == "" {
		cfg, err := config.LoadConfig(config.DefaultConfigPath())
		if err == nil {
			r = cfg.Root
		} else {
			r = config.DefaultConfig().Root
		}
	}
	return pause.PauseCLI(r, pauseMode, pos)
}

type multiFlag []string

func (m *multiFlag) String() string { return "" }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// —— 控制通道子命令：status / release / flash；pause/resume 走 socket 优先 ——

func ctlSend(sockOverride string, req ctl.Request, onEvent func(ctl.Response) bool) error {
	path := sockOverride
	if path == "" {
		path = ctl.DefaultSocketPath()
	}
	return ctl.Send(path, req, onEvent)
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	sock := fs.String("sock", "", "控制 socket 路径（默认自动）")
	parseFlags(fs, args)
	var respErr string
	err := ctlSend(*sock, ctl.Request{Cmd: "status"}, func(r ctl.Response) bool {
		if !r.OK {
			respErr = r.Error // 统一由 Run 的错误出口打印
			return true
		}
		if len(r.Devices) == 0 {
			fmt.Println("（无采集设备）")
			return true
		}
		fmt.Printf("%-16s %-14s %-10s %-24s %s\n", "NAME", "TTY", "STATE", "PROXY", "KEY")
		for _, d := range r.Devices {
			// 活跃会话显示客户端地址；无会话但端点在等则显示监听地址（listen: 前缀
			// 区分），两者皆空才是 "-"（#16：端点待命不可见是观测盲区）。
			px := "-"
			switch {
			case d.Proxy != "":
				px = d.Proxy
			case d.ProxyEndpoint != "":
				px = "listen:" + d.ProxyEndpoint
			}
			fmt.Printf("%-16s %-14s %-10s %-24s %s\n", d.Name, d.Tty, d.State, px, d.Key)
		}
		return true
	})
	if respErr != "" {
		return fmt.Errorf("%s", respErr)
	}
	return err
}

func cmdProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	sock := fs.String("sock", "", "控制 socket 路径")
	stop := fs.Bool("stop", false, "停止透传（默认开启）")
	pos := parseFlags(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("proxy 需要一个设备匹配正则，如 proxy luatos")
	}
	req := ctl.Request{Cmd: "proxy", Pattern: pos[0]}
	if *stop {
		req.Action = "stop"
	}
	var respErr string
	err := ctlSend(*sock, req, func(r ctl.Response) bool {
		if !r.OK {
			respErr = r.Error
			return true
		}
		if req.Action == "stop" {
			fmt.Printf("已停止透传（%s）\n", pos[0])
		} else {
			dev := pos[0]
			if r.Device != "" {
				dev = r.Device // 守护侧确认的端点所属设备（多板同名时以它为准）
			}
			fmt.Printf("透传端点: %s\n设备 %s 的串口现在可经该 TCP 端点直接读写（采集照常）\n", r.Endpoint, dev)
		}
		return true
	})
	if respErr != "" {
		return fmt.Errorf("%s", respErr)
	}
	return err
}

func cmdRelease(args []string) error {
	fs := flag.NewFlagSet("release", flag.ExitOnError)
	sock := fs.String("sock", "", "控制 socket 路径")
	forDur := fs.String("for", "", "限时自动回采（如 5m / 90s）；省略则端口空闲自动回采")
	pos := parseFlags(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("release 需要一个设备匹配正则，如 release luatos")
	}
	req := ctl.Request{Cmd: "release", Pattern: pos[0], UntilIdle: true}
	if *forDur != "" {
		d, err := time.ParseDuration(*forDur)
		if err != nil {
			return fmt.Errorf("--for 解析失败: %w", err)
		}
		req.ForMs = d.Milliseconds()
		req.UntilIdle = false
	}
	var respErr string
	err := ctlSend(*sock, req, func(r ctl.Response) bool {
		if !r.OK {
			respErr = r.Error // 统一由 Run 的错误出口打印
			return true
		}
		if req.ForMs > 0 {
			fmt.Printf("已让出端口（%s 限时 %s 后自动回采）—— 其他工具现在可用该口\n", pos[0], *forDur)
		} else {
			fmt.Printf("已让出端口（%s，空闲 3s 后自动回采）—— 其他工具现在可用该口\n", pos[0])
		}
		return true
	})
	if respErr != "" {
		return fmt.Errorf("%s", respErr)
	}
	return err
}

// noRetryError 标记不值得重试的失败（如守护进程不可达）。
type noRetryError struct{ error }

// flashRetry 让 flash 命令在客户端侧按次数重试。动机：Windows 上
// USB-CDC 设备复位/重枚举后的首次 open / SetCommState 常以
// ERROR_GEN_FAILURE 瞬时失败（实测 ESP32-S3 USB-Serial-JTAG，
// 2026-09-22），esptool 不做任何重试 —— 单发 CLI 一撞即退。
// 每次重试都会让守护进程完整走一遍 让口 → esptool → 回采 编排，
// 幂等且顺带充当了端口的"预热开合"。
type flashRetry struct {
	attempts int           // 总尝试次数（含首次），1 = 不重试
	wait     time.Duration // 尝试间隔
	sleep    func(time.Duration)
	logf     func(string, ...any)
}

func (r flashRetry) run(op func() error) error {
	var err error
	for i := 1; i <= r.attempts; i++ {
		if i > 1 {
			r.logf("—— flash 第 %d/%d 次尝试 ——", i, r.attempts)
		}
		if err = op(); err == nil {
			return nil
		}
		var nr noRetryError
		if errors.As(err, &nr) {
			return nr.error
		}
		if i < r.attempts {
			r.logf("— 尝试失败（%s），%s 后重试", err, r.wait)
			r.sleep(r.wait)
		}
	}
	return err
}

func cmdFlash(args []string) error {
	fs := flag.NewFlagSet("flash", flag.ExitOnError)
	sock := fs.String("sock", "", "控制 socket 路径")
	cfgPath := fs.String("config", "", "配置文件 JSON")
	esptool := fs.String("esptool", "", "esptool 命令（默认 PATH 自动发现或配置）")
	baud := fs.Int("baud", 0, "刷写波特率")
	chip := fs.String("chip", "", "芯片类型（如 esp32s3，省略自动识别）")
	argsFile := fs.String("args-file", "", "ESP-IDF build/flasher_args.json（与其余 bin 参数二选一）")
	dryRun := fs.Bool("dry-run", false, "只预演：显示每台匹配设备将执行的 esptool 命令，不动端口")
	all := fs.Bool("all", false, "模式匹配多台设备时仍逐台刷（默认拒绝——多板同名时防误刷在测设备，精确刷一台请锚定正则）")
	retries := fs.Int("retries", 3, "失败重试总次数（含首次；Windows CDC 复位后首开常瞬时失败，1=不重试）")
	retryWait := fs.Duration("retry-wait", 5*time.Second, "重试间隔")
	pos := parseFlags(fs, args)
	if len(pos) < 1 || (len(pos) < 2 && *argsFile == "") {
		return fmt.Errorf("用法: flash <设备正则> <镜像>[@<offset>]... 或 --args-file build/flasher_args.json")
	}
	cfg, err := loadCfgMerged(*cfgPath, "", 0)
	if err != nil {
		return err
	}
	spec := flash.Spec{ArgsFile: *argsFile, DryRun: *dryRun}
	if *esptool != "" {
		spec.Esptool = *esptool
	} else if cfg.Esptool != "" {
		spec.Esptool = cfg.Esptool
	}
	if *baud > 0 {
		spec.Baud = *baud
	} else if cfg.FlashBaud > 0 {
		spec.Baud = cfg.FlashBaud
	}
	spec.Chip = *chip
	for _, p := range pos[1:] {
		bin := flash.BinSpec{Path: p, Offset: "0x0"}
		if i := strings.LastIndex(p, "@"); i > 0 {
			bin.Path, bin.Offset = p[:i], p[i+1:]
		}
		spec.Bins = append(spec.Bins, bin)
	}
	// flash-done 带 ok=false 时 ctlSend 本身不报错（协议层正常），
	// 退出码要反映刷写失败 —— 脚本化调用依赖它。
	// 传输层错误（守护进程不可达等）包成 noRetryError：重试无益。
	runOnce := func() error {
		var flashErr error
		err = ctlSend(*sock, ctl.Request{Cmd: "flash", Pattern: pos[0], All: *all, Spec: spec}, func(r ctl.Response) bool {
			switch r.Event {
			case "flash-log":
				fmt.Println(r.Line)
				return false
			case "flash-done":
				if !r.OK {
					flashErr = fmt.Errorf("刷写失败: %s", r.Error)
				} else {
					fmt.Println("✓ 刷写完成，已恢复采集")
				}
				return true
			default:
				if !r.OK {
					flashErr = fmt.Errorf("%s", r.Error)
					return true
				}
				return false
			}
		})
		if err != nil {
			return noRetryError{err}
		}
		return flashErr
	}
	attempts := *retries
	if *dryRun || attempts < 1 {
		attempts = 1
	}
	return flashRetry{
		attempts: attempts,
		wait:     *retryWait,
		sleep:    time.Sleep,
		logf:     func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	}.run(runOnce)
}

// —— 串口层/USB 层软断开重连 ——

// cmdReopen: 串口层软断开重连（立即关口→跳过退避重开；打断透传会话）。
func cmdReopen(args []string) error {
	fs := flag.NewFlagSet("reopen", flag.ExitOnError)
	sock := fs.String("sock", "", "控制 socket 路径")
	all := fs.Bool("all", false, "模式匹配多台设备时仍逐台重开（默认拒绝——精确操作一台请锚定正则）")
	pos := parseFlags(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("reopen 需要一个设备匹配正则，如 reopen '^sense-c3$'")
	}
	var respErr string
	err := ctlSend(*sock, ctl.Request{Cmd: "reopen", Pattern: pos[0], All: *all}, func(r ctl.Response) bool {
		if !r.OK {
			respErr = r.Error
			return true
		}
		fmt.Printf("已触发 %s 台设备的串口软重连（关口→立即重开；透传客户端会断开，重连即可）\n", r.Line)
		return true
	})
	if respErr != "" {
		return fmt.Errorf("%s", respErr)
	}
	return err
}

// cmdReset: USB 层软拔插（让口 → pnputil 重启设备节点 → 回采，需管理员）。
func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	sock := fs.String("sock", "", "控制 socket 路径")
	all := fs.Bool("all", false, "模式匹配多台设备时仍逐台重置（默认拒绝——精确操作一台请锚定正则）")
	pos := parseFlags(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("reset 需要一个设备匹配正则，如 reset '^s3zero$'")
	}
	fmt.Println("USB 软重置中：让口 → pnputil 重启设备节点（若弹出 UAC 请确认）→ 回采…")
	var respErr string
	err := ctlSend(*sock, ctl.Request{Cmd: "reset", Pattern: pos[0], All: *all}, func(r ctl.Response) bool {
		if !r.OK {
			respErr = r.Error
			return true
		}
		fmt.Println("✓ USB 设备节点已重启，采集已恢复")
		return true
	})
	if respErr != "" {
		return fmt.Errorf("%s", respErr)
	}
	return err
}

// pause/resume：守护进程在 → socket（立即生效且走同一文件语义）；不在 → 直接改文件。
// 服务端拒绝（ok:false，如"刷写进行中"）必须原样报错退出 —— 不能回退文件直改
// （守护明明活着），也不能谎报成功。
func cmdPauseSocket(args []string, pauseMode bool) error {
	fs := flag.NewFlagSet("pause/resume", flag.ExitOnError)
	sock := fs.String("sock", "", "控制 socket 路径")
	fs.String("root", "", "日志根目录（回退文件直改时用）")
	pos := parseFlags(fs, args)
	pattern := ""
	if len(pos) > 0 {
		pattern = pos[0]
	}
	cmd := "resume"
	if pauseMode {
		cmd = "pause"
	}
	var respErr string
	err := ctlSend(*sock, ctl.Request{Cmd: cmd, Pattern: pattern},
		func(r ctl.Response) bool {
			if !r.OK && r.Error != "" {
				respErr = r.Error
			}
			return true
		})
	if err == nil {
		if respErr != "" {
			return fmt.Errorf("%s", respErr)
		}
		if pauseMode {
			fmt.Println("已暂停（守护进程已生效）")
		} else {
			fmt.Println("已恢复（守护进程已生效）")
		}
		return nil
	}
	// 守护不在（连接失败）→ 文件直改（历史行为）
	return cmdPauseCLI(args, pauseMode)
}
