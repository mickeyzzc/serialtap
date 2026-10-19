package web

// 面板刷机端点：上传（multipart bins/args_file + offsets）→ 落盘 →
// 假 Flasher 执行 → SSE 历史回放。daemon 侧编排（让口→esptool→回采）
// 由 daemon 包测试覆盖，这里只验面板侧契约。

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/board"
	"github.com/mickeyzzc/serialtap/internal/flash"
)

// newFlashServer: 假 flasher（记录调用参数，回放固定输出后成功返回）。
func newFlashServer(t *testing.T, emit func(out func(string))) (*Server, *httptest.Server, *specsLog) {
	t.Helper()
	log := &specsLog{}
	s := &Server{root: t.TempDir()}
	s.flasher = func(pattern string, all bool, spec flash.Spec, out func(string)) error {
		log.mu.Lock()
		log.items = append(log.items, spec)
		log.mu.Unlock()
		emit(out)
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/flash", s.handleFlash)
	mux.HandleFunc("/api/flash/stream", s.flashStream)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, ts, log
}

// specsLog: 假 flasher 的调用记录（读写在 -race 下都要持锁）。
type specsLog struct {
	mu    sync.Mutex
	items []flash.Spec
}

func (l *specsLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}

func (l *specsLog) first() flash.Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.items[0]
}

// postFlash: 带 X-serialtap: 1 的 multipart POST（服务端 /api/flash 的 CSRF
// 自定义头校验；测试直连 handler 也按生产形态带头）。
func postFlash(url string, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-serialtap", "1")
	return http.DefaultClient.Do(req)
}

