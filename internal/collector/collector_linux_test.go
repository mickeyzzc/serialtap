//go:build linux

package collector

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/logstore"
	"github.com/mickeyzzc/serialtap/internal/pause"
	"github.com/mickeyzzc/serialtap/internal/signature"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

func TestLineAssemblerSplitsAcrossChunks(t *testing.T) {
	var a lineAssembler
	if lines := a.feed([]byte("rst:0xc (SW_C")); len(lines) != 0 {
		t.Fatalf("半行不应提前吐出: %v", lines)
	}
	lines := a.feed([]byte("PU_RESET)\nE (102) cam: boom\r\npartial tail"))
	want := []string{"rst:0xc (SW_CPU_RESET)", "E (102) cam: boom"}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("行拼装错误: %v want %v", lines, want)
	}
	if got := a.flush(); got != "partial tail" {
		t.Fatalf("flush 残余错误: %q", got)
	}
}

func TestLineAssemblerFloodProtection(t *testing.T) {
	var a lineAssembler
	big := strings.Repeat("x", maxTail+100)
	lines := a.feed([]byte(big))
	if len(lines) != 1 || len(lines[0]) != len(big) {
		t.Fatalf("泥石流保护失败: %d 行", len(lines))
	}
	if len(a.tail) != 0 {
		t.Fatal("缓冲未清空")
	}
}

// runCollector: 启动采集器并注册"等待退出"清理（防泄漏协程与后续测试
// 写 OpenPort 全局竞态 —— CI race 实锤过一次）。
func runCollector(t *testing.T, c *Collector) *sync.WaitGroup {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Run()
	}()
	t.Cleanup(func() {
		c.Stop()
		wg.Wait()
	})
	return &wg
}

func withFakePort(t *testing.T, fp *testutil.FakePort) {
	t.Helper()
	old := OpenPort
	OpenPort = func(tty string, baud int) (Port, error) { return fp, nil }
	t.Cleanup(func() { OpenPort = old })
}

func TestEndToEndWithFakePort(t *testing.T) {
	root := t.TempDir()
	fp := &testutil.FakePort{Chunks: [][]byte{
		[]byte("I (100) boot: hello\r\nE (200) cam: boom\n"),
		[]byte("partial-no-newline"),
	}}
	withFakePort(t, fp)

	cfg := config.DefaultConfig()
	cfg.Root = root
	w, _ := logstore.NewDeviceWriter(root, "fakedev", 64)
	c := NewCollector(device.DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "fakedev"}, cfg, w,
		signature.New(nil), pause.NewPauseState(), nil)
	wg := runCollector(t, c)

	day := time.Now().Format("20060102")
	serialPath := filepath.Join(root, "fakedev", "serial-"+day+".log")
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(testutil.ReadFile(t, serialPath), "boom")
	}, "数据未落盘")

	c.Stop()
	wg.Wait()

	ser := testutil.ReadFile(t, serialPath)
	if !strings.Contains(ser, "boot: hello") || !strings.Contains(ser, "…partial partial-no-newline") {
		t.Fatalf("全量/半行落盘错误:\n%s", ser)
	}
	ev := testutil.ReadFile(t, filepath.Join(root, "fakedev", "events-"+day+".log"))
	for _, want := range []string{"collector started", "serial opened", "[esp-log-error]", "collector stopped"} {
		if !strings.Contains(ev, want) {
			t.Fatalf("事件流缺 %q:\n%s", want, ev)
		}
	}
	if fp.DTR || fp.RTS {
		t.Fatal("open 后 DTR/RTS 应已释放（CH340 RTS 接 EN，按住=复位态）")
	}
}

func TestSilentWatchdog(t *testing.T) {
	t.Run("disabled_never_reopens", func(t *testing.T) {
		root := t.TempDir()
		withFakePort(t, &testutil.FakePort{}) // 永远 0 字节静默
		cfg := config.DefaultConfig()
		cfg.Root = root
		w, _ := logstore.NewDeviceWriter(root, "quiet", 64)
		c := NewCollector(device.DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "quiet"}, cfg, w,
			signature.New(nil), pause.NewPauseState(), nil)
		wg := runCollector(t, c)
		time.Sleep(1500 * time.Millisecond)
		c.Stop()
		wg.Wait()
		day := time.Now().Format("20060102")
		ev := testutil.ReadFile(t, filepath.Join(root, "quiet", "events-"+day+".log"))
		if strings.Contains(ev, "forcing reopen") {
			t.Fatalf("看门狗关闭时不应触发重开:\n%s", ev)
		}
	})
	t.Run("enabled_reopens_on_silence", func(t *testing.T) {
		root := t.TempDir()
		withFakePort(t, &testutil.FakePort{Chunks: [][]byte{[]byte("first line\n")}})
		cfg := config.DefaultConfig()
		cfg.Root = root
		cfg.SilentReopenS = 1
		w, _ := logstore.NewDeviceWriter(root, "chatty", 64)
		c := NewCollector(device.DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "chatty"}, cfg, w,
			signature.New(nil), pause.NewPauseState(), nil)
		wg := runCollector(t, c)
		day := time.Now().Format("20060102")
		testutil.WaitFor(t, 5*time.Second, func() bool {
			return strings.Contains(testutil.ReadFile(t, filepath.Join(root, "chatty", "events-"+day+".log")), "forcing reopen")
		}, "静默看门狗未触发")
		c.Stop()
		wg.Wait()
	})
}

