package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
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
  serialtap pause [RE]...                暂停采集（省略=全部）— USB 刷写前必做
  serialtap resume [RE]...               恢复采集（省略=全部）
  serialtap version

日志布局: <root>/<设备名>/serial-YYYYMMDD.log（全量）+ events-YYYYMMDD.log（事件）
暂停清单: <root>/PAUSED（每行一个正则，匹配 tty/by-path/by-id/设备名）
`, Version)
}

// loadCfgMerged: --config 显式指定 > 默认路径（存在才读）> 纯默认。
func loadCfgMerged(configPath, rootFlag string, baud int) (Config, error) {
	path := configPath
	if path == "" {
		if def := DefaultConfigPath(); fileExists(def) {
			path = def
		}
	}
	cfg, err := LoadConfig(path)
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
	fmt.Printf("["+stamp(time.Now())+"] "+format+"\n", args...)
}

func discardLog(string, ...any) {}

func main() {
	os.Exit(runMain(os.Args[1:]))
}

// runMain: 命令分发（返回退出码；独立出来便于测试）。
func runMain(args []string) int {
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
	case "pause":
		err = cmdPauseCLI(args[1:], true)
	case "resume":
		err = cmdPauseCLI(args[1:], false)
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

// —— 守护循环 ——

// daemon: run 命令的可测核心。每 tick 枚举设备、起停采集器、热重载暂停清单。
type daemon struct {
	cfg        Config
	excl       []*regexp.Regexp
	eng        *SignatureEngine
	pause      *PauseState
	pauseMTime time.Time
	collectors map[string]*Collector
	namesUsed  map[string]bool
	enum       func() ([]DeviceInfo, error)
	logf       func(format string, args ...any)
	wg         sync.WaitGroup // 采集器 Run 协程追踪（shutdown 等待，防泄漏）
}

func newDaemon(cfg Config, excl []*regexp.Regexp, enum func() ([]DeviceInfo, error), logf func(string, ...any)) (*daemon, error) {
	if logf == nil {
		logf = discardLog
	}
	if enum == nil {
		enum = func() ([]DeviceInfo, error) { return Enumerate(excl, cfg.Names) }
	}
	pause, mtime, err := LoadPauseFile(cfg.Root)
	if err != nil {
		return nil, err
	}
	return &daemon{
		cfg: cfg, excl: excl, eng: NewSignatureEngine(cfg.ExtraSigs),
		pause: pause, pauseMTime: mtime,
		collectors: map[string]*Collector{}, namesUsed: map[string]bool{},
		enum: enum, logf: logf,
	}, nil
}

// tick: 单轮巡检（枚举 diff、起停采集器、暂停清单热重载）。
func (d *daemon) tick() {
	infos, err := d.enum()
	if err != nil {
		d.logf("[watch] 枚举失败: %v", err)
	}
	seen := map[string]bool{}
	for _, inf := range infos {
		seen[inf.Key] = true
		if _, ok := d.collectors[inf.Key]; ok {
			continue
		}
		// 重名设备加后缀（同型号适配器 by-id 无序列号时可能撞名）
		name := inf.Name
		for i := 2; d.namesUsed[name]; i++ {
			name = fmt.Sprintf("%s-%d", inf.Name, i)
		}
		d.namesUsed[name] = true
		inf.Name = name
		w, err := NewDeviceWriter(d.cfg.Root, name, d.cfg.RotateMB)
		if err != nil {
			d.logf("[watch] 建目录失败 %s: %v", name, err)
			continue
		}
		c := NewCollector(inf, d.cfg, w, d.eng, d.pause, d.logf)
		d.collectors[inf.Key] = c
		d.logf("[watch] 设备接入 %s → %s (key=%s by-id=%s vid:pid=%s:%s)",
			inf.Tty, name, inf.Key, inf.ByID, inf.VID, inf.PID)
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			c.Run()
		}()
	}
	for key, c := range d.collectors {
		if !seen[key] {
			d.logf("[watch] 设备移除 %s (key=%s)", c.dev.Tty, key)
			c.Stop()
			delete(d.collectors, key)
			delete(d.namesUsed, c.dev.Name)
		}
	}
	d.reloadPause()
}

// reloadPause: PAUSED 文件 mtime 变更 → 热替换暂停模式表。
func (d *daemon) reloadPause() {
	if fi, err := os.Stat(PauseFilePath(d.cfg.Root)); err == nil {
		if !fi.ModTime().Equal(d.pauseMTime) {
			d.pauseMTime = fi.ModTime()
			np, _, err := LoadPauseFile(d.cfg.Root)
			if err == nil {
				d.pause.mu.Lock()
				d.pause.pats = np.pats
				d.pause.mu.Unlock()
				d.logf("[watch] PAUSED 重载: %d 条模式", len(np.pats))
			}
		}
		return
	}
	d.pause.mu.Lock()
	if len(d.pause.pats) != 0 {
		d.pause.pats = nil
		d.logf("[watch] PAUSED 移除 — 恢复采集")
	}
	d.pause.mu.Unlock()
	d.pauseMTime = time.Time{}
}

func (d *daemon) shutdown() {
	for _, c := range d.collectors {
		c.Stop()
	}
	d.wg.Wait() // 等全部采集协程退出，不留泄漏 goroutine
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
	excl, bad := CompilePatterns(exclPats)
	for _, b := range bad {
		stdoutLog("[watch] 忽略坏正则: %s", b)
	}

	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return err
	}
	d, err := newDaemon(cfg, excl, nil, stdoutLog)
	if err != nil {
		return err
	}

	if n, err := SweepRetention(cfg.Root, cfg.RetentionDays); err == nil && n > 0 {
		stdoutLog("[watch] 保留期清理: 删除 %d 个旧日志", n)
	}
	stdoutLog("[watch] serialtap run v%s root=%s poll=%dms", Version, cfg.Root, cfg.PollMs)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	tick := time.NewTicker(time.Duration(cfg.PollMs) * time.Millisecond)
	defer tick.Stop()
	sweepTick := time.NewTicker(time.Hour)
	defer sweepTick.Stop()

	for {
		d.tick()
		select {
		case <-sigCh:
			stdoutLog("[watch] 退出信号 — 停止 %d 个采集器", len(d.collectors))
			d.shutdown()
			return nil
		case <-tick.C:
		case <-sweepTick.C:
			if n, err := SweepRetention(cfg.Root, cfg.RetentionDays); err == nil && n > 0 {
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
	w, err := NewDeviceWriter(cfg.Root, devName, cfg.RotateMB)
	if err != nil {
		return err
	}
	pause, _, err := LoadPauseFile(cfg.Root)
	if err != nil {
		return err
	}
	dev := DeviceInfo{Tty: tty, Key: "attach:" + tty, Name: SanitizeName(devName)}
	c := NewCollector(dev, cfg, w, NewSignatureEngine(cfg.ExtraSigs), pause, stdoutLog)

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
	excl, _ := CompilePatterns(cfg.Exclude)
	infos, err := Enumerate(excl, cfg.Names)
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
	return cmdAnalyze(pos, *lines)
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
	return cmdDecodeBacktrace(pos[0], *elf, *a2l, cfg)
}

func cmdPauseCLI(args []string, pause bool) error {
	fs := flag.NewFlagSet("pause/resume", flag.ExitOnError)
	root := fs.String("root", "", "日志根目录")
	pos := parseFlags(fs, args)
	r := *root
	if r == "" {
		cfg, err := LoadConfig(DefaultConfigPath())
		if err == nil {
			r = cfg.Root
		} else {
			r = DefaultConfig().Root
		}
	}
	return PauseCLI(r, pause, pos)
}

type multiFlag []string

func (m *multiFlag) String() string { return "" }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
