package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// —— TDD 修复 1：静默看门狗默认必须关 ——
// 通用场景下"安静"的合法设备（如只在开机打一行日志的传感器）会被
// 默认开启的看门狗复位循环打死。默认 0=关，需要的人在配置里显式开。
func TestDefaultSilentWatchdogDisabled(t *testing.T) {
	if DefaultConfig().SilentReopenS != 0 {
		t.Fatalf("静默看门狗默认必须为 0（关）：家族配置显式设 120")
	}
}

// —— TDD 修复 2：analyze 首末时间必须按时间序而非文件序 ——
func TestAnalyzeChronologicalFirstLast(t *testing.T) {
	dir := t.TempDir()
	newer := filepath.Join(dir, "b.log")
	older := filepath.Join(dir, "a.log")
	os.WriteFile(newer, []byte("[2026-06-01 10:00:00.000] rst:0x2 (X)\n"), 0o644)
	os.WriteFile(older, []byte("[2026-01-01 08:00:00.000] rst:0x1 (Y)\n"), 0o644)

	var buf bytes.Buffer
	// 故意按"新文件在前"传入 —— 修复前 FIRST 会取到 06-01
	if err := analyzeLogs(&buf, []string{newer, older}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "2026-01-01 08:00:00.000") || !strings.Contains(out, "2026-06-01 10:00:00.000") {
		t.Fatalf("首末时间缺失: %s", out)
	}
	f := strings.Index(out, "2026-01-01")
	l := strings.Index(out, "2026-06-01")
	if f < 0 || l < 0 || f > l {
		t.Fatalf("FIRST 应早于 LAST（FIRST 列必须取最早时间）: %s", out)
	}
}

// —— 假串口：让采集器全链路可离线测试 ——
type fakePort struct {
	mu     sync.Mutex
	chunks [][]byte
	closed bool
	dtr    bool
	rts    bool
	onRead func() // 每次读后回调（注入错误/暂停等场景）
}

func (f *fakePort) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if len(f.chunks) > 0 {
		c := f.chunks[0]
		f.chunks = f.chunks[1:]
		n := copy(p, c)
		if f.onRead != nil {
			f.onRead()
		}
		return n, nil
	}
	if f.onRead != nil {
		f.onRead()
	}
	return 0, nil // 模拟读超时（0 字节无错误）
}

func (f *fakePort) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakePort) SetDTR(v bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dtr = v
	return nil
}

func (f *fakePort) SetRTS(v bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rts = v
	return nil
}

func (f *fakePort) SetReadTimeout(d time.Duration) error { return nil }

// withFakePort 把包级串口打开器换成假实现，测试结束恢复。
func withFakePort(t *testing.T, fp *fakePort) {
	t.Helper()
	old := portOpener
	portOpener = func(tty string, baud int) (serialPort, error) {
		return fp, nil
	}
	t.Cleanup(func() { portOpener = old })
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// —— 采集器全链路（假端口驱动）：数据落盘 + DTR/RTS 释放 + 优雅停止 ——
func TestCollectorEndToEndWithFakePort(t *testing.T) {
	root := t.TempDir()
	fp := &fakePort{chunks: [][]byte{
		[]byte("I (100) boot: hello\r\nE (200) cam: boom\n"),
		[]byte("partial-no-newline"),
	}}
	withFakePort(t, fp)

	cfg := DefaultConfig()
	cfg.Root = root
	w, err := NewDeviceWriter(root, "fakedev", 64)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "fakedev"}, cfg, w,
		NewSignatureEngine(nil), NewPauseState(), func(string, ...any) {})
	done := make(chan struct{})
	go func() { c.Run(); close(done) }()

	day := time.Now().Format("20060102")
	serialPath := filepath.Join(root, "fakedev", "serial-"+day+".log")
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readLogFile(t, serialPath), "boom")
	}, "数据未落盘")

	c.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未随 Stop 退出")
	}

	ser := readLogFile(t, serialPath)
	if !strings.Contains(ser, "boot: hello") || !strings.Contains(ser, "…partial partial-no-newline") {
		t.Fatalf("全量/半行落盘错误:\n%s", ser)
	}
	ev := readLogFile(t, filepath.Join(root, "fakedev", "events-"+day+".log"))
	for _, want := range []string{"collector started", "serial opened", "[esp-log-error]", "collector stopped"} {
		if !strings.Contains(ev, want) {
			t.Fatalf("事件流缺 %q:\n%s", want, ev)
		}
	}
	if fp.dtr || fp.rts {
		t.Fatal("open 后 DTR/RTS 应已释放（CH340 RTS 接 EN，按住=复位态）")
	}
}

// —— 静默看门狗：关闭(默认)时静默不重开；开启时静默超阈值强制重开 ——
func TestCollectorSilentWatchdog(t *testing.T) {
	t.Run("disabled_never_reopens", func(t *testing.T) {
		root := t.TempDir()
		fp := &fakePort{} // 永远 0 字节静默
		withFakePort(t, fp)
		cfg := DefaultConfig()
		cfg.Root = root
		w, _ := NewDeviceWriter(root, "quiet", 64)
		c := NewCollector(DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "quiet"}, cfg, w,
			NewSignatureEngine(nil), NewPauseState(), func(string, ...any) {})
		go c.Run()
		time.Sleep(1500 * time.Millisecond)
		c.Stop()
		day := time.Now().Format("20060102")
		ev := readLogFile(t, filepath.Join(root, "quiet", "events-"+day+".log"))
		if strings.Contains(ev, "forcing reopen") {
			t.Fatalf("看门狗关闭时不应触发重开:\n%s", ev)
		}
	})
	t.Run("enabled_reopens_on_silence", func(t *testing.T) {
		root := t.TempDir()
		fp := &fakePort{chunks: [][]byte{[]byte("first line\n")}}
		withFakePort(t, fp)
		cfg := DefaultConfig()
		cfg.Root = root
		cfg.SilentReopenS = 1
		w, _ := NewDeviceWriter(root, "chatty", 64)
		c := NewCollector(DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "chatty"}, cfg, w,
			NewSignatureEngine(nil), NewPauseState(), func(string, ...any) {})
		go c.Run()
		day := time.Now().Format("20060102")
		waitFor(t, 5*time.Second, func() bool {
			return strings.Contains(readLogFile(t, filepath.Join(root, "chatty", "events-"+day+".log")), "forcing reopen")
		}, "静默看门狗未触发")
		c.Stop()
	})
}