func TestReadErrorReopens(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	opens := 0
	old := OpenPort
	OpenPort = func(tty string, baud int) (Port, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		if opens == 1 {
			return &testutil.ErrPort{}, nil // 第一次：读即报错
		}
		return &testutil.FakePort{Chunks: [][]byte{[]byte("after reopen\n")}}, nil
	}
	t.Cleanup(func() { OpenPort = old })

	cfg := config.DefaultConfig()
	cfg.Root = root
	cfg.ReopenMinS = 1
	cfg.ReopenMaxS = 1
	w, _ := logstore.NewDeviceWriter(root, "errdev", 64)
	c := NewCollector(device.DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "errdev"}, cfg, w,
		signature.New(nil), pause.NewPauseState(), nil)
	wg := runCollector(t, c)
	day := time.Now().Format("20060102")
	testutil.WaitFor(t, 5*time.Second, func() bool {
		return strings.Contains(testutil.ReadFile(t, filepath.Join(root, "errdev", "serial-"+day+".log")), "after reopen")
	}, "重开后未收到数据")
	ev := testutil.ReadFile(t, filepath.Join(root, "errdev", "events-"+day+".log"))
	if !strings.Contains(ev, "read error") || !strings.Contains(ev, "port lost") {
		t.Fatalf("读错误路径事件缺失:\n%s", ev)
	}
	c.Stop()
	wg.Wait()
}

func TestPauseResume(t *testing.T) {
	root := t.TempDir()
	withFakePort(t, &testutil.FakePort{Chunks: [][]byte{[]byte("line one\n")}})
	cfg := config.DefaultConfig()
	cfg.Root = root
	w, _ := logstore.NewDeviceWriter(root, "pdev", 64)
	c := NewCollector(device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "k", Name: "pdev"}, cfg, w,
		signature.New(nil), pause.NewPauseState(), nil)
	wg := runCollector(t, c)
	day := time.Now().Format("20060102")
	evPath := filepath.Join(root, "pdev", "events-"+day+".log")
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(testutil.ReadFile(t, evPath), "serial opened")
	}, "未开端口")

	// 写入暂停清单并热替换（daemon tick 的等价操作）
	if err := os.WriteFile(pause.PauseFilePath(root), []byte("ttyFAKE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _, err := pause.LoadPauseFile(root)
	if err != nil {
		t.Fatal(err)
	}
	c.pauseState().ReplaceWith(st)
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(testutil.ReadFile(t, evPath), "paused")
	}, "未响应暂停")

	// 解除暂停 → 重开
	os.Remove(pause.PauseFilePath(root))
	c.pauseState().Clear()
	testutil.WaitFor(t, 5*time.Second, func() bool {
		s := testutil.ReadFile(t, evPath)
		return strings.Contains(s, "hold cleared") && strings.Count(s, "serial opened") >= 2
	}, "解除暂停后未重开")
	c.Stop()
	wg.Wait()
}

func TestOpenFailureLogsError(t *testing.T) {
	root := t.TempDir()
	old := OpenPort
	OpenPort = func(tty string, baud int) (Port, error) {
		return nil, os.NewSyscallError("open", syscall.EBUSY)
	}
	t.Cleanup(func() { OpenPort = old })

	cfg := config.DefaultConfig()
	cfg.Root = root
	cfg.ReopenMinS = 1
	cfg.ReopenMaxS = 1
	w, _ := logstore.NewDeviceWriter(root, "busydev", 64)
	c := NewCollector(device.DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "busydev"}, cfg, w,
		signature.New(nil), pause.NewPauseState(), nil)
	wg := runCollector(t, c)
	day := time.Now().Format("20060102")
	testutil.WaitFor(t, 5*time.Second, func() bool {
		ev := testutil.ReadFile(t, filepath.Join(root, "busydev", "events-"+day+".log"))
		return strings.Contains(ev, "open failed:") && strings.Contains(ev, "busy")
	}, "open 错误详情未进事件流")
	c.Stop()
	wg.Wait()
}

