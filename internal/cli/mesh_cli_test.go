package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/mesh"
)

// captureStdout: 截获命令的标准输出（表格式输出的断言用）。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// meshFakeServer: 记录请求 + 按 cmd 回 canned 响应的 ctl 服务端。
type meshFakeServer struct {
	mu   sync.Mutex
	last ctl.Request
}

func startMeshFakeServer(t *testing.T) (*meshFakeServer, string) {
	t.Helper()
	fs := &meshFakeServer{}
	sock := ctlSockPath(t, "mesh-cli")
	ln, err := ctl.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ln.Close)
	go ln.Serve(func(req ctl.Request, respond func(ctl.Response)) {
		fs.mu.Lock()
		fs.last = req
		fs.mu.Unlock()
		switch req.Cmd {
		case "mesh":
			respond(ctl.Response{OK: true, Peers: []ctl.PeerStatus{
				{ID: "1a2b", Name: "bench-a", Addr: "192.168.63.10:8802", State: "online", LatencyMs: 3,
					Devices: []ctl.DevState{{Name: "n16r8-u1", Tty: "/dev/ttyUSB0", Key: "usb-1a86", State: "collecting"}}},
				{ID: "9f8e", Name: "bench-b", Addr: "192.168.63.11:8802", State: "offline", Err: "连不上 peer"},
			}})
		case "proxy":
			respond(ctl.Response{OK: true, Endpoint: "127.0.0.1:45678", Device: "echo-dev", DeviceKey: "usb-k"})
		case "mesh-pair":
			respond(ctl.Response{OK: true, Peers: []ctl.PeerStatus{
				{ID: "1a2b", Name: "bench-a", Addr: "192.168.63.10:8802", State: "pending"},
				{ID: "9f8e", Name: "bench-b", Addr: "192.168.63.11:8802", State: "approved"},
			}})
		case "mesh-approve", "mesh-revoke":
			if req.Pattern == "" {
				respond(ctl.Response{OK: false, Error: "缺少 peer 选择器"})
				return
			}
			respond(ctl.Response{OK: true, Line: req.Pattern})
		default:
			respond(ctl.Response{OK: true})
		}
	})
	return fs, sock
}