// —— 读错误立即放弃 fd（USB 重枚举场景）——
func TestCollectorReadErrorReopens(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	opens := 0
	portOpenerOld := portOpener
	portOpener = func(tty string, baud int) (serialPort, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		if opens == 1 {
			return &errPort{}, nil // 第一次：读即报错
		}
		return &fakePort{chunks: [][]byte{[]byte("after reopen\n")}}, nil
	}
	t.Cleanup(func() { portOpener = portOpenerOld })

	cfg := DefaultConfig()
	cfg.Root = root
	cfg.ReopenMinS = 1
	cfg.ReopenMaxS = 1
	w, _ := NewDeviceWriter(root, "errdev", 64)
	c := NewCollector(DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "errdev"}, cfg, w,
		NewSignatureEngine(nil), NewPauseState(), func(string, ...any) {})
	go c.Run()
	day := time.Now().Format("20060102")
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(readLogFile(t, filepath.Join(root, "errdev", "serial-"+day+".log")), "after reopen")
	}, "重开后未收到数据")
	ev := readLogFile(t, filepath.Join(root, "errdev", "events-"+day+".log"))
	if !strings.Contains(ev, "read error") || !strings.Contains(ev, "port lost") {
		t.Fatalf("读错误路径事件缺失:\n%s", ev)
	}
	c.Stop()
}

type errPort struct{}

func (e *errPort) Read([]byte) (int, error)             { return 0, os.ErrClosed }
func (e *errPort) Close() error                         { return nil }
func (e *errPort) SetDTR(bool) error                    { return nil }
func (e *errPort) SetRTS(bool) error                    { return nil }
func (e *errPort) SetReadTimeout(d time.Duration) error { return nil }

// —— 暂停语义：PAUSED 命中 → 关口等待；解除 → 重开 ——
func TestCollectorPauseResume(t *testing.T) {
	root := t.TempDir()
	fp := &fakePort{chunks: [][]byte{[]byte("line one\n")}}
	withFakePort(t, fp)
	cfg := DefaultConfig()
	cfg.Root = root
	w, _ := NewDeviceWriter(root, "pdev", 64)
	c := NewCollector(DeviceInfo{Tty: "/dev/ttyFAKE", Key: "k", Name: "pdev"}, cfg, w,
		NewSignatureEngine(nil), NewPauseState(), func(string, ...any) {})
	go c.Run()
	day := time.Now().Format("20060102")
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readLogFile(t, filepath.Join(root, "pdev", "events-"+day+".log")), "serial opened")
	}, "未开端口")

	// 写入暂停清单（匹配 tty 名），并模拟 daemon tick 的热重载
	if err := os.WriteFile(PauseFilePath(root), []byte("ttyFAKE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _, err := LoadPauseFile(root)
	if err != nil {
		t.Fatal(err)
	}
	c.pause.mu.Lock()
	c.pause.pats = st.pats
	c.pause.mu.Unlock()

	evPath := filepath.Join(root, "pdev", "events-"+day+".log")
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readLogFile(t, evPath), "paused")
	}, "未响应暂停")

	// 解除暂停 → 重开
	os.Remove(PauseFilePath(root))
	c.pause.mu.Lock()
	c.pause.pats = nil
	c.pause.mu.Unlock()
	waitFor(t, 5*time.Second, func() bool {
		s := readLogFile(t, evPath)
		return strings.Contains(s, "unpaused") && strings.Count(s, "serial opened") >= 2
	}, "解除暂停后未重开")
	c.Stop()
}

// —— open 失败：错误详情必须进事件流（EBUSY 排查靠它）——
func TestCollectorOpenFailureLogsError(t *testing.T) {
	root := t.TempDir()
	old := portOpener
	portOpener = func(tty string, baud int) (serialPort, error) {
		return nil, os.NewSyscallError("open", syscall.EBUSY)
	}
	t.Cleanup(func() { portOpener = old })

	cfg := DefaultConfig()
	cfg.Root = root
	cfg.ReopenMinS = 1
	cfg.ReopenMaxS = 1
	w, _ := NewDeviceWriter(root, "busydev", 64)
	c := NewCollector(DeviceInfo{Tty: "/dev/fake", Key: "k", Name: "busydev"}, cfg, w,
		NewSignatureEngine(nil), NewPauseState(), func(string, ...any) {})
	go c.Run()
	day := time.Now().Format("20060102")
	waitFor(t, 5*time.Second, func() bool {
		ev := readLogFile(t, filepath.Join(root, "busydev", "events-"+day+".log"))
		return strings.Contains(ev, "open failed:") && strings.Contains(ev, "busy")
	}, "open 错误详情未进事件流")
	c.Stop()
}
