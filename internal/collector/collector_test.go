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

func TestLineAssemblerSplitsBeforeFloodDump(t *testing.T) {
	var a lineAssembler
	payload := "rst:0x1 (POWERON_RESET)\n" + strings.Repeat("x", maxTail+50)
	lines := a.feed([]byte(payload))
	if len(lines) != 2 {
		t.Fatalf("want banner + flood dump, got %d lines", len(lines))
	}
	if lines[0] != "rst:0x1 (POWERON_RESET)" {
		t.Fatalf("banner lost in flood path: %q", lines[0])
	}
	if strings.Contains(lines[0], "\n") || strings.Contains(lines[1], "\n") {
		t.Fatalf("flood dump smuggled a newline: %#v", lines)
	}
	if len(lines[1]) != maxTail+50 {
		t.Fatalf("flood remainder size %d", len(lines[1]))
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
		return strings.Contains(s, "unpaused") && strings.Count(s, "serial opened") >= 2
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