func TestMeshStatusTable(t *testing.T) {
	_, sock := startMeshFakeServer(t)
	out := captureStdout(t, func() {
		if code := Run([]string{"mesh", "status", "--sock", sock}); code != 0 {
			t.Errorf("mesh status 退出码 %d", code)
		}
	})
	for _, want := range []string{"bench-a", "1a2b", "online", "3ms", "n16r8-u1", "bench-b", "offline", "连不上 peer"} {
		if !strings.Contains(out, want) {
			t.Errorf("mesh status 表缺 %q:\n%s", want, out)
		}
	}
	// --json：可解析、字段齐
	out = captureStdout(t, func() {
		if code := Run([]string{"mesh", "status", "--json", "--sock", sock}); code != 0 {
			t.Errorf("mesh status --json 退出码 %d", code)
		}
	})
	var v struct {
		Peers []ctl.PeerStatus `json:"peers"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("--json 输出不可解析: %v\n%s", err, out)
	}
	if len(v.Peers) != 2 || v.Peers[0].Devices[0].Name != "n16r8-u1" {
		t.Fatalf("--json 内容错误: %+v", v.Peers)
	}
}

func TestMeshKeygen(t *testing.T) {
	a := captureStdout(t, func() {
		if code := Run([]string{"mesh", "keygen"}); code != 0 {
			t.Errorf("keygen 退出码 %d", code)
		}
	})
	b := captureStdout(t, func() {
		_ = Run([]string{"mesh", "keygen"})
	})
	ka, kb := strings.TrimSpace(a), strings.TrimSpace(b)
	if len(ka) != 32 || ka == kb {
		t.Fatalf("keygen 应输出 32 字符且每次不同: %q / %q", ka, kb)
	}
}

func TestMeshForwardPrintsEndpoint(t *testing.T) {
	_, sock := startMeshFakeServer(t)
	out := captureStdout(t, func() {
		if code := Run([]string{"mesh", "forward", "bench-a", "^echo$", "--sock", sock}); code != 0 {
			t.Errorf("mesh forward 退出码 %d", code)
		}
	})
	if !strings.Contains(out, "127.0.0.1:45678") || !strings.Contains(out, "echo-dev") {
		t.Fatalf("mesh forward 输出错误:\n%s", out)
	}
}

func TestPeerFlagPlumbedToRequest(t *testing.T) {
	fs, sock := startMeshFakeServer(t)
	// status --peer：请求带 Peer 字段
	if code := Run([]string{"status", "--peer", "bench-a", "--sock", sock}); code != 0 {
		t.Fatalf("status --peer 退出码 %d", code)
	}
	fs.mu.Lock()
	peer := fs.last.Peer
	fs.mu.Unlock()
	if peer != "bench-a" {
		t.Fatalf("status 请求未带 peer 字段: %q", peer)
	}

	// pause --peer：带字段且不走文件直改回退（守护可达，正常路径）
	if code := Run([]string{"pause", "--peer", "bench-a", "--sock", sock}); code != 0 {
		t.Fatalf("pause --peer 退出码 %d", code)
	}
	fs.mu.Lock()
	peer = fs.last.Peer
	fs.mu.Unlock()
	if peer != "bench-a" {
		t.Fatalf("pause 请求未带 peer 字段: %q", peer)
	}
}

func TestMeshPairTableAndApprove(t *testing.T) {
	_, sock := startMeshFakeServer(t)
	out := captureStdout(t, func() {
		if code := Run([]string{"mesh", "pair", "--sock", sock}); code != 0 {
			t.Errorf("mesh pair 退出码 %d", code)
		}
	})
	for _, want := range []string{"待授权", "bench-a", "1a2b", "mesh approve 1a2b", "已授权", "bench-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("mesh pair 输出缺 %q:\n%s", want, out)
		}
	}
	// approve：请求带选择器，成功回显
	out = captureStdout(t, func() {
		if code := Run([]string{"mesh", "approve", "1a2b", "--sock", sock}); code != 0 {
			t.Errorf("mesh approve 退出码 %d", code)
		}
	})
	if !strings.Contains(out, "已授权 peer 1a2b") {
		t.Errorf("approve 输出错误:\n%s", out)
	}
	// revoke 缺参 → 退出 1
	if code := Run([]string{"mesh", "revoke", "--sock", sock}); code != 1 {
		t.Errorf("revoke 缺参应退出 1: %d", code)
	}
}

func TestMeshSubcommandErrors(t *testing.T) {
	if code := Run([]string{"mesh", "bogus"}); code != 1 {
		t.Fatalf("未知子命令应退出 1: %d", code)
	}
	if code := Run([]string{"mesh"}); code != 1 {
		t.Fatalf("缺子命令应退出 1: %d", code)
	}
}

// TestMeshEnabledDaemonE2E: CLI → 本地 ctl socket → handler peer 分支 →
// mesh 加密信道 → 对端节点 → fake forward 的全链闭环（两个真 mesh.Node，
// 不经 cmdRun 的托盘/守护循环——peer 分支接线与 cmdRun 一字不差地复刻）。
func TestMeshEnabledDaemonE2E(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	sockB := ctlSockPath(t, "mesh-e2e-b")

	nodeA, err := mesh.NewNode(mesh.Options{
		Name: "bench-a", Key: "e2e-mesh-key", Root: rootA, NoUDP: true,
		PairRetryWait: 200 * time.Millisecond, LinkDialBackoff: 100 * time.Millisecond,
		Forward: func(req ctl.Request, respond func(ctl.Response)) {
			respond(ctl.Response{OK: true, Devices: []ctl.DevState{
				{Name: "remote-dev", Tty: "/dev/ttyUSB0", Key: "usb-r", State: "collecting"},
			}})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nodeA.Close)
	portA := nodeA.Self().Port

	nodeB, err := mesh.NewNode(mesh.Options{
		Name: "bench-b", Key: "e2e-mesh-key", Root: rootB, NoUDP: true,
		StaticPeers:     []string{fmt.Sprintf("127.0.0.1:%d", portA)},
		PairRetryWait:   200 * time.Millisecond,
		LinkDialBackoff: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nodeB.Close)

	ctlSrv, err := ctl.Listen(sockB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctlSrv.Close)
	go ctlSrv.Serve(func(req ctl.Request, respond func(ctl.Response)) {
		if req.Peer != "" {
			if ferr := nodeB.Forward(req.Peer, req, respond); ferr != nil {
				respond(ctl.Response{OK: false, Error: ferr.Error()})
			}
			return
		}
		respond(ctl.Response{OK: true, Devices: []ctl.DevState{{Name: "local-dev", State: "collecting"}}})
	})

	// 引导静态种子身份（生产里 mesh status 聚合做同样的事）——首连即敲门，
	// A 未授权 B：要么快速失败（链接循环已知未授权）要么闸门拒——都带 approve 指引
	firstErr := ""
	_, err = nodeB.Call(fmt.Sprintf("127.0.0.1:%d", portA), ctl.Request{Cmd: "status"}, func(r ctl.Response) bool {
		if !r.OK {
			firstErr = r.Error
		}
		return true
	})
	if !strings.Contains(fmt.Sprint(err)+firstErr, "mesh approve") {
		t.Fatalf("未授权首连必须被拒并带指引: err=%v resp=%q", err, firstErr)
	}

	// A 批准 B（= serialtap mesh approve）→ 链接重敲转正（200ms 周期）→ 操作放行。
	// 转正前 Call 会快速失败，按行为轮询直到成功。
	if _, err := nodeA.ApprovePeer(nodeB.Self().ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := nodeB.Call(fmt.Sprintf("127.0.0.1:%d", portA), ctl.Request{Cmd: "status"}, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("批准后操作未在 5s 内放行")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// status --peer：远端设备透传回 CLI。批准状态随敲门周期（200ms）传播，
	// CLI 单发可能撞上未授权窗口——按行为轮询直到成功（CI 慢机实测会抖）。
	deadline2 := time.Now().Add(5 * time.Second)
	var out string
	for {
		out = captureStdout(t, func() {
			if code := Run([]string{"status", "--peer", "bench-a", "--sock", sockB}); code != 0 {
				t.Errorf("status --peer 退出码 %d", code)
			}
		})
		if strings.Contains(out, "remote-dev") {
			break
		}
		if time.Now().After(deadline2) {
			t.Fatalf("远端设备未出现在输出:\n%s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
