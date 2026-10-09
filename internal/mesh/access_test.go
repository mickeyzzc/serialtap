package mesh

// TDD：mesh 节点接入权限模式（mesh_access = "ro" | "rw"）。
// ro = 只读接入：远端 peer 只能读（status/facts/日志尾随），一切写命令
// （pause/resume/proxy/release/reopen/reset/flash/board/隧道…）与本机串口
// 数据通道都被拒；rw = 可写接入（现状默认，全权）。
// 三个行为测试 + 传播测试（PeerStatus.Access 让对端/面板可见）。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

// accessPair: 双节点，A 以指定接入模式运行（AccessMode），双方 auto-approve
// 免配对流程，B 静态种子指向 A。
func accessPair(t *testing.T, aAccess string) (*Node, *Node) {
	t.Helper()
	rootA, rootB := t.TempDir(), t.TempDir()
	a, err := NewNode(Options{
		Name: "bench-a", Key: "k", Root: rootA, NoUDP: true, AutoApprove: true,
		AccessMode: aAccess,
		Forward: func(req ctl.Request, respond func(ctl.Response)) {
			respond(ctl.Response{OK: true, Line: "from-a:" + req.Cmd})
		},
		PairRetryWait:   200 * time.Millisecond,
		LinkDialBackoff: 100 * time.Millisecond,
		Logf:            func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	b, err := NewNode(Options{
		Name: "bench-b", Key: "k", Root: rootB, NoUDP: true, AutoApprove: true,
		StaticPeers:     []string{fmt.Sprintf("127.0.0.1:%d", a.Self().Port)},
		Forward:         func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true}) },
		PairRetryWait:   200 * time.Millisecond,
		LinkDialBackoff: 100 * time.Millisecond,
		Logf:            func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return a, b
}

func callPeer(t *testing.T, n *Node, peer, cmd string) (ctl.Response, error) {
	t.Helper()
	var last ctl.Response
	err := n.Forward(peer, ctl.Request{Cmd: cmd}, func(r ctl.Response) { last = r })
	return last, err
}

// ro 节点：读命令放行、写命令拒绝（错误信息指明只读模式）。
func TestAccessReadOnlyGate(t *testing.T) {
	_, b := accessPair(t, "ro")
	testutil.WaitFor(t, 5*time.Second, func() bool {
		resp, err := callPeer(t, b, "bench-a", "status")
		return err == nil && resp.OK
	}, "等 B→A 链路就绪")

	// 读：status / facts 放行
	for _, cmd := range []string{"status", "facts"} {
		resp, err := callPeer(t, b, "bench-a", cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if !resp.OK {
			t.Fatalf("ro 节点必须放行读命令 %s: %+v", cmd, resp)
		}
	}
	// 写：pause / flash / board / reset / proxy / release / reopen / resume 全拒
	for _, cmd := range []string{"pause", "resume", "proxy", "release", "reopen", "reset", "flash", "board"} {
		resp, err := callPeer(t, b, "bench-a", cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if resp.OK {
			t.Fatalf("ro 节点必须拒绝写命令 %s", cmd)
		}
		if !strings.Contains(resp.Error, "只读") {
			t.Fatalf("%s 拒绝信息应指明只读模式: %+v", cmd, resp)
		}
	}
}

// rw（默认）节点：写命令照常到达业务 handler。
func TestAccessReadWriteStillWorks(t *testing.T) {
	_, b := accessPair(t, "")
	testutil.WaitFor(t, 5*time.Second, func() bool {
		resp, err := callPeer(t, b, "bench-a", "status")
		return err == nil && resp.OK
	}, "等 B→A 链路就绪")
	resp, err := callPeer(t, b, "bench-a", "pause")
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Line != "from-a:pause" {
		t.Fatalf("rw 节点写命令应直达业务: %+v", resp)
	}
}

// 接入模式传播：status 响应携带 access → 对端 PeerStatus 可见（面板徽标数据源）。
func TestAccessPropagatesToPeerStatus(t *testing.T) {
	_, b := accessPair(t, "ro")
	testutil.WaitFor(t, 5*time.Second, func() bool {
		for _, p := range b.AggregateStatus(2 * time.Second) {
			if p.Name == "bench-a" {
				return p.Access == "ro"
			}
		}
		return false
	}, "PeerStatus.Access 应为 ro")
	// rw 默认传播
	_, b2 := accessPair(t, "")
	testutil.WaitFor(t, 5*time.Second, func() bool {
		for _, p := range b2.AggregateStatus(2 * time.Second) {
			if p.Name == "bench-a" {
				return p.Access == "rw"
			}
		}
		return false
	}, "PeerStatus.Access 默认应为 rw")
}

// ro 节点拒绝数据隧道（at --peer / mesh forward 会直写对端串口）。
func TestAccessReadOnlyRejectsTunnel(t *testing.T) {
	_, b := accessPair(t, "ro")
	testutil.WaitFor(t, 5*time.Second, func() bool {
		resp, err := callPeer(t, b, "bench-a", "status")
		return err == nil && resp.OK
	}, "等 B→A 链路就绪")
	_, _, _, err := b.OpenTunnel("bench-a", "dev")
	if err == nil || !strings.Contains(err.Error(), "只读") {
		t.Fatalf("ro 节点必须拒绝隧道: err=%v", err)
	}
}
