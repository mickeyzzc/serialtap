package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFlagsAnywhere(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	name := fs.String("name", "", "")
	baud := fs.Int("baud", 0, "")
	pos := parseFlags(fs, []string{"/dev/ttyACM0", "--name", "x", "--baud", "9600", "extra-pos"})
	if len(pos) != 2 || pos[0] != "/dev/ttyACM0" || pos[1] != "extra-pos" {
		t.Fatalf("位置参数错误: %v", pos)
	}
	if *name != "x" || *baud != 9600 {
		t.Fatalf("flag 解析错误: name=%q baud=%d", *name, *baud)
	}
}

func TestParseFlagsLeading(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	name := fs.String("name", "", "")
	pos := parseFlags(fs, []string{"--name", "y", "/dev/ttyUSB0"})
	if *name != "y" || len(pos) != 1 || pos[0] != "/dev/ttyUSB0" {
		t.Fatalf("前导 flag 解析错误: name=%q pos=%v", *name, pos)
	}
}

// —— daemon 生命周期（假枚举 + 假端口，无硬件）——
func TestDaemonLifecycle(t *testing.T) {
	root := t.TempDir()
	// 每次 open 都给全新端口（两个采集器不能共享同一份待读数据）
	old := portOpener
	portOpener = func(tty string, baud int) (serialPort, error) {
		return &fakePort{chunks: [][]byte{[]byte("hello from dev\n")}}, nil
	}
	t.Cleanup(func() { portOpener = old })

	cfg := DefaultConfig()
	cfg.Root = root
	cfg.PollMs = 10

	var devs = []DeviceInfo{
		{Tty: "/dev/fakeA", Key: "keyA", Name: "fakeA", ByID: "idA"},
		{Tty: "/dev/fakeB", Key: "keyB", Name: "fakeB", ByID: "idB"},
	}
	d, err := newDaemon(cfg, nil, func() ([]DeviceInfo, error) { return devs, nil }, discardLog)
	if err != nil {
		t.Fatal(err)
	}
	d.tick()
	if len(d.collectors) != 2 {
		t.Fatalf("应起 2 个采集器: %d", len(d.collectors))
	}
	day := time.Now().Format("20060102")
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readLogFile(t, filepath.Join(root, "fakeA", "serial-"+day+".log")), "hello from dev")
	}, "daemon 采集器未落盘")

	// 同 key 再 tick 不重复起
	d.tick()
	if len(d.collectors) != 2 {
		t.Fatalf("重复起采集器: %d", len(d.collectors))
	}

	// 设备移除 → 采集器停止并释放名字
	devs = devs[:1]
	d.tick()
	if len(d.collectors) != 1 {
		t.Fatalf("设备移除后应剩 1 个: %d", len(d.collectors))
	}

	// 重名设备 → -2 后缀
	devs = append(devs, DeviceInfo{Tty: "/dev/fakeA2", Key: "keyA2", Name: "fakeA"})
	d.tick()
	if c := d.collectors["keyA2"]; c == nil || c.dev.Name != "fakeA-2" {
		t.Fatalf("重名后缀错误: %+v", d.collectors["keyA2"])
	}

	// 暂停清单热重载
	os.WriteFile(PauseFilePath(root), []byte("fakeA\n"), 0o644)
	d.tick() // 触发 reloadPause（mtime 变更）
	waitFor(t, 3*time.Second, func() bool {
		d.pause.mu.RLock()
		defer d.pause.mu.RUnlock()
		return d.pause.Matches(DeviceInfo{Name: "fakeA"})
	}, "暂停清单未热重载")

	d.shutdown()
}

func TestDaemonEnumErrorDoesNotPanic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Root = t.TempDir()
	d, err := newDaemon(cfg, nil, func() ([]DeviceInfo, error) { return nil, os.ErrNotExist }, discardLog)
	if err != nil {
		t.Fatal(err)
	}
	d.tick() // 只记日志不崩
	d.shutdown()
}

