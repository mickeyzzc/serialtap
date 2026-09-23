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
	"testing"
	"time"

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
	resp, err := http.Post(url, mw.FormDataContentType(), &buf)
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
	resp, err := http.Post(ts.URL+"/api/flash", mw.FormDataContentType(), &buf)
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
