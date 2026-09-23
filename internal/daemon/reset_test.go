package daemon

// reopen（串口层软重连）与 reset（USB 层软重枚举）的编排测试。
// usbRestartDevice 为 seam，pnputil 不真跑；端口/枚举走假件。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

// newMutableDaemon: enum 返回可变切片的守护（reset 测试要模拟设备消失/回来）。
// snapshot 由调用方提供并自行加锁 —— enum 轮询跑在别的 goroutine 上，
// 无锁直读共享切片会被 -race 抓（TestResetOrchestration 写侧有锁，读侧必须同锁）。
func newMutableDaemon(t *testing.T, root string, snapshot func() []device.DeviceInfo) (*daemon, error) {
	cfg := config.DefaultConfig()
	cfg.Root = root
	cfg.PollMs = 10
	return New(cfg, nil, func() ([]device.DeviceInfo, error) { return snapshot(), nil }, nil)
}

func fakePorts(t *testing.T) {
	t.Helper()
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{Chunks: [][]byte{[]byte("hello\n")}}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })
}

// 串口层软重连：立即重开（默认退避 5s——3s 内出现第二次 open 即证明跳过了退避），
// 事件流记录 manual reopen 而非 port lost 退避路径。
func TestReopenCyclesPortImmediately(t *testing.T) {
	root := t.TempDir()
	fakePorts(t)
	devs := []device.DeviceInfo{{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"}}
	d, err := newMutableDaemon(t, root, func() []device.DeviceInfo {
		return append([]device.DeviceInfo(nil), devs...)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	day := time.Now().Format("20060102")
	evFile := filepath.Join(root, "fakeA", "events-"+day+".log")
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Count(testutil.ReadFile(t, evFile), "serial opened") >= 1
	}, "首次打开未发生")

	n, err := d.Reopen("^fakeA$", false)
	if err != nil || n != 1 {
		t.Fatalf("Reopen 失败: n=%d err=%v", n, err)
	}
	// 退避下限默认 5s：3s 内完成第二次 open = 跳过了退避
	testutil.WaitFor(t, 3*time.Second, func() bool {
		ev := testutil.ReadFile(t, evFile)
		return strings.Count(ev, "serial opened") >= 2 && strings.Contains(ev, "manual reopen")
	}, "软重连未立即重开")
	if ev := testutil.ReadFile(t, evFile); strings.Contains(ev, "port lost") {
		t.Fatalf("软重连不应走 port lost 退避路径: %s", ev)
	}
}

// reopen/reset 与 flash 同款多设备门禁：未锚定匹配多台默认拒绝并列名，
// --all 才逐台执行。
func TestReopenResetMultiDeviceGate(t *testing.T) {
	root := t.TempDir()
	fakePorts(t)
	devs := []device.DeviceInfo{
		{Tty: "/dev/a", Key: "ka", Name: "sense", ByID: "USB\\VID_303A&PID_1001&MI_00\\7&1"},
		{Tty: "/dev/b", Key: "kb", Name: "sense-dev", ByID: "USB\\VID_303A&PID_1001&MI_00\\7&2"},
	}
	d, err := newMutableDaemon(t, root, func() []device.DeviceInfo {
		return append([]device.DeviceInfo(nil), devs...)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()

	if _, err := d.Reopen("sense", false); err == nil ||
		!strings.Contains(err.Error(), "2 台") || !strings.Contains(err.Error(), "sense-dev") ||
		!strings.Contains(err.Error(), "--all") {
		t.Fatalf("reopen 多台未确认应拒绝并列出设备: %v", err)
	}
	if n, err := d.Reopen("sense", true); err != nil || n != 2 {
		t.Fatalf("reopen all=true 应逐台: n=%d err=%v", n, err)
	}

	// reset 门禁同口径（seam 不真跑 pnputil）
	old := usbRestartDevice
	usbRestartDevice = func(string) (string, error) { return "restarted", nil }
	t.Cleanup(func() { usbRestartDevice = old })
	if err := d.Reset("sense", false); err == nil || !strings.Contains(err.Error(), "2 台") {
		t.Fatalf("reset 多台未确认应拒绝: %v", err)
	}
	if err := d.Reset("^sense-dev$", false); err != nil {
		t.Fatalf("reset 锚定单台应成功: %v", err)
	}
}

// reset 编排全流程（seam 模拟设备节点消失 400ms 后回来）：
// 让口 → 重启 → 枚举确认 → 回采；事件流含 start/finished。
func TestResetOrchestration(t *testing.T) {
	root := t.TempDir()
	fakePorts(t)
	var mu sync.Mutex // devs 的读写锁：enum 快照（别的 goroutine）与下面的消失/回来写都走它
	devs := []device.DeviceInfo{
		{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA", ByID: "USB\\VID_303A&PID_1001&MI_00\\7&fake&2&0000"},
	}
	d, err := newMutableDaemon(t, root, func() []device.DeviceInfo {
		mu.Lock()
		defer mu.Unlock()
		return append([]device.DeviceInfo(nil), devs...)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()

	var gotInstance string
	old := usbRestartDevice
	usbRestartDevice = func(instanceID string) (string, error) {
		mu.Lock()
		gotInstance = instanceID
		devs = nil // 设备节点消失（重枚举中）
		mu.Unlock()
		go func() { // 400ms 后节点回来
			time.Sleep(400 * time.Millisecond)
			mu.Lock()
			devs = []device.DeviceInfo{
				{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA", ByID: "USB\\VID_303A&PID_1001&MI_00\\7&fake&2&0000"},
			}
			mu.Unlock()
		}()
		return "restarted", nil
	}
	t.Cleanup(func() { usbRestartDevice = old })

	if err := d.Reset("^fakeA$", false); err != nil {
		t.Fatalf("Reset 失败: %v", err)
	}
	mu.Lock()
	inst := gotInstance
	mu.Unlock()
	if inst != "USB\\VID_303A&PID_1001&MI_00\\7&fake&2&0000" {
		t.Fatalf("应以 by-id 实例路径调重启: %q", inst)
	}
	day := time.Now().Format("20060102")
	ev := testutil.ReadFile(t, filepath.Join(root, "fakeA", "events-"+day+".log"))
	if !strings.Contains(ev, "usb reset start") || !strings.Contains(ev, "usb reset finished (err=<nil>)") {
		t.Fatalf("事件流缺 reset 里程碑: %s", ev)
	}
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return d.Status()[0].State == "collecting"
	}, "reset 后未回采")
}

// pnputil 失败：报错带设备名，且采集器照样回采（不让口卡死）。
func TestResetPnputilFailureStillResumes(t *testing.T) {
	root := t.TempDir()
	fakePorts(t)
	d, err := newMutableDaemon(t, root, func() []device.DeviceInfo {
		return []device.DeviceInfo{{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA", ByID: "USB\\X\\7&1"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()

	old := usbRestartDevice
	usbRestartDevice = func(string) (string, error) {
		return "无法重启设备: 拒绝访问。", os.ErrPermission
	}
	t.Cleanup(func() { usbRestartDevice = old })

	err = d.Reset("^fakeA$", false)
	if err == nil || !strings.Contains(err.Error(), "fakeA") {
		t.Fatalf("pnputil 失败应报错带设备名: %v", err)
	}
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return d.Status()[0].State == "collecting"
	}, "失败路径也应回采")
}

// 非 USB 枚举设备（by-id 空）无法 reset，明确报错。
func TestResetRequiresInstanceID(t *testing.T) {
	root := t.TempDir()
	fakePorts(t)
	devs := []device.DeviceInfo{{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"}}
	d, err := newMutableDaemon(t, root, func() []device.DeviceInfo {
		return append([]device.DeviceInfo(nil), devs...)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	if err := d.Reset("^fakeA$", false); err == nil || !strings.Contains(err.Error(), "实例路径") {
		t.Fatalf("by-id 空应拒绝: %v", err)
	}
}

// pnputil 输出标记判定（zh/en；退出码不可靠——实测失败也返回 0）。
func TestPnputilMarkers(t *testing.T) {
	if !accessDenied("Microsoft PnP 工具\r\n\r\n无法重启设备:  X\r\n拒绝访问。\r\n") {
		t.Fatal("中文拒绝访问应识别")
	}
	if !accessDenied("Access is denied.") {
		t.Fatal("英文拒绝访问应识别")
	}
	if accessDenied("已成功重新启动设备") {
		t.Fatal("成功输出不应误判为拒绝")
	}
	if !pnputilFailed("无法重启设备: X") || !pnputilFailed("failed to restart") {
		t.Fatal("失败标记应识别")
	}
	if pnputilFailed("已成功重新启动设备") {
		t.Fatal("成功输出不应误判为失败")
	}
}