func multipartFlash(t *testing.T, url string, pattern string, bins [][2]string,
	offsets string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("pattern", pattern)
	if offsets != "" {
		_ = mw.WriteField("offsets", offsets)
	}
	for _, b := range bins {
		fw, _ := mw.CreateFormFile("bins", b[0])
		fw.Write([]byte(b[1]))
	}
	_ = mw.Close()
	resp, err := postFlash(url, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestFlashUploadRunsAndStreamReplays(t *testing.T) {
	_, ts, log := newFlashServer(t, func(out func(string)) {
		out("esptool v4.9 fake")
		out("Wrote 100 bytes at 0x0")
	})

	resp := multipartFlash(t, ts.URL+"/api/flash", "^board$",
		[][2]string{{"boot.bin", "BOOT"}, {"app..bin", "APP"}}, `["0x0","0x10000"]`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("上传应 200: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// spec 传到了 flasher：两个 bin、偏移对齐、文件名保留（Base+白名单清洗）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && log.len() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if log.len() != 1 {
		t.Fatalf("flasher 应被调一次: %d", log.len())
	}
	sp := log.first()
	if len(sp.Bins) != 2 || sp.Bins[0].Offset != "0x0" || sp.Bins[1].Offset != "0x10000" {
		t.Fatalf("spec 偏移错误: %+v", sp.Bins)
	}
	if !strings.HasSuffix(sp.Bins[1].Path, "app..bin") {
		t.Fatalf("文件名异常: %s", sp.Bins[1].Path)
	}

	// SSE：假 flasher 毫秒级完成 → 一次 GET 即全量历史回放 + done 收尾
	cl := &http.Client{Timeout: 3 * time.Second}
	sse, err := cl.Get(ts.URL + "/api/flash/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Body.Close()
	b, _ := io.ReadAll(sse.Body)
	got := string(b)
	if !strings.Contains(got, "esptool v4.9 fake") || !strings.Contains(got, "Wrote 100 bytes") {
		t.Fatalf("SSE 未回放输出: %s", got)
	}
	if !strings.Contains(got, `"done":true`) || strings.Contains(got, `"err":"`) {
		t.Fatalf("SSE 应以成功 done 收尾: %s", got)
	}
}

func TestFlashUploadValidation(t *testing.T) {
	_, ts, _ := newFlashServer(t, func(out func(string)) {})
	// 缺 pattern
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()
	resp, err := postFlash(ts.URL+"/api/flash", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 pattern 应 400: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// 无文件
	resp2 := multipartFlash(t, ts.URL+"/api/flash", "^b$", nil, "")
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("无 bins/args 应 400: %d", resp2.StatusCode)
	}
	resp2.Body.Close()
	// offsets 数量不匹配
	resp3 := multipartFlash(t, ts.URL+"/api/flash", "^b$",
		[][2]string{{"a.bin", "A"}}, `["0x0","0x1"]`)
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("offsets 不匹配应 400: %d", resp3.StatusCode)
	}
	resp3.Body.Close()
}

// 回归（#72）：flashStream 的历史快照与订阅必须原子（同一临界区）。任务
// 进行中连上的进度流要既拿到已发事件、又收到后续事件直到 done——拆成两
// 段锁的话，恰在缝里收尾的任务 done 会丢，浏览器进度条永久等待。
func TestFlashStreamMidJobSeesDone(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	_, ts, _ := newFlashServer(t, func(out func(string)) {
		out("esptool mid-flight line")
		close(started)
		<-release // 阻住任务，等进度流连上并吃到历史行再放行
	})

	resp := multipartFlash(t, ts.URL+"/api/flash", "^b$",
		[][2]string{{"a.bin", "A"}}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("上传应 200: %d", resp.StatusCode)
	}
	resp.Body.Close()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("flasher 未启动")
	}

	cl := &http.Client{Timeout: 5 * time.Second}
	sse, err := cl.Get(ts.URL + "/api/flash/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sse.Body.Close() }()

	buf := make([]byte, 0, 16*1024)
	tmp := make([]byte, 4096)
	released := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := sse.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		got := string(buf)
		if !released && strings.Contains(got, "esptool mid-flight line") {
			close(release) // 历史行已到：放行任务收尾，done 应随后经订阅到达
			released = true
		}
		if released && strings.Contains(got, `"done":true`) {
			return // 快照（历史行）+ 订阅（done）两路都吃到
		}
	}
	t.Fatalf("进度流未收齐历史行与 done: %s", string(buf))
}

// 回归（#72）：任务槽 check+set 必须原子（handleFlash 此前分两次锁，检查
// 与占坑之间有缝）。新语义 = 占坑后整个上传/执行期都持槽：阻塞中的任务
// 期间第二个 POST 必 409，flasher 只被调一次。（旧缝隙是纯并发窗口，无法
// 确定性复现；本测试钉住单槽契约本身。）
func TestFlashConcurrentPostsSingleSlot(t *testing.T) {
	block := make(chan struct{})
	var calls int32
	s, ts, _ := newFlashServer(t, func(out func(string)) {
		atomic.AddInt32(&calls, 1)
		<-block // 占住任务槽直到放行
	})
	defer close(block)

	post := func() int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("pattern", "^b$")
		fw, _ := mw.CreateFormFile("bins", "a.bin")
		_, _ = fw.Write([]byte("A"))
		_ = mw.Close()
		resp, err := postFlash(ts.URL+"/api/flash", mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		return code
	}
	c1, c2 := post(), post()
	okCount, conflictCount := 0, 0
	for _, c := range []int{c1, c2} {
		switch c {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("并发 POST 应恰一 200 一 409: got %d / %d", c1, c2)
	}
	// 放行后等任务收尾（goroutine publish(done) 释放槽位）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.job.mu.Lock()
		active := s.job.active
		s.job.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("flasher 应只被调一次: %d", got)
	}
}

// 深度识别：POST /api/board → boarder 执行 → 行进任务总线 → done 收尾。
func TestHandleBoardRunsAndFinishes(t *testing.T) {
	var gotPattern string
	done := make(chan struct{})
	s := &Server{root: t.TempDir(),
		boarder: func(pattern string, all bool, spec board.Spec, out func(string)) error {
			gotPattern = pattern
			out("esptool v5.5.0 fake")
			out("MAC: 80:b5:4e:c2:be:5c")
			close(done)
			return nil
		}}
	s.job.subs = map[chan flashEvent]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/board", s.handleBoard)
	mux.HandleFunc("/api/flash/stream", s.flashStream)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/board", "application/json",
		strings.NewReader(`{"pattern":"^board$"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("board 应 200: %d", resp.StatusCode)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("boarder 未被调用")
	}
	if gotPattern != "^board$" {
		t.Fatalf("pattern 传递错误: %q", gotPattern)
	}

	// SSE：历史回放应含 esptool 输出行与 done
	cl := &http.Client{Timeout: 3 * time.Second}
	r2, err := cl.Get(ts.URL + "/api/flash/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	b, _ := io.ReadAll(r2.Body)
	if !strings.Contains(string(b), "80:b5:4e:c2:be:5c") {
		t.Fatalf("SSE 缺探测输出:\n%s", b)
	}
}

// boarder 未注入时拒绝；pattern 缺失 400。
func TestHandleBoardValidation(t *testing.T) {
	s := &Server{root: t.TempDir()}
	s.job.subs = map[chan flashEvent]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/board", s.handleBoard)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/api/board", "application/json", strings.NewReader(`{"pattern":"^x$"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("无 boarder 应 503: %d", resp.StatusCode)
	}
	resp2, _ := http.Post(ts.URL+"/api/board", "application/json", strings.NewReader(`{}`))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 pattern 应 400: %d", resp2.StatusCode)
	}
}
