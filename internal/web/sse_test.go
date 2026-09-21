package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 回归：serveLive 在初始尾随之后必须持续增量推送（发现于真机面板：
// 2 个 data 事件后流停滞）。写入 → sleep >700ms → 写入，断言 >=3 个事件。
func TestLiveStreamIncremental(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, "dev")
	os.MkdirAll(devDir, 0o755)
	logPath := filepath.Join(devDir, "serial-20260921.log")
	os.WriteFile(logPath, []byte("init-line\n"), 0o644)

	s := &Server{root: root}
	ts := httptest.NewServer(http.HandlerFunc(s.handleDevice))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/devices/dev/live?kind=serial", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	type chunk struct {
		n   int
		err error
	}
	ch := make(chan chunk, 16)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				ch <- chunk{n, nil}
			}
			if err != nil {
				ch <- chunk{0, err}
				return
			}
		}
	}()

	readFor := func(d time.Duration) int {
		var total int
		deadline := time.After(d)
		for {
			select {
			case c := <-ch:
				if c.err != nil {
					return total
				}
				total += c.n
			case <-deadline:
				return total
			}
		}
	}

	// 初始尾随
	init := readFor(2 * time.Second)
	if init == 0 {
		t.Fatal("初始尾随缺失")
	}

	// 追加一行，等 700ms tick 周期的若干倍
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("grow-1\n")
	f.Close()
	n1 := readFor(2 * time.Second)

	// 再追加一行
	f, _ = os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("grow-2\n")
	f.Close()
	n2 := readFor(2 * time.Second)

	cancel()
	if n1 == 0 {
		t.Fatalf("第一次追加未推送（init=%d bytes 后流停滞）", init)
	}
	if n2 == 0 {
		t.Fatalf("第二次追加未推送（第一次 n1=%d 后流停滞）", n1)
	}
}

// 高频追加（贴近真实串口日志节奏）：持续写小行，SSE 应持续有增量，
// 而不是初始尾随 + 一次增量后停滞（真机面板实测病象）。
func TestLiveStreamHighFrequencyAppends(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, "dev")
	os.MkdirAll(devDir, 0o755)
	logPath := filepath.Join(devDir, "serial-20260921.log")
	os.WriteFile(logPath, []byte("init-line-that-is-longish\n"), 0o644)

	s := &Server{root: root}
	ts := httptest.NewServer(http.HandlerFunc(s.handleDevice))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/devices/dev/live?kind=serial", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	defer cancel()
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	type chunk struct {
		n   int
		err error
	}
	ch := make(chan chunk, 256)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				ch <- chunk{n, nil}
			}
			if err != nil {
				ch <- chunk{0, err}
				return
			}
		}
	}()
	readFor := func(d time.Duration) int {
		var total int
		deadline := time.After(d)
		for {
			select {
			case c := <-ch:
				if c.err != nil {
					return total
				}
				total += c.n
			case <-deadline:
				return total
			}
		}
	}

	stop := make(chan struct{})
	var written int64
	var wmu sync.Mutex
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				continue
			}
			n, _ := f.WriteString("I (123456) app: periodic log line for stream test\n")
			f.Close()
			wmu.Lock()
			written += int64(n)
			wmu.Unlock()
			time.Sleep(300 * time.Millisecond)
		}
	}()
	defer close(stop)

	// 吃掉初始尾随
	init := readFor(2 * time.Second)
	// 之后 5 秒：写者每 300ms 一行，SSE 每 700ms 一个增量 → 期望多个事件
	body := readFor(5 * time.Second)
	wmu.Lock()
	total := written
	wmu.Unlock()
	if body < 200 {
		t.Fatalf("高频追加下 SSE 停滞：init=%d bytes，其后 5s 仅 %d bytes（写入者共写 %d bytes）", init, body, total)
	}
}
