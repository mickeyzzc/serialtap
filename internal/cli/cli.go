// Package cli 实现命令行界面：子命令分发、flag 解析与各命令编排。
package cli

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
)

const Version = "0.1.0"

func usage() {
	fmt.Fprintf(os.Stderr, `serialtap v%s — USB 串口持续采集器

用法:
  serialtap run                          守护模式：轮询发现 USB 串口，自动起采集器
      [--config F] [--root DIR] [--baud N] [--exclude RE]... [--poll-ms N]
  serialtap attach TTY [--name N]        单口采集（手动/测试，可接 socat PTY）
      [--config F] [--root DIR] [--baud N]
  serialtap list [--config F]            列出当前设备与身份
  serialtap analyze LOG... [--lines]     离线签名扫描汇总
  serialtap decode-backtrace LOG         Backtrace addr2line 解码
      [--elf F] [--addr2line BIN] [--config F]
  serialtap pause [RE]...                暂停采集（省略=全部）
  serialtap resume [RE]...               恢复采集（省略=全部）
  serialtap release RE [--for 5m]        临时让出串口给外部工具（默认空闲 3s 自动回采）
  serialtap flash RE <bin>[@0x10000]...  代理刷固件：让口 → esptool → 自动回采
      [--args-file F] [--esptool CMD] [--baud N] [--chip C]
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
	case "release":
		err = cmdRelease(args[1:])
	case "flash":
		err = cmdFlash(args[1:])
	case "pause":
		err = cmdPauseSocket(args[1:], true)
	case "resume":
		err = cmdPauseSocket(args[1:], false)
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
	parseFlags(fs, args)

	cfg, err := loadCfgMerged(*cfgPath, *root, *baud)
	if err != nil {
		return err
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
	if sockPath == "" {
		sockPath = ctl.DefaultSocketPath()
	}
	ctlSrv, err := ctl.Listen(sockPath)
	if err != nil {
		return err
	}
	defer ctlSrv.Close()
	go ctlSrv.Serve(func(req ctl.Request, respond func(ctl.Response)) {
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
		case "release":
			forDur := time.Duration(req.ForMs) * time.Millisecond
			n, err := d.Release(req.Pattern, forDur, req.UntilIdle)
			if err != nil {
				respond(ctl.Response{OK: false, Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true, Line: fmt.Sprintf("%d", n)})
		case "flash":
			err := d.Flash(req.Pattern, req.Spec, func(line string) {
				respond(ctl.Response{OK: true, Event: "flash-log", Line: line})
			})
			if err != nil {
				respond(ctl.Response{OK: false, Event: "flash-done", Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true, Event: "flash-done"})
		default:
			respond(ctl.Response{OK: false, Error: "unknown cmd: " + req.Cmd})
		}
	})
	stdoutLog("[ctl] 控制通道: %s", sockPath)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	tick := time.NewTicker(time.Duration(cfg.PollMs) * time.Millisecond)
	defer tick.Stop()
	sweepTick := time.NewTicker(time.Hour)
	defer sweepTick.Stop()

	for {
		d.Tick()
		select {
		case <-sigCh:
			stdoutLog("[watch] 退出信号 — 停止 %d 个采集器", d.Collectors())
			d.Shutdown()
			return nil
		case <-tick.C:
		case <-sweepTick.C:
			if n, err := logstore.SweepRetention(cfg.Root, cfg.RetentionDays); err == nil && n > 0 {
				stdoutLog("[watch] 保留期清理: 删除 %d 个旧日志", n)
			}
		}
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
	return ctlSend(*sock, ctl.Request{Cmd: "status"}, func(r ctl.Response) bool {
		if !r.OK {
			return errOut(r.Error)
		}
		if len(r.Devices) == 0 {
			fmt.Println("（无采集设备）")
			return true
		}
		fmt.Printf("%-16s %-14s %-10s %s\n", "NAME", "TTY", "STATE", "KEY")
		for _, d := range r.Devices {
			fmt.Printf("%-16s %-14s %-10s %s\n", d.Name, d.Tty, d.State, d.Key)
		}
		return true
	})
}

func errOut(msg string) bool {
	fmt.Fprintln(os.Stderr, "错误:", msg)
	return true
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
	return ctlSend(*sock, req, func(r ctl.Response) bool {
		if !r.OK {
			return errOut(r.Error)
		}
		if req.ForMs > 0 {
			fmt.Printf("已让出端口（%s 限时 %s 后自动回采）—— 其他工具现在可用该口\n", pos[0], *forDur)
		} else {
			fmt.Printf("已让出端口（%s，空闲 3s 后自动回采）—— 其他工具现在可用该口\n", pos[0])
		}
		return true
	})
}

func cmdFlash(args []string) error {
	fs := flag.NewFlagSet("flash", flag.ExitOnError)
	sock := fs.String("sock", "", "控制 socket 路径")
	cfgPath := fs.String("config", "", "配置文件 JSON")
	esptool := fs.String("esptool", "", "esptool 命令（默认 PATH 自动发现或配置）")
	baud := fs.Int("baud", 0, "刷写波特率")
	chip := fs.String("chip", "", "芯片类型（如 esp32s3，省略自动识别）")
	argsFile := fs.String("args-file", "", "ESP-IDF build/flasher_args.json（与其余 bin 参数二选一）")
	pos := parseFlags(fs, args)
	if len(pos) < 1 || (len(pos) < 2 && *argsFile == "") {
		return fmt.Errorf("用法: flash <设备正则> <镜像>[@<offset>]... 或 --args-file build/flasher_args.json")
	}
	cfg, err := loadCfgMerged(*cfgPath, "", 0)
	if err != nil {
		return err
	}
	spec := flash.Spec{ArgsFile: *argsFile}
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
	return ctlSend(*sock, ctl.Request{Cmd: "flash", Pattern: pos[0], Spec: spec}, func(r ctl.Response) bool {
		switch r.Event {
		case "flash-log":
			fmt.Println(r.Line)
			return false
		case "flash-done":
			if !r.OK {
				fmt.Fprintf(os.Stderr, "刷写失败: %s\n", r.Error)
			} else {
				fmt.Println("✓ 刷写完成，已恢复采集")
			}
			return true
		default:
			if !r.OK {
				return errOut(r.Error)
			}
			return false
		}
	})
}

// pause/resume：守护进程在 → socket（立即生效且走同一文件语义）；不在 → 直接改文件
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
	err := ctlSend(*sock, ctl.Request{Cmd: cmd, Pattern: pattern}, func(r ctl.Response) bool { return true })
	if err == nil {
		if pauseMode {
			fmt.Println("已暂停（守护进程已生效）")
		} else {
			fmt.Println("已恢复（守护进程已生效）")
		}
		return nil
	}
	// 守护不在 → 文件直改（历史行为）
	return cmdPauseCLI(args, pauseMode)
}
