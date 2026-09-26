package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/daemon"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/pause"
)

// ctlSockPath: 控制 socket 测试路径。macOS sun_path 上限 104 字节，
// t.TempDir() 在 darwin 上太长（bind 报 invalid argument）→ 用 /tmp 短路径。
func ctlSockPath(t *testing.T, name string) string {
	if runtime.GOOS == "darwin" {
		p := filepath.Join("/tmp", fmt.Sprintf("serialtap-cli-test-%d-%s.sock", os.Getpid(), name))
		t.Cleanup(func() { os.Remove(p) })
		return p
	}
	return filepath.Join(t.TempDir(), name+".sock")
}

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
	// os.UserHomeDir() 在 Windows 上看 USERPROFILE 而非 HOME —— 只设 HOME
	// 隔离不了真机上的 ~/.config/serialtap/config.json（CI 无该文件所以从未暴露）
	t.Setenv("USERPROFILE", home)
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
	sock := ctlSockPath(t, "live")
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
		case "proxy":
			if req.Pattern == "" {
				respond(ctl.Response{OK: false, Error: "no pattern"})
				return
			}
			if req.Action == "stop" {
				respond(ctl.Response{OK: true, Line: "1"})
				return
			}
			respond(ctl.Response{OK: true, Endpoint: "127.0.0.1:7100",
				Device: "luatos", DeviceKey: "k1"})
		case "reopen", "reset":
			if req.Pattern == "" {
				respond(ctl.Response{OK: false, Error: "no pattern"})
				return
			}
			respond(ctl.Response{OK: true, Line: "1"})
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
	// proxy 开启（回端点）
	if code := Run([]string{"proxy", "luatos", "--sock", sock}); code != 0 {
		t.Fatalf("proxy 失败: %d", code)
	}
	// proxy 停止
	if code := Run([]string{"proxy", "luatos", "--stop", "--sock", sock}); code != 0 {
		t.Fatalf("proxy --stop 失败: %d", code)
	}
	// proxy 缺参数
	if code := Run([]string{"proxy", "--sock", sock}); code != 1 {
		t.Fatalf("proxy 缺设备应退出码 1: %d", code)
	}
	// reopen / reset（含 --all）
	if code := Run([]string{"reopen", "^sense$", "--sock", sock}); code != 0 {
		t.Fatalf("reopen 失败: %d", code)
	}
	if code := Run([]string{"reopen", "sense", "--all", "--sock", sock}); code != 0 {
		t.Fatalf("reopen --all 失败: %d", code)
	}
	if code := Run([]string{"reset", "^s3zero$", "--sock", sock}); code != 0 {
		t.Fatalf("reset 失败: %d", code)
	}
	if code := Run([]string{"reset", "--sock", sock}); code != 1 {
		t.Fatalf("reset 缺设备应退出码 1: %d", code)
	}
	// --for 坏值
	if code := Run([]string{"release", "x", "--for", "bad", "--sock", sock}); code != 1 {
		t.Fatalf("坏 --for 应退出码 1: %d", code)
	}
}

func TestPauseFallsBackToFileWhenNoDaemon(t *testing.T) {
	// socket 不存在 → 回退文件直改（历史行为）
	root := t.TempDir()
	sock := ctlSockPath(t, "gone")
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

// 服务端 ok:false 必须反映为非零退出码（此前 release/status/pause 只打
// stderr 却退出 0，脚本化会误判成功）
func TestCtlServerErrorPropagatesExitCode(t *testing.T) {
	// TODO(Windows): 本测试在 windows runner 上死锁（ctl.Send/Accept 在 Go 的
	// af_unix 实现上不返回，10m 超时）。该分支此前从未在 Windows 跑过测试，
	// 属既有问题 —— 需在 Windows 上定位后恢复。
	if runtime.GOOS == "windows" {
		t.Skip("ctl unix socket 在 Windows 上存在死锁，见 TODO")
	}
	sock := ctlSockPath(t, "ctl-err")
	srv, err := ctl.Listen(sock)
	if err != nil {
		t.Skipf("本平台无法监听 unix socket: %v", err)
	}
	t.Cleanup(srv.Close)
	go srv.Serve(func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: false, Error: "boom"})
	})
	for _, args := range [][]string{
		{"status", "--sock", sock},
		{"release", "x", "--sock", sock},
		{"pause", "--sock", sock},
		{"resume", "--sock", sock},
	} {
		if rc := Run(args); rc != 1 {
			t.Fatalf("Run(%v) = %d, want 1", args, rc)
		}
	}
}

func TestStateZH(t *testing.T) {
	cases := map[string]string{
		"collecting": "● 采集中",
		"paused":     "⏸ 已暂停",
		"suspended":  "↩ 让口中",
		"flashing":   "⚡ 刷写中",
		"odd-state":  "odd-state", // 未知状态原样透传
	}
	for in, want := range cases {
		if got := stateZH(in); got != want {
			t.Fatalf("stateZH(%q) = %q, want %q", in, got, want)
		}
	}
	discardLog("覆盖无操作 sink %d", 1)
}

// trayHost 在 Linux（无托盘）上只是不被调用，构造与回调契约仍须可测：
// 面板 URL 归一化、空设备 Status=nil、PauseAll/ResumeAll 走 PAUSED 文件、Quit 透传。
func TestTrayHost(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{Root: root}
	d, err := daemon.New(cfg, nil, func() ([]device.DeviceInfo, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)

	h := trayHost(d, cfg, func() {})
	if h.Version != Version {
		t.Fatalf("Version = %q, want %q", h.Version, Version)
	}
	if h.PanelURL != "http://127.0.0.1:8801/" {
		t.Fatalf("缺省 WebAddr 应给出默认面板 URL, got %q", h.PanelURL)
	}
	if h := trayHost(d, config.Config{Root: root, WebAddr: "off"}, nil); h.PanelURL != "" {
		t.Fatalf("WebAddr=off 不应给面板按钮, got %q", h.PanelURL)
	}
	if lines := h.Status(); len(lines) != 0 {
		t.Fatalf("无设备时 Status 应为空, got %v", lines)
	}
	if err := h.PauseAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "PAUSED")); err != nil {
		t.Fatalf("PauseAll 应写 PAUSED 文件: %v", err)
	}
	if err := h.ResumeAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "PAUSED")); !os.IsNotExist(err) {
		t.Fatal("ResumeAll 后 PAUSED 应移除")
	}
	quit := false
	hq := trayHost(d, cfg, func() { quit = true })
	hq.Quit()
	if !quit {
		t.Fatal("Quit 应回调退出钩子")
	}
	_ = h.OpenLogs() // 平台相关（open 命令），覆盖即可不断言
}
