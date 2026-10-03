package web

import (
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/flash"
)

// fakeMesh: 记录型 MeshService。
type fakeMesh struct {
	mu      sync.Mutex
	tailReq []string // {peer, device, kind}
	pump    func(onData func([]byte))
}

func (f *fakeMesh) AggregateStatus(timeout time.Duration) []ctl.PeerStatus {
	return []ctl.PeerStatus{
		{ID: "1a2b", Name: "bench-a", Addr: "192.168.63.10:8802", State: "online", LatencyMs: 3,
			Devices: []ctl.DevState{{Name: "n16r8-u1", Tty: "/dev/ttyUSB0", Key: "usb-1a86", State: "collecting"}}},
	}
}

func (f *fakeMesh) TailStream(ctx context.Context, peer, device, kind string, onData func([]byte)) error {
	f.mu.Lock()
	f.tailReq = append(f.tailReq, peer+"|"+device+"|"+kind)
	pump := f.pump
	f.mu.Unlock()
	if pump != nil {
		pump(onData)
	}
	<-ctx.Done() // 尾随是无限流：桥接端点 ctx 取消才返回
	return nil
}

func newMeshTestServer(t *testing.T, m MeshService, mf MeshFlasher) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{root: t.TempDir()}
	if m != nil {
		s.mesh = m
	}
	if mf != nil {
		s.meshFlasher = mf
		s.flasher = func(string, bool, flash.Spec, func(string)) error {
			t.Fatal("本地 flasher 不应被调用")
			return nil
		}
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/mesh/status":
			s.handleMeshStatus(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/mesh/devices/"):
			s.handleMeshDevice(w, r)
		case r.URL.Path == "/api/flash":
			s.handleFlash(w, r)
		case r.URL.Path == "/api/flash/stream":
			s.flashStream(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return s, ts
}

func TestMeshStatusDisabled(t *testing.T) {
	_, ts := newMeshTestServer(t, nil, nil)
	resp, err := ts.Client().Get(ts.URL + "/api/mesh/status")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["enabled"] != false {
		t.Fatalf("未启用应 enabled=false: %v", m)
	}
}

func TestMeshStatusPeers(t *testing.T) {
	_, ts := newMeshTestServer(t, &fakeMesh{}, nil)
	resp, err := ts.Client().Get(ts.URL + "/api/mesh/status")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Enabled bool             `json:"enabled"`
		Peers   []ctl.PeerStatus `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if !m.Enabled || len(m.Peers) != 1 || m.Peers[0].Devices[0].Name != "n16r8-u1" {
		t.Fatalf("聚合内容错误: %+v", m)
	}
}

func TestMeshDeviceLiveSSE(t *testing.T) {
	fm := &fakeMesh{pump: func(onData func([]byte)) {
		onData([]byte("remote line1\n"))
	}}
	_, ts := newMeshTestServer(t, fm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/mesh/devices/bench-a/n16r8-u1/live?kind=serial", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("Content-Type 错误: %s body=%s", resp.Header.Get("Content-Type"), b)
	}
	buf := make([]byte, 4096)
	_ = resp.Body.(io.Reader)
	n, _ := resp.Body.Read(buf) // 首帧（SSE 头 + ping + 数据）
	head := string(buf[:n])
	if !strings.Contains(head, ": ping") {
		t.Fatalf("缺 SSE 心跳帧: %q", head)
	}
	// 读到 data 帧（可能要再读几次；连接无 deadline，靠循环上限兜底）
	deadline := time.Now().Add(3 * time.Second)
	all := head
	for !strings.Contains(all, "remote line1") && time.Now().Before(deadline) {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			all += string(buf[:n])
		}
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(all, `"remote line1\n"`) {
		t.Fatalf("远程尾随数据未到: %q", all)
	}
	cancel()
	fm.mu.Lock()
	got := fm.tailReq
	fm.mu.Unlock()
	if len(got) != 1 || got[0] != "bench-a|n16r8-u1|serial" {
		t.Fatalf("TailStream 参数错误: %v", got)
	}
}

func TestFlashRoutesToMeshFlasher(t *testing.T) {
	var gotPeer, gotPattern string
	var gotSpec flash.Spec
	called := make(chan struct{})
	mf := func(peer, pattern string, all bool, spec flash.Spec, out func(string)) error {
		gotPeer, gotPattern, gotSpec = peer, pattern, spec
		out("remote esptool line")
		close(called) // 写在 close 前、读在 receive 后：happens-before 成立
		return nil
	}
	s, ts := newMeshTestServer(t, nil, mf)

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		_ = mw.WriteField("pattern", "^dev$")
		_ = mw.WriteField("peer", "bench-a")
		f, _ := mw.CreateFormFile("bins", "app.bin")
		_, _ = f.Write([]byte("IMAGE"))
		_ = mw.Close()
		_ = pw.Close()
	}()
	resp, err := ts.Client().Post(ts.URL+"/api/flash", mw.FormDataContentType(), pr)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("flash 状态码 %d", resp.StatusCode)
	}
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("meshFlasher 未被调用")
	}
	if gotPeer != "bench-a" || gotPattern != "^dev$" {
		t.Fatalf("meshFlasher 参数错误: peer=%q pattern=%q", gotPeer, gotPattern)
	}
	if len(gotSpec.Bins) != 1 {
		t.Fatalf("spec.Bins 错误: %+v", gotSpec)
	}
	// 镜像先落本机（meshFlasher 从这里读并加密上传）
	b, err := os.ReadFile(gotSpec.Bins[0].Path)
	if err != nil || string(b) != "IMAGE" {
		t.Fatalf("本机落盘镜像错误: %q (%v)", b, err)
	}
	if !strings.Contains(gotSpec.Bins[0].Path, filepath.Join(s.root, ".flash-upload")) {
		t.Fatalf("镜像未落在面板根 .flash-upload: %s", gotSpec.Bins[0].Path)
	}
}

func TestFlashPeerWithoutMeshFails(t *testing.T) {
	_, ts := newMeshTestServer(t, nil, nil) // 无 meshFlasher
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		_ = mw.WriteField("pattern", "^dev$")
		_ = mw.WriteField("peer", "bench-a")
		_ = mw.Close()
		_ = pw.Close()
	}()
	resp, err := ts.Client().Post(ts.URL+"/api/flash", mw.FormDataContentType(), pr)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("无 mesh 时远程刷机应 503: %d", resp.StatusCode)
	}
}
