package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
)

// newTestServer: 临时日志根 + 假状态源 + 记录型 commander。
func newTestServer(t *testing.T) (*Server, *httptest.Server, *[]ctl.Request) {
	t.Helper()
	root := t.TempDir()
	devDir := filepath.Join(root, "esp32dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	serial := "[2026-09-21 10:00:00.000] boot line1\n[2026-09-21 10:00:01.000] line2\n"
	if err := os.WriteFile(filepath.Join(devDir, "serial-20260921.log"), []byte(serial), 0o644); err != nil {
		t.Fatal(err)
	}
	events := "[2026-09-21 10:00:00.000] EVT rst:0x15 reset-banner\n"
	if err := os.WriteFile(filepath.Join(devDir, "events-20260921.log"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []ctl.Request
	s := &Server{
		root: root,
		status: func() []ctl.DevState {
			return []ctl.DevState{{Name: "esp32dev", Tty: "COM3", Key: "k1", State: "collecting"}}
		},
		commander: func(req ctl.Request) (ctl.Response, error) {
			mu.Lock()
			got = append(got, req)
			mu.Unlock()
			if req.Cmd == "pause" {
				return ctl.Response{OK: true}, nil
			}
			return ctl.Response{OK: false, Error: "boom"}, nil
		},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			s.handleIndex(w, r)
		case r.URL.Path == "/api/status":
			s.handleStatus(w, r)
		case r.URL.Path == "/api/events":
			s.handleEvents(w, r)
		case r.URL.Path == "/api/cmd":
			s.handleCmd(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/devices/"):
			s.handleDevice(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return s, ts, &got
}

func TestStatusEnrich(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var dto snapshotDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if len(dto.Devices) != 1 || dto.Devices[0].Name != "esp32dev" {
		t.Fatalf("设备载荷: %+v", dto.Devices)
	}
	d := dto.Devices[0]
	if d.SerialFile != "serial-20260921.log" || d.SerialSize != int64(len("[2026-09-21 10:00:00.000] boot line1\n[2026-09-21 10:00:01.000] line2\n")) {
		t.Errorf("serial 富化: %+v", d)
	}
	if d.EventsFile != "events-20260921.log" {
		t.Errorf("events 富化: %+v", d)
	}
}

func TestTailLineAlign(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/api/devices/esp32dev/tail?kind=serial&bytes=40")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// bytes=40 起点落在首行中间 —— 对齐到行首丢掉残行后应恰好只剩第二行
	// （窗口更小时残行丢完返回空也是合法行为，不在此断言）
	if !strings.HasPrefix(string(b), "[2026-09-21 10:00:01.000] line2") {
		t.Errorf("tail 未对齐行首: %q", string(b))
	}
}

func TestEventsMerge(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	var out []eventsDTO
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Device != "esp32dev" || len(out[0].Lines) == 0 {
		t.Fatalf("事件载荷: %+v", out)
	}
	if !strings.Contains(strings.Join(out[0].Lines, "\n"), "rst:0x15") {
		t.Errorf("事件内容缺失: %+v", out[0])
	}
}

func TestCmdPassthrough(t *testing.T) {
	_, ts, got := newTestServer(t)
	body := strings.NewReader(`{"cmd":"pause","pattern":"^esp32dev$"}`)
	resp, err := ts.Client().Post(ts.URL+"/api/cmd", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	var r ctl.Response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Errorf("pause 应成功: %+v", r)
	}
	if len(*got) != 1 || (*got)[0].Pattern != "^esp32dev$" {
		t.Errorf("commander 收到: %+v", *got)
	}
	// flash 必须拒绝（面板不允许流式/长操作）
	resp2, err := ts.Client().Post(ts.URL+"/api/cmd", "application/json",
		strings.NewReader(`{"cmd":"flash"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("flash 应被拒绝，状态码 %d", resp2.StatusCode)
	}
}

func TestPathSafety(t *testing.T) {
	_, ts, _ := newTestServer(t)
	for _, bad := range []string{"..%2F..%2Fetc", "a b", "x/y"} {
		resp, err := ts.Client().Get(ts.URL + "/api/devices/" + bad + "/files")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("恶意路径 %s 未被拒绝: %d", bad, resp.StatusCode)
		}
	}
}

func TestLiveStream(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, "esp32dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(devDir, "serial-20260921.log")
	if err := os.WriteFile(logPath, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{root: root}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleDevice(w, r)
	}))
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/devices/esp32dev/live?kind=serial")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 消费 goroutine + channel：SSE 长连接不能阻塞读（io.ReadCloser 无 deadline 接口）
	// 跳过 ": ping" 心跳注释帧，只统计 data 帧
	type chunk struct {
		data []byte
		err  error
	}
	ch := make(chan chunk, 8)
	go func() {
		buf := make([]byte, 4096)
		var pending []byte
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				for {
					i := indexByte(pending, '\n')
					if i < 0 {
						break
					}
					line := string(pending[:i])
					pending = pending[i+1:]
					if len(line) > 0 && line[0] == ':' {
						continue // 心跳
					}
					if strings.HasPrefix(line, "data:") {
						b := make([]byte, len(line)+1)
						copy(b, line+"\n")
						ch <- chunk{b, nil}
					}
				}
			}
			if err != nil {
				ch <- chunk{nil, err}
				return
			}
		}
	}()

	// 初始尾随
	select {
	case c := <-ch:
		if !strings.Contains(string(c.data), "first") {
			t.Fatalf("初始尾随缺失: %q", string(c.data))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待初始 SSE 事件超时")
	}

	// 追加数据 → 应在 ~1s 内以 SSE 事件到达
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("appended-live\n")
	_ = f.Close()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-ch:
			if strings.Contains(string(c.data), "appended-live") {
				return // 通过
			}
			if c.err != nil {
				t.Fatalf("SSE 流错误: %v", c.err)
			}
		case <-deadline:
			t.Fatal("追加内容未在 SSE 流中出现")
		}
	}
}

func TestPickAddr(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "127.0.0.1:8801",
		"off":              "",
		"127.0.0.1:9000":   "127.0.0.1:9000",
		"bad addr":         "127.0.0.1:8801",
	} {
		if got := PickAddr(in); got != want {
			t.Errorf("PickAddr(%q)=%q want %q", in, got, want)
		}
	}
}

func TestLatestFileRotation(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"serial-20260920.log", "serial-20260921.log", "serial-20260921.001.log"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	name, _, ok := latestFile(dir, "serial")
	if !ok || name != "serial-20260921.001.log" {
		t.Errorf("轮转后最新文件判定: %q ok=%v", name, ok)
	}
	if _, _, ok := latestFile(dir, "events"); ok {
		t.Error("不存在的通道不应命中")
	}
}

func TestIndexServed(t *testing.T) {
	_, ts, _ := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index 状态码 %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("Content-Type: %s", resp.Header.Get("Content-Type"))
	}
}