func TestLoadCfgMergedOverrides(t *testing.T) {
	// 无配置文件环境（HOME 指到空目录）→ 纯默认
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := loadCfgMerged("", "", 0)
	if err != nil || cfg.Baud != 115200 || cfg.Root != "logs" {
		t.Fatalf("默认配置错误: %+v err=%v", cfg, err)
	}
	// flag 覆盖
	cfg, err = loadCfgMerged("", "/tmp/xx", 9600)
	if err != nil || cfg.Root != "/tmp/xx" || cfg.Baud != 9600 {
		t.Fatalf("flag 覆盖失败: %+v", cfg)
	}
	// 指定配置文件
	f := filepath.Join(home, "cfg.json")
	os.WriteFile(f, []byte(`{"root":"/from/file","baud":57600,"signatures_extra":["NO-SOI"]}`), 0o644)
	cfg, err = loadCfgMerged(f, "", 0)
	if err != nil || cfg.Root != "/from/file" || cfg.Baud != 57600 || len(cfg.ExtraSigs) != 1 {
		t.Fatalf("配置文件加载失败: %+v err=%v", cfg, err)
	}
	// 环配置文件 → 报错
	if _, err := loadCfgMerged(filepath.Join(home, "nope.json"), "", 0); err == nil {
		t.Fatal("缺文件应报错")
	}
	os.WriteFile(f, []byte("{bad json"), 0o644)
	if _, err := loadCfgMerged(f, "", 0); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestElfFromLogPath(t *testing.T) {
	cfg := Config{ElfMap: map[string]string{"esp32s3-jtag": "/x/y.elf"}}
	if got := elfFromLogPath("logs/esp32s3-jtag/serial-20260920.log", cfg); got != "/x/y.elf" {
		t.Fatalf("elf_map 映射失败: %q", got)
	}
	if got := elfFromLogPath("one.log", cfg); got != "" {
		t.Fatalf("短路径应返回空: %q", got)
	}
	if got := elfFromLogPath("logs/unknown/serial.log", cfg); got != "" {
		t.Fatalf("未映射设备应返回空: %q", got)
	}
}

func TestBacktraceAddrs(t *testing.T) {
	addrs := backtraceAddrs("Backtrace: 0x400D0C5A:0x3FFB7D40 0x4008C71E:0x3FFB7D60")
	if len(addrs) != 2 || addrs[0] != "0x400D0C5A" || addrs[1] != "0x4008C71E" {
		t.Fatalf("地址帧提取错误: %v", addrs)
	}
	addrs = backtraceAddrs("Backtrace: 0x4008:0x3ffb1c30 |<-0x4008")
	if len(addrs) != 2 || addrs[1] != "0x4008" {
		t.Fatalf("|<-PC 标记提取错误: %v", addrs)
	}
	if got := backtraceAddrs("no addresses here"); got != nil {
		t.Fatalf("无地址行应返回 nil: %v", got)
	}
}

func TestFindAddr2line(t *testing.T) {
	// 显式指定有效可执行文件
	if p, err := findAddr2line("/bin/echo"); err != nil || p == "" {
		t.Fatalf("显式指定失败: %q %v", p, err)
	}
	// 显式指定无效 → 报错
	if _, err := findAddr2line("/nonexistent/a2l"); err == nil {
		t.Fatal("无效路径应报错")
	}
	// 环境变量
	t.Setenv("ESP_ADDR2LINE", "/usr/local/my-a2l")
	if p, _ := findAddr2line(""); p != "/usr/local/my-a2l" {
		t.Fatalf("环境变量优先级错误: %q", p)
	}
}

func TestPauseCLIFileEditing(t *testing.T) {
	root := t.TempDir()
	if err := PauseCLI(root, true, []string{"ch340", "ttyACM1"}); err != nil {
		t.Fatal(err)
	}
	st, _, err := LoadPauseFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Matches(DeviceInfo{Name: "ch340"}) || !st.Matches(DeviceInfo{Tty: "/dev/ttyACM1"}) {
		t.Fatal("pause 清单未生效")
	}
	// 移除单条
	if err := PauseCLI(root, false, []string{"ch340"}); err != nil {
		t.Fatal(err)
	}
	st, _, _ = LoadPauseFile(root)
	if st.Matches(DeviceInfo{Name: "ch340"}) || !st.Matches(DeviceInfo{Tty: "/dev/ttyACM1"}) {
		t.Fatal("resume 单条失败")
	}
	// 全清 → 文件删除
	if err := PauseCLI(root, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PauseFilePath(root)); !os.IsNotExist(err) {
		t.Fatal("清空后 PAUSED 应删除")
	}
	// 无参 pause = 全部
	if err := PauseCLI(root, true, nil); err != nil {
		t.Fatal(err)
	}
	st, _, _ = LoadPauseFile(root)
	if !st.Matches(DeviceInfo{Name: "whatever"}) {
		t.Fatal("无参 pause 应匹配全部")
	}
}

func TestCmdAnalyzeNoMatch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "empty.log")
	os.WriteFile(f, []byte("I (1) all: fine\n"), 0o644)
	if err := cmdAnalyze([]string{f}, false); err != nil {
		t.Fatal(err)
	}
	// 缺文件 → 报错
	if err := cmdAnalyze([]string{filepath.Join(dir, "nope.log")}, false); err == nil {
		t.Fatal("缺文件应报错")
	}
}

func TestMultiFlag(t *testing.T) {
	var m multiFlag
	if err := m.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("b"); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m.String() != "" {
		t.Fatalf("multiFlag 错误: %v %q", m, m.String())
	}
}
