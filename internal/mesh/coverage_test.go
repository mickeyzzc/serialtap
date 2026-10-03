package mesh

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/board"
	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/flash"
)

// TestNodeUDPDiscoveryPair: 真链路 UDP 发现——两节点各持真 UDP 端口，
// A 广播（目标注入为 B 的单播地址），B 应学得 A。
func TestNodeUDPDiscoveryPair(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	var bUDP atomic.Int32
	b := &Node{}
	var a *Node
	a, err := NewNode(Options{
		Name: "bench-a", Key: "k", Root: rootA, Port: 0, AutoApprove: true,
		Forward: func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true}) },
		// A 的广播目标 = B 的 UDP 端口（生产是定向广播，这里注入单播）
		BeaconTargets: func(int) []string { return []string{fmt.Sprintf("127.0.0.1:%d", bUDP.Load())} },
		AnnounceS:     1,
		Logf:          func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)

	b, err = NewNode(Options{
		Name: "bench-b", Key: "k", Root: rootB, Port: 0,
		Forward:   func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true}) },
		AnnounceS: 3600, // B 不主动广播（等 A 学完后反向清理麻烦），只听
		Logf:      func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	bUDP.Store(int32(b.self.Port))

	deadline := time.Now().Add(6 * time.Second) // 首帧即广播，宽限给 UDP 抖动
	for time.Now().Before(deadline) {
		for _, p := range b.Peers() {
			if p.ID == a.Self().ID && p.Addr == fmt.Sprintf("127.0.0.1:%d", a.Self().Port) {
				return // 学到：id、addr（含 TCP 端口）都对
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("B 未通过 beacon 学到 A: %+v", b.Peers())
}

// TestListenBeaconsFingerprintFilter: 不同密钥的 beacon 被忽略，同密钥的进回调。
func TestListenBeaconsFingerprintFilter(t *testing.T) {
	secA := testSecrets(t, "key-a")
	secB := testSecrets(t, "key-b")
	conn, err := ListenBeacons(0, "self-id", secA, func(b Beacon, from net.Addr) {
		t.Errorf("不同密钥的 beacon 不应进回调: %+v", b)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	addr := conn.LocalAddr().String()

	send := func(b Beacon) {
		c, err := net.DialTimeout("udp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write(encodeBeacon(b))
		_ = c.Close()
	}
	// B 密钥的 beacon（指纹不匹配）→ 被滤掉（回调报错即失败）
	send(Beacon{ID: "intruder", Name: "evil", Port: 1, FP: secB.Fingerprint("intruder")})
	// 自己的 beacon → 被滤掉
	send(Beacon{ID: "self-id", Name: "me", Port: 1, FP: secA.Fingerprint("self-id")})
	time.Sleep(200 * time.Millisecond)

	// 同密钥 beacon：换一个带正回调的监听器验证
	got := make(chan Beacon, 1)
	conn2, err := ListenBeacons(0, "self-id", secA, func(b Beacon, from net.Addr) {
		select {
		case got <- b:
		default:
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn2.Close() })
	c, err := net.DialTimeout("udp", conn2.LocalAddr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write(encodeBeacon(Beacon{ID: "friend", Name: "ok", Port: 8802, FP: secA.Fingerprint("friend")}))
	_ = c.Close()
	select {
	case b := <-got:
		if b.ID != "friend" || b.Port != 8802 {
			t.Fatalf("beacon 字段错误: %+v", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("同密钥 beacon 未进回调")
	}
}

// TestForwardDefaultBranch: 非 flash/board 请求经 Forward 原样转发。
func TestForwardDefaultBranch(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true, Line: "forwarded:" + req.Cmd + ":" + req.Pattern})
	}, nil)
	err := p.b.Forward("bench-a", ctl.Request{Cmd: "pause", Pattern: "^x$"}, func(r ctl.Response) {
		if r.Line != "forwarded:pause:^x$" {
			t.Errorf("默认分支转发错误: %+v", r)
		}
	})
	if err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
}

// TestForwardMeshDisabledErr: NewNode 空密钥拒绝；节点身份持久化往返。
func TestNodeIDPersistenceAndReject(t *testing.T) {
	if _, err := NewNode(Options{Key: "", Root: t.TempDir(), NoUDP: true}); err == nil {
		t.Fatal("空密钥必须拒绝启动")
	}
	root := t.TempDir()
	n1, err := NewNode(Options{Name: "x", Key: "k", Root: root, NoUDP: true,
		Forward: func(ctl.Request, func(ctl.Response)) {}})
	if err != nil {
		t.Fatal(err)
	}
	id1 := n1.Self()
	n1.Close()
	// 再起：id 复用（持久化生效），名字重新计算
	n2, err := NewNode(Options{Key: "k", Root: root, NoUDP: true,
		Forward: func(ctl.Request, func(ctl.Response)) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer n2.Close()
	if n2.Self().ID != id1.ID {
		t.Fatalf("node id 未持久化: %s != %s", n2.Self().ID, id1.ID)
	}
	// 坏 id 文件（长度不对）→ 重新生成
	if err := os.WriteFile(filepath.Join(root, ".mesh-node-id"), []byte("bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n3, err := NewNode(Options{Key: "k", Root: root, NoUDP: true,
		Forward: func(ctl.Request, func(ctl.Response)) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer n3.Close()
	if n3.Self().ID == "bad" || len(n3.Self().ID) != 8 {
		t.Fatalf("坏 id 文件应重新生成 8 位 id: %q", n3.Self().ID)
	}
}

// TestHandleDialRejects: 隧道请求的错误分支（坏 JSON / 空 pattern / 无代理能力）。
func TestHandleDialRejects(t *testing.T) {
	// 节点无 Proxy 能力 → 拒绝
	n, err := NewNode(Options{Name: "n", Key: "k", Root: t.TempDir(), NoUDP: true, AutoApprove: true,
		Forward: func(ctl.Request, func(ctl.Response)) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)

	dialAndExpectReject := func(payload []byte) {
		t.Helper()
		conn, derr := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", n.Self().Port))
		if derr != nil {
			t.Fatal(derr)
		}
		defer func() { _ = conn.Close() }()
		ch, _, herr := DialChannel(conn, testSecrets(t, "k"), Ident{ID: "c", Name: "c"})
		if herr != nil {
			t.Fatal(herr)
		}
		if err := ch.Send(ftDial, payload); err != nil {
			t.Fatal(err)
		}
		ft, got, rerr := ch.Recv()
		if rerr != nil {
			t.Fatal(rerr)
		}
		if ft != ftDialA {
			t.Fatalf("期待拒绝帧得到 %q", ft)
		}
		var ack dialAckFrame
		if err := json.Unmarshal(got, &ack); err != nil {
			t.Fatal(err)
		}
		if ack.OK || ack.Err == "" {
			t.Fatalf("必须拒绝: %+v", ack)
		}
	}
	dialAndExpectReject([]byte("{not json"))   // 坏 JSON
	dialAndExpectReject(mustJSON(dialFrame{})) // 空 pattern
}

// TestCallPeerOffline: 对端死地址 → 聚合状态报 offline 且带错误。
func TestAggregateStatusOfflinePeer(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true}) }, nil)
	// 塞一个永远不可达的静态地址
	p.b.reg.SetStatic([]string{"127.0.0.1:1"})
	agg := p.b.AggregateStatus(3 * time.Second)
	found := false
	for _, ps := range agg {
		if ps.Addr == "127.0.0.1:1" {
			found = true
			if ps.State != "offline" || ps.Err == "" {
				t.Fatalf("死地址必须 offline 带错误: %+v", ps)
			}
		}
	}
	if !found {
		t.Fatalf("静态死地址未出现在聚合: %+v", agg)
	}
}

// TestUploadRejectsBadNames: 非法文件名/会话 id 被拒（LastSeq=-1）。
func TestUploadRejectsBadNames(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true}) }, nil)
	_, err := p.b.uploadSession("bench-a", map[string][]byte{"../evil.bin": []byte("x")})
	if err == nil {
		t.Fatal("路径穿越文件名必须被拒")
	}
}

// TestRemoteFlashArgsFile: --args-file 分支——本地解析 flasher_args.json、
// 逐 bin 上传、chip 提示随迁、远端 spec 指向上传落盘。
func TestRemoteFlashArgsFile(t *testing.T) {
	build := t.TempDir()
	binPath := filepath.Join(build, "bootloader.bin")
	appPath := filepath.Join(build, "app.bin")
	if err := os.WriteFile(binPath, []byte("BOOTLOADER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appPath, []byte("APPIMAGE!"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(build, "flasher_args.json")
	args := `{"flash_files":{"0x0":"bootloader.bin","0x10000":"app.bin"},"extra_esptool_args":{"--chip":"esp32s3"}}`
	if err := os.WriteFile(argsPath, []byte(args), 0o600); err != nil {
		t.Fatal(err)
	}

	var seenBins []flashBinView
	var seenChip string
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd != "flash" {
			respond(ctl.Response{OK: false, Error: "unexpected"})
			return
		}
		seenChip = req.Spec.Chip
		seenBins = make([]flashBinView, len(req.Spec.Bins))
		for i, b := range req.Spec.Bins {
			data, _ := os.ReadFile(b.Path)
			seenBins[i] = flashBinView{Offset: b.Offset, Content: string(data)}
		}
		respond(ctl.Response{OK: true, Event: "flash-done"})
	}, nil)
	err := p.b.remoteFlash("bench-a", ctl.Request{Cmd: "flash", Pattern: "^d$",
		Spec: flash.Spec{ArgsFile: argsPath}}, func(ctl.Response) {})
	if err != nil {
		t.Fatalf("args-file 远程刷机失败: %v", err)
	}
	if seenChip != "esp32s3" {
		t.Fatalf("chip 提示未随迁: %q", seenChip)
	}
	if len(seenBins) != 2 || seenBins[0].Offset != "0x0" || seenBins[0].Content != "BOOTLOADER" ||
		seenBins[1].Offset != "0x10000" || seenBins[1].Content != "APPIMAGE!" {
		t.Fatalf("远端 bins 错误: %+v", seenBins)
	}
}

type flashBinView struct {
	Offset  string
	Content string
}

// TestRemoteBoardDumpNoArtifact: 对端 handler 没产文件 → 报"未返回产物引用"。
func TestRemoteBoardDumpNoArtifact(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		if req.Cmd == "board" && req.Board != nil && req.Board.Action == "dump" {
			respond(ctl.Response{OK: true, Event: "board-done"}) // 没写 OutPath
			return
		}
		respond(ctl.Response{OK: false, Error: "unexpected"})
	}, nil)
	err := p.b.Forward("bench-a", ctl.Request{Cmd: "board", Pattern: "^d$",
		Board: &board.Spec{Action: "dump", Addr: "0x0", Size: "0x100", OutPath: filepath.Join(t.TempDir(), "o.bin")}},
		func(ctl.Response) {})
	if err == nil {
		t.Fatal("无产物必须报错")
	}
}

// TestRemoteBoardDumpPeerDown: dump 转发时 peer 不可达——错误原样浮出。
func TestRemoteBoardDumpPeerDown(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true}) }, nil)
	// 学到 bench-a 后把它停掉，再发起远程 dump → 拨号失败错误浮出
	p.a.Close()
	err := p.b.Forward("bench-a", ctl.Request{Cmd: "board", Pattern: "^d$",
		Board: &board.Spec{Action: "dump", Addr: "0x0", Size: "0x100", OutPath: "unused.bin"}},
		func(ctl.Response) {})
	if err == nil {
		t.Fatal("peer 掉线时远程 dump 必须报错")
	}
}
