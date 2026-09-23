package tray

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

func TestPatternFor(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"esp32s3-jtag", `^esp32s3-jtag$`},
		{"cu.usbmodem2101", `^cu\.usbmodem2101$`}, // '.' 必须转义
		{"a+b", `^a\+b$`},
	} {
		if got := PatternFor(c.in); got != c.want {
			t.Fatalf("PatternFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDeviceTitleAndState(t *testing.T) {
	d := ctl.DevState{Name: "board", Tty: "COM3", State: "paused"}
	if got := DeviceTitle(d); got != "⏸ board (COM3)" {
		t.Fatalf("DeviceTitle: %q", got)
	}
	if !Collecting("collecting") || Collecting("paused") {
		t.Fatal("Collecting 语义错误")
	}
	if StateIcon("flashing") != "⚡" || StateIcon("??") != "·" {
		t.Fatal("StateIcon 语义错误")
	}
}

func TestSnapshotHashChangesOnState(t *testing.T) {
	a := []ctl.DevState{{Name: "a", Tty: "COM3", State: "collecting"}}
	if SnapshotHash(a, true) == SnapshotHash(a, false) {
		t.Fatal("daemon 状态变化应改变 hash")
	}
	b := []ctl.DevState{{Name: "a", Tty: "COM3", State: "paused"}}
	if SnapshotHash(a, true) == SnapshotHash(b, true) {
		t.Fatal("设备状态变化应改变 hash")
	}
	h1, h2 := SnapshotHash(a, true), SnapshotHash(a, true)
	if h1 != h2 {
		t.Fatal("相同快照 hash 应稳定")
	}
}

func TestLatestSerialLog(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "board")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"serial-20260920.log", "serial-20260921.001.log", "serial-20260921.log",
		"events-20260921.log", "serial-20260921.log.bak",
	} {
		if err := os.WriteFile(filepath.Join(dev, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 轮转后同日内后缀越大越新：.001 是基础文件写满后正在写的文件
	// （字典序会错判：".001" < ".log"）
	want := filepath.Join(dev, "serial-20260921.001.log")
	if got := LatestSerialLog(dir, "board"); got != want {
		t.Fatalf("LatestSerialLog = %q, want %q", got, want)
	}
	if got := LatestSerialLog(dir, "nope"); got != "" {
		t.Fatalf("不存在设备应返回空: %q", got)
	}
}

// QueryStatus: 真 ctl 服务端往返 + 不可达 socket 的两条路径
func TestQueryStatus(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "t.sock")
	srv, err := ctl.Listen(sock)
	if err != nil {
		t.Skipf("本平台无法监听 unix socket: %v", err)
	}
	t.Cleanup(srv.Close)
	go srv.Serve(func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true, Devices: []ctl.DevState{
			{Name: "a", Tty: "COM3", State: "collecting"},
		}})
	})
	testutil.WaitFor(t, 3*time.Second, func() bool {
		devs, ok := QueryStatus(sock)
		return ok && len(devs) == 1 && devs[0].Name == "a"
	}, "QueryStatus 未取到服务端设备")
	if _, ok := QueryStatus(filepath.Join(t.TempDir(), "absent.sock")); ok {
		t.Fatal("不可达 socket 应返回 false")
	}
}

func TestSortedDevicesStable(t *testing.T) {
	in := []ctl.DevState{
		{Name: "b", Tty: "COM5", State: "collecting"},
		{Name: "a", Tty: "COM3", State: "paused"},
		{Name: "c", Tty: "COM9", State: "collecting"},
	}
	got := SortedDevices(in)
	if got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "c" {
		t.Fatalf("未按名排序: %+v", got)
	}
	// 入参切片不被改动（daemon.Status 遍历 map 顺序随机，原序由调用方持有）
	if in[0].Name != "b" {
		t.Fatalf("入参被改动: %+v", in)
	}
}

// SendPause/SendResume：经真 socket 的往返（daemon handler 的同款语义）。
func TestSendPauseResumeRoundTrip(t *testing.T) {
	sock := sockPathT(t, "send-pause")
	srv, err := ctl.Listen(sock)
	if err != nil {
		t.Skipf("本平台无法监听 unix socket: %v", err)
	}
	t.Cleanup(srv.Close)
	var gotPattern string
	go srv.Serve(func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd == "pause" || req.Cmd == "resume" {
			gotPattern = req.Pattern
		}
		respond(ctl.Response{OK: true})
	})
	if err := SendPause(sock, "^board$"); err != nil {
		t.Fatal(err)
	}
	if err := SendResume(sock, ""); err != nil {
		t.Fatal(err)
	}
	if gotPattern != "" {
		t.Fatalf("resume 空 pattern 应覆盖为空: %q", gotPattern)
	}
}

// sockPathT: 测试 socket 短路径（macOS sun_path 104 字节上限）。
func sockPathT(t *testing.T, name string) string {
	if runtime.GOOS == "darwin" {
		p := filepath.Join("/tmp", fmt.Sprintf("serialtap-tray-test-%d-%s.sock", os.Getpid(), name))
		t.Cleanup(func() { os.Remove(p) })
		return p
	}
	return filepath.Join(t.TempDir(), name+".sock")
}
