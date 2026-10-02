package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/board"
	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/flash"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

// fakeEchoProxy: 假 ProxyAPI——ProxyStart 开一个回显 TCP 端点。
type fakeEchoProxy struct {
	ln net.Listener
}

func (f *fakeEchoProxy) ProxyStart(pattern string) (string, string, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", "", err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	f.ln = ln
	return ln.Addr().String(), "echo-dev", "usb-echo-key", nil
}

func (f *fakeEchoProxy) ProxyStop(pattern string) (int, error) {
	if f.ln != nil {
		_ = f.ln.Close()
	}
	return 1, nil
}

// nodePair: 双节点回环拓扑（NoUDP，B 以静态种子指向 A）。
// forwardA 是 A 的业务分发（fake handler）。
type nodePair struct {
	a, b     *Node
	rootA    string
	rootB    string
	forwardA ctl.Handler
}

func startNode(t *testing.T, name, root string, forward ctl.Handler, proxy ProxyAPI) *Node {
	t.Helper()
	n, err := NewNode(Options{
		Name:    name,
		Key:     "test-mesh-key",
		Root:    root,
		Forward: forward,
		Proxy:   proxy,
		NoUDP:   true,
		Logf:    func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("节点 %s 启动失败: %v", name, err)
	}
	t.Cleanup(n.Close)
	return n
}

func newNodePair(t *testing.T, forward ctl.Handler, proxy ProxyAPI) *nodePair {
	t.Helper()
	rootA := t.TempDir()
	rootB := t.TempDir()
	a := startNode(t, "bench-a", rootA, forward, proxy)
	b := startNode(t, "bench-b", rootB, nil, nil)
	addr := fmt.Sprintf("127.0.0.1:%d", a.self.Port)
	b.reg.SetStatic([]string{addr})
	// 静态种子按址先拨一次学身份（生产里 mesh status 聚合做同样的事）
	if _, err := b.Call(addr, ctl.Request{Cmd: "status"}, func(ctl.Response) bool { return false }); err != nil {
		t.Fatalf("静态种子引导失败: %v", err)
	}
	return &nodePair{a: a, b: b, rootA: rootA, rootB: rootB, forwardA: forward}
}

func TestNodePairStatusForward(t *testing.T) {
	forward := func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd != "status" {
			respond(ctl.Response{OK: false, Error: "unknown cmd"})
			return
		}
		respond(ctl.Response{OK: true, Devices: []ctl.DevState{
			{Name: "n16r8-u1", Tty: "/dev/ttyUSB0", Key: "usb-1a86", State: "collecting", Opens: 1},
		}})
	}
	p := newNodePair(t, forward, nil)

	var got []ctl.DevState
	_, err := p.b.Call("bench-a", ctl.Request{Cmd: "status"}, func(r ctl.Response) bool {
		if r.OK {
			got = r.Devices
		}
		return false
	})
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if len(got) != 1 || got[0].Name != "n16r8-u1" || got[0].Opens != 1 {
		t.Fatalf("设备状态转发错误: %+v", got)
	}
	// 静态种子身份已被学习（占位 → 真名）
	peers := p.b.Peers()
	if len(peers) != 1 || peers[0].Name != "bench-a" || !peers[0].Static {
		t.Fatalf("静态种子学习失败: %+v", peers)
	}

	// 聚合状态
	agg := p.b.AggregateStatus(3 * time.Second)
	if len(agg) != 1 || agg[0].State != "online" || len(agg[0].Devices) != 1 {
		t.Fatalf("聚合状态错误: %+v", agg)
	}
}

func TestNodeForwardStreamsMultipleResponses(t *testing.T) {
	forward := func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd != "flash" {
			respond(ctl.Response{OK: false, Error: "unknown cmd"})
			return
		}
		respond(ctl.Response{OK: true, Event: "flash-log", Line: "esptool v1"})
		respond(ctl.Response{OK: true, Event: "flash-log", Line: "Chip is ESP32-S3"})
		respond(ctl.Response{OK: true, Event: "flash-done"})
	}
	p := newNodePair(t, forward, nil)
	var events []string
	err := p.b.Forward("bench-a", ctl.Request{Cmd: "flash", Pattern: "^dev$"}, func(r ctl.Response) {
		events = append(events, r.Event+":"+r.Line)
	})
	if err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
	if len(events) != 3 || !strings.Contains(events[1], "ESP32-S3") || !strings.Contains(events[2], "flash-done") {
		t.Fatalf("流式响应次序/内容错误: %v", events)
	}
}

