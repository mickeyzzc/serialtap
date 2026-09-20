package cli

import (
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/pause"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

func TestRunDispatch(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"version"}, 0},
		{nil, 2},
		{[]string{"no-such-cmd"}, 2},
		{[]string{"analyze"}, 1},
		{[]string{"decode-backtrace"}, 1},
		{[]string{"attach"}, 1},
		{[]string{"pause", "--root", t.TempDir()}, 0},
		{[]string{"resume", "--root", t.TempDir()}, 0},
		{[]string{"list"}, 0}, // 真实 sysfs，有无设备都不算错
	}
	for _, c := range cases {
		if got := Run(c.args); got != c.want {
			t.Fatalf("Run(%v) = %d, want %d", c.args, got, c.want)
		}
	}
}

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

func TestLoadCfgMerged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := loadCfgMerged("", "", 0)
	if err != nil || cfg.Baud != 115200 || cfg.Root != "logs" {
		t.Fatalf("默认配置错误: %+v err=%v", cfg, err)
	}
	cfg, err = loadCfgMerged("", "/tmp/xx", 9600)
	if err != nil || cfg.Root != "/tmp/xx" || cfg.Baud != 9600 {
		t.Fatalf("flag 覆盖失败: %+v", cfg)
	}
	f := filepath.Join(home, "cfg.json")
	os.WriteFile(f, []byte(`{"root":"/from/file","baud":57600,"signatures_extra":["NO-SOI"]}`), 0o644)
	cfg, err = loadCfgMerged(f, "", 0)
	if err != nil || cfg.Root != "/from/file" || cfg.Baud != 57600 || len(cfg.ExtraSigs) != 1 {
		t.Fatalf("配置文件加载失败: %+v err=%v", cfg, err)
	}
	if _, err := loadCfgMerged(filepath.Join(home, "nope.json"), "", 0); err == nil {
		t.Fatal("缺文件应报错")
	}
	os.WriteFile(f, []byte("{bad json"), 0o644)
	if _, err := loadCfgMerged(f, "", 0); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

// run 优雅退出：SIGTERM → 停采集器返回（--exclude .* 避免碰真实设备）。
func TestRunGracefulShutdownOnSIGTERM(t *testing.T) {
	root := t.TempDir()
	go func() {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
	t.Cleanup(func() { signal.Reset(syscall.SIGTERM, syscall.SIGINT) })
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"run", "--root", root, "--exclude", ".*", "--poll-ms", "50"})
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run 应优雅退出: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run 未随 SIGTERM 退出")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root 目录应已创建: %v", err)
	}
}

func TestAttachLifecycle(t *testing.T) {
	root := t.TempDir()
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{Chunks: [][]byte{[]byte("attached line\n")}}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	go func() {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
	t.Cleanup(func() { signal.Reset(syscall.SIGTERM, syscall.SIGINT) })
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"attach", "/dev/fakeTTY", "--name", "attachtest", "--root", root})
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("attach 应优雅退出: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach 未随 SIGTERM 退出")
	}
	day := time.Now().Format("20060102")
	ser := testutil.ReadFile(t, filepath.Join(root, "attachtest", "serial-"+day+".log"))
	if !strings.Contains(ser, "attached line") {
		t.Fatalf("attach 未落盘: %q", ser)
	}
}

func TestMiscSmallSurfaces(t *testing.T) {
	usage() // 覆盖 usage 输出（写到 stderr）
	stdoutLog("hi %d", 1)
	discardLog("hi")

	if _, err := collector.OpenPort("/dev/definitely-not-here", 115200); err == nil {
		t.Fatal("不存在设备应报错")
	}
	if !strings.HasPrefix(Version, "0.") {
		t.Fatalf("Version 格式异常: %s", Version)
	}
	var m multiFlag
	if err := m.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("b"); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m.String() != "" {
		t.Fatalf("multiFlag 错误: %v", m)
	}
}

// —— 控制通道子命令（本地起真 socket 服务验证 CLI → 协议全链路）——
func TestCtlSubcommandsAgainstLiveServer(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ctl.sock")
	srv, err := ctl.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(func(req ctl.Request, respond func(ctl.Response)) {
		switch req.Cmd {
		case "status":
			respond(ctl.Response{OK: true, Devices: []ctl.DevState{
				{Name: "luatos", Tty: "/dev/ttyACM1", Key: "k1", State: "collecting"}}})
		case "release":
			if req.Pattern == "" || (!req.UntilIdle && req.ForMs == 0) {
				respond(ctl.Response{OK: false, Error: "bad release"})
				return
			}
			respond(ctl.Response{OK: true, Line: "1"})
		case "flash":
			if req.Spec.Bins == nil && req.Spec.ArgsFile == "" {
				respond(ctl.Response{OK: false, Error: "no bins"})
				return
			}
			respond(ctl.Response{OK: true, Event: "flash-log", Line: "progress 1"})
			respond(ctl.Response{OK: true, Event: "flash-done"})
		default:
			respond(ctl.Response{OK: true})
		}
	})
	t.Cleanup(srv.Close)

	// status
	if code := Run([]string{"status", "--sock", sock}); code != 0 {
		t.Fatalf("status 失败: %d", code)
	}
	// release（默认 until_idle）
	if code := Run([]string{"release", "luatos", "--sock", sock}); code != 0 {
		t.Fatalf("release 失败: %d", code)
	}
	// release --for
	if code := Run([]string{"release", "luatos", "--for", "2m", "--sock", sock}); code != 0 {
		t.Fatalf("release --for 失败: %d", code)
	}
	// flash bin@offset
	if code := Run([]string{"flash", "luatos", "/dev/null@0x10000", "--sock", sock}); code != 0 {
		t.Fatalf("flash 失败: %d", code)
	}
	// flash 参数校验失败（无 bin 无 args-file）
	if code := Run([]string{"flash", "luatos", "--sock", sock}); code != 1 {
		t.Fatalf("flash 无镜像应退出码 1: %d", code)
	}
	// release 缺参数
	if code := Run([]string{"release", "--sock", sock}); code != 1 {
		t.Fatalf("release 缺设备应退出码 1: %d", code)
	}
	// --for 坏值
	if code := Run([]string{"release", "x", "--for", "bad", "--sock", sock}); code != 1 {
		t.Fatalf("坏 --for 应退出码 1: %d", code)
	}
}

func TestPauseFallsBackToFileWhenNoDaemon(t *testing.T) {
	// socket 不存在 → 回退文件直改（历史行为）
	root := t.TempDir()
	sock := filepath.Join(t.TempDir(), "gone.sock")
	if code := Run([]string{"pause", "ch340", "--sock", sock, "--root", root}); code != 0 {
		t.Fatalf("pause 文件回退失败: %d", code)
	}
	if _, err := os.Stat(pause.PauseFilePath(root)); err != nil {
		t.Fatal("回退后 PAUSED 文件应存在")
	}
	if code := Run([]string{"resume", "--sock", sock, "--root", root}); code != 0 {
		t.Fatalf("resume 文件回退失败: %d", code)
	}
}