// —— Suspend/Resume 直控：让出端口要等真关闭，恢复后回采 ——
func TestSuspendResumeDirectControl(t *testing.T) {
	root := t.TempDir()
	withFakePort(t, &testutil.FakePort{Chunks: [][]byte{[]byte("before\n")}})
	cfg := config.DefaultConfig()
	cfg.Root = root
	w, _ := logstore.NewDeviceWriter(root, "sdev", 64)
	c := NewCollector(device.DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "sdev"}, cfg, w,
		signature.New(nil), pause.NewPauseState(), nil)
	wg := runCollector(t, c)
	day := time.Now().Format("20060102")
	evPath := filepath.Join(root, "sdev", "events-"+day+".log")
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(testutil.ReadFile(t, evPath), "serial opened")
	}, "未开端口")

	if !c.Suspend(3 * time.Second) {
		t.Fatal("Suspend 未在超时内确认端口关闭")
	}
	if c.State() != "suspended" {
		t.Fatalf("状态错误: %s", c.State())
	}
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(testutil.ReadFile(t, evPath), "port released")
	}, "让出未记事件")

	// 恢复 → 重开
	c.Resume()
	testutil.WaitFor(t, 5*time.Second, func() bool {
		return strings.Count(testutil.ReadFile(t, evPath), "serial opened") >= 2
	}, "恢复后未重开")
	if c.State() != "collecting" {
		t.Fatalf("恢复后状态错误: %s", c.State())
	}
	c.Stop()
	wg.Wait()
}

// Suspend 超时（端口持续开着，采集器无响应时）必须返回 false 而非死等
func TestSuspendTimeout(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Root = root
	w, _ := logstore.NewDeviceWriter(root, "tdev", 64)
	c := NewCollector(device.DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "tdev"}, cfg, w,
		signature.New(nil), pause.NewPauseState(), nil)
	// 不启动 Run —— Suspend 端口从未开过 → portOpen=false 立即确认……
	// 改测：手动把 portOpen 置真模拟"卡住"，Suspend 应超时
	c.sr.portOpen.Store(true)
	if c.Suspend(100 * time.Millisecond) {
		t.Fatal("端口未关闭时 Suspend 应超时返回 false")
	}
	c.sr.portOpen.Store(false)
}

// —— issue #4：成功会话后退避必须复位，不得棘轮到上限 ——
func TestBackoffResetsAfterSuccessfulSession(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	opens := 0
	old := OpenPort
	OpenPort = func(tty string, baud int) (Port, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		switch {
		case opens <= 2: // 前两次 open 失败 → backoff 1s→2s
			return nil, os.ErrPermission
		case opens == 3: // 第三次成功会话（读到数据后 read 错）
			return &errReadOncePort{chunks: [][]byte{[]byte("ok\n")}}, nil
		default:
			return nil, os.ErrPermission // 再次失败
		}
	}
	t.Cleanup(func() { OpenPort = old })

	cfg := config.DefaultConfig()
	cfg.Root = root
	cfg.ReopenMinS = 1
	cfg.ReopenMaxS = 8
	w, _ := logstore.NewDeviceWriter(root, "bdev", 64)
	c := NewCollector(device.DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "bdev"}, cfg, w,
		signature.New(nil), pause.NewPauseState(), nil)
	wg := runCollector(t, c)

	// 等到第 4 次 open 失败（退避已按复位后的值记账）
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := opens
		mu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.Stop()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if opens < 4 {
		t.Fatalf("序列未走完: opens=%d", opens)
	}
	// 时序：open3 成功会话复位 backoff=1s；port-lost 重连睡 1s（=min ✓ 复位生效）
	// 后翻倍到 2s；open4 失败是新连击的第二次失败 → 2s（而非旧棘轮 4s/8s）
	if got := c.curBackoff; got != 2*time.Second {
		t.Fatalf("退避复位语义错误: %s (want 2s = min 翻倍一次，非棘轮值)", got)
	}
}

// errReadOncePort: 吐一块数据后读错误（制造"成功会话"）
type errReadOncePort struct {
	chunks [][]byte
}

func (p *errReadOncePort) Read(b []byte) (int, error) {
	if len(p.chunks) > 0 {
		c := p.chunks[0]
		p.chunks = p.chunks[1:]
		return copy(b, c), nil
	}
	return 0, os.ErrClosed
}
func (p *errReadOncePort) Close() error                         { return nil }
func (p *errReadOncePort) SetDTR(bool) error                    { return nil }
func (p *errReadOncePort) SetRTS(bool) error                    { return nil }
func (p *errReadOncePort) SetReadTimeout(d time.Duration) error { return nil }
func (p *errReadOncePort) Write(b []byte) (int, error) { return len(b), nil } // Port 接口新增 Write（代理透传）