func TestNodeCallUnknownPeer(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true})
	}, nil)
	_, err := p.b.Call("no-such", ctl.Request{Cmd: "status"}, nil)
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("未知 peer 必须报错: %v", err)
	}
}

func TestRemoteFlashUploadsImages(t *testing.T) {
	// A 侧 fake handler：收 flash 时把收到的镜像首字节回显（证明文件已落 A 的盘）
	binPath := filepath.Join(t.TempDir(), "app.bin")
	payload := []byte("FIRMWARE-IMAGE-BYTES-0123456789")
	if err := os.WriteFile(binPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var seenPath string
	forward := func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd == "flash" && len(req.Spec.Bins) == 1 {
			seenPath = req.Spec.Bins[0].Path
			b, _ := os.ReadFile(seenPath)
			respond(ctl.Response{OK: true, Event: "flash-log", Line: fmt.Sprintf("size=%d head=%q", len(b), b[:8])})
			respond(ctl.Response{OK: true, Event: "flash-done"})
		} else {
			respond(ctl.Response{OK: false, Error: "unexpected"})
		}
	}
	p := newNodePair(t, forward, nil)
	err := p.b.remoteFlash("bench-a", ctl.Request{
		Cmd:     "flash",
		Pattern: "^dev$",
		Spec:    flashSpecFor(binPath),
	}, func(r ctl.Response) {
		if r.Event == "flash-log" {
			if !strings.Contains(r.Line, "head=\"FIRMWARE\"") {
				t.Errorf("A 侧收到的镜像内容错误: %s", r.Line)
			}
		}
	})
	if err != nil {
		t.Fatalf("remoteFlash 失败: %v", err)
	}
	if seenPath == "" || !strings.Contains(seenPath, ".flash-upload") || !strings.Contains(seenPath, "mesh-") {
		t.Fatalf("远端 spec 路径未指向上传落盘: %s", seenPath)
	}
	// 上传分块 >1MB 也走通（2.5MB → 3 块）
	big := make([]byte, 2<<20+1234)
	for i := range big {
		big[i] = byte(i % 251)
	}
	bigPath := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(bigPath, big, 0o600); err != nil {
		t.Fatal(err)
	}
	err = p.b.remoteFlash("bench-a", ctl.Request{Cmd: "flash", Pattern: "^dev$", Spec: flashSpecFor(bigPath)},
		func(ctl.Response) {})
	if err != nil {
		t.Fatalf("大镜像 remoteFlash 失败: %v", err)
	}
	if seenPath == "" {
		t.Fatal("第二次刷写未见路径")
	}
	got, err := os.ReadFile(seenPath)
	if err != nil || len(got) != len(big) {
		t.Fatalf("A 侧大镜像落盘错误: %d B (err=%v)", len(got), err)
	}
}

func flashSpecFor(paths ...string) (s flash.Spec) {
	for _, p := range paths {
		s.Bins = append(s.Bins, flash.BinSpec{Path: p, Offset: "0x10000"})
	}
	return s
}

func TestRemoteBoardDumpFetchesArtifact(t *testing.T) {
	// A 侧 fake handler：board dump 到 OutPath 写产物（模拟 board.Exec）
	forward := func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd == "board" && req.Board != nil && req.Board.Action == "dump" {
			if req.Board.OutPath == "" {
				respond(ctl.Response{OK: false, Event: "board-done", Error: "OutPath 为空"})
				return
			}
			if err := os.MkdirAll(filepath.Dir(req.Board.OutPath), 0o700); err != nil {
				respond(ctl.Response{OK: false, Event: "board-done", Error: err.Error()})
				return
			}
			if err := os.WriteFile(req.Board.OutPath, []byte("NVS-REGION-DATA"), 0o600); err != nil {
				respond(ctl.Response{OK: false, Event: "board-done", Error: err.Error()})
				return
			}
			respond(ctl.Response{OK: true, Event: "board-log", Line: "dumped"})
			respond(ctl.Response{OK: true, Event: "board-done"})
			return
		}
		respond(ctl.Response{OK: false, Error: "unexpected"})
	}
	p := newNodePair(t, forward, nil)
	localOut := filepath.Join(t.TempDir(), "fetched", "dump.bin")
	err := p.b.Forward("bench-a", ctl.Request{
		Cmd:     "board",
		Pattern: "^dev$",
		Board:   &board.Spec{Action: "dump", Addr: "0x9000", Size: "0x6000", OutPath: localOut},
	}, func(ctl.Response) {})
	if err != nil {
		t.Fatalf("remoteBoardDump 失败: %v", err)
	}
	got := testutil.ReadFile(t, localOut)
	if string(got) != "NVS-REGION-DATA" {
		t.Fatalf("取回产物内容错误: %q", got)
	}
	// 一次性 token：再下载同 token 必须拿不到
	n, _ := p.b.download("bench-a", "forged-token", "dump.bin")
	if len(n) != 0 {
		t.Fatal("伪造 token 不应取到数据")
	}
}

func TestTailStreamFollowsLog(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true})
	}, nil)
	devDir := filepath.Join(p.rootA, "n16r8-u1")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(devDir, "serial-20260101.log")
	if err := os.WriteFile(logPath, []byte("line1\nline2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := make(chan []byte, 64)
	done := make(chan error, 1)
	go func() {
		done <- p.b.TailStream(ctx, "bench-a", "n16r8-u1", "serial", func(chunk []byte) {
			data <- append([]byte(nil), chunk...)
		})
	}()
	// 初始尾部
	var acc []byte
	deadline := time.Now().Add(3 * time.Second)
	for string(acc) != "line1\nline2\n" && time.Now().Before(deadline) {
		select {
		case c := <-data:
			acc = append(acc, c...)
		case <-time.After(500 * time.Millisecond):
		}
	}
	if string(acc) != "line1\nline2\n" {
		t.Fatalf("初始尾部错误: %q", acc)
	}
	// 增量跟随
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("line3\n")
	_ = f.Close()
	acc2 := ""
	deadline = time.Now().Add(3 * time.Second)
	for acc2 != "line3\n" && time.Now().Before(deadline) {
		select {
		case c := <-data:
			acc2 += string(c)
		case <-time.After(500 * time.Millisecond):
		}
	}
	if acc2 != "line3\n" {
		t.Fatalf("增量跟随错误: %q", acc2)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TailStream 取消后未返回")
	}
}

func TestTunnelEndToEnd(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true})
	}, &fakeEchoProxy{})
	endpoint, dev, key, err := p.b.OpenTunnel("bench-a", "^echo$")
	if err != nil {
		t.Fatalf("OpenTunnel 失败: %v", err)
	}
	if dev != "echo-dev" || key != "usb-echo-key" {
		t.Fatalf("隧道设备身份错误: %s/%s", dev, key)
	}
	conn, err := net.DialTimeout("tcp", endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("连接本地隧道端点失败: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("ping-%d\n", i)
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		if string(buf[:n]) != msg {
			t.Fatalf("回声不符: %q != %q", buf[:n], msg)
		}
	}
	_ = conn.Close()
	// 用户连接断开 → 隧道自清
	testutil.WaitFor(t, 2*time.Second, func() bool {
		p.b.mu.Lock()
		defer p.b.mu.Unlock()
		return len(p.b.tunnels) == 0
	}, "隧道未在用户连接结束后清理")
	if n := p.b.CloseTunnel("^echo$"); n != 0 {
		t.Fatalf("已清理的隧道不应再关: %d", n)
	}
}

func TestMeshStatusJSONShape(t *testing.T) {
	// 聚合状态能被 JSON 序列化（ctl.Response.Peers 出网形状）
	ps := ctl.PeerStatus{ID: "ab12", Name: "bench-a", Addr: "1.2.3.4:8802", State: "online", LatencyMs: 12, Devices: []ctl.DevState{{Name: "d"}}}
	b, err := json.Marshal(ctl.Response{OK: true, Peers: []ctl.PeerStatus{ps}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"peers"`, `"state":"online"`, `"latency_ms"`, `"devices"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("JSON 缺字段 %s: %s", want, b)
		}
	}
}
