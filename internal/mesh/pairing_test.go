package mesh

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

// pairNodePair: 双节点回环拓扑，B 以静态种子自动链接 A（缩短重试间隔）。
// 返回（A, B）；A 的 forward 收到的请求经 chan 传出供断言。
func pairNodePair(t *testing.T, aAuto, bAuto bool) (*Node, *Node) {
	t.Helper()
	rootA, rootB := t.TempDir(), t.TempDir()
	forwardA := func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true, Line: "from-a:" + req.Cmd})
	}
	a, err := NewNode(Options{
		Name: "bench-a", Key: "k", Root: rootA, NoUDP: true, AutoApprove: aAuto,
		Forward: forwardA, PairRetryWait: 200 * time.Millisecond, LinkDialBackoff: 100 * time.Millisecond,
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	b, err := NewNode(Options{
		Name: "bench-b", Key: "k", Root: rootB, NoUDP: true, AutoApprove: bAuto,
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

func waitPeerAuthed(t *testing.T, n *Node, peerID string, timeout time.Duration) {
	t.Helper()
	testutil.WaitFor(t, timeout, func() bool {
		authed, known := n.peerAuthedFresh(peerID)
		return known && authed
	}, "等待对端授权状态转正")
}

func TestPairStoreLifecycle(t *testing.T) {
	root := t.TempDir()
	st, err := LoadPairStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Knock("ab12", "bench-a", "1.2.3.4:8802", false) {
		t.Fatal("首次敲门必须返回未授权")
	}
	if st.Approved("ab12") {
		t.Fatal("未批准前不得授权")
	}
	if st.Knock("ab12", "bench-a", "1.2.3.4:8802", false) {
		t.Fatal("重复敲门仍是未授权")
	}
	// 批准（按 id 前缀选择）
	id, err := st.Approve("ab")
	if err != nil || id != "ab12" {
		t.Fatalf("批准失败: %v %s", err, id)
	}
	if !st.Approved("ab12") {
		t.Fatal("批准后必须授权")
	}
	if st.Knock("ab12", "bench-a", "1.2.3.4:8802", false) != true {
		t.Fatal("批准后敲门返回 true")
	}
	// 拒绝名单：敲门即拒、不进 pending
	if st.Knock("cd34", "evil", "5.6.7.8:1", false) {
		t.Fatal("evil 不应自动授权")
	}
	if _, err := st.Revoke("cd34"); err != nil {
		t.Fatal(err)
	}
	if st.Knock("cd34", "evil", "5.6.7.8:1", false) {
		t.Fatal("拒绝名单敲门必须仍拒")
	}
	// 撤销已批准
	if _, err := st.Revoke("ab12"); err != nil {
		t.Fatal(err)
	}
	if st.Approved("ab12") {
		t.Fatal("撤销后不得授权")
	}
	// auto-approve 路径
	if !st.Knock("ef56", "new", "1.1.1.1:2", true) {
		t.Fatal("auto_approve 敲门即批")
	}
	// 持久化往返
	st2, err := LoadPairStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Approved("ef56") || st2.Approved("ab12") {
		t.Fatal("授权表持久化错误")
	}
	if _, err := st2.Approve("zz99"); err == nil {
		t.Fatal("未知选择器必须报错")
	}
}

func TestPairingGateUntilApproved(t *testing.T) {
	// A 需授权（默认）；B 自动链接 A 并敲门。批准前 B 的操作被拒；
	// A 批准后（≤重试周期）B 的操作放行。
	a, b := pairNodePair(t, false, false)

	// B 自动敲门 → A 记 pending
	testutil.WaitFor(t, 3*time.Second, func() bool {
		_, pending, _ := a.pairs.Overview()
		return len(pending) == 1
	}, "A 未收到 B 的敲门")

	// 批准前：B Call A 被闸门拒（带指引）
	_, err := b.Call("bench-a", ctl.Request{Cmd: "status"}, func(ctl.Response) bool { return false })
	if err == nil {
		t.Fatal("批准前操作必须被拒")
	}
	if !containsAll(err.Error(), "未授权", "mesh approve") {
		t.Fatalf("拒绝信息应带 approve 指引: %v", err)
	}

	// A 批准 → B 的链接自动转正 → 操作放行
	if _, err := a.ApprovePeer(b.Self().ID); err != nil {
		t.Fatal(err)
	}
	waitPeerAuthed(t, b, a.Self().ID, 5*time.Second)
	var got string
	_, err = b.Call("bench-a", ctl.Request{Cmd: "status"}, func(r ctl.Response) bool {
		got = r.Line
		return true
	})
	if err != nil {
		t.Fatalf("批准后操作仍失败: %v", err)
	}
	if got != "from-a:status" {
		t.Fatalf("操作结果错误: %q", got)
	}
}

func TestPairingAutoApproveOldBehavior(t *testing.T) {
	// A 开 auto-approve：B 敲门即批，操作直接可用（PR #31 的 PSK-only 行为）
	a, b := pairNodePair(t, true, false)
	waitPeerAuthed(t, b, a.Self().ID, 5*time.Second)
	var got string
	_, err := b.Call("bench-a", ctl.Request{Cmd: "status"}, func(r ctl.Response) bool {
		got = r.Line
		return true
	})
	if err != nil {
		t.Fatalf("auto-approve 下操作失败: %v", err)
	}
	if got != "from-a:status" {
		t.Fatalf("操作结果错误: %q", got)
	}
}

func TestPairingRevokeKillsAccess(t *testing.T) {
	a, b := pairNodePair(t, true, false)
	waitPeerAuthed(t, b, a.Self().ID, 5*time.Second)
	// 撤销 → 下一轮敲门转负 → B 快速失败
	if _, err := a.RevokePeer(b.Self().ID); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, 5*time.Second, func() bool {
		authed, known := b.peerAuthedFresh(a.Self().ID)
		return known && !authed
	}, "等待 B 感知被撤销")
	_, err := b.Call("bench-a", ctl.Request{Cmd: "status"}, nil)
	if err == nil {
		t.Fatal("撤销后操作必须失败")
	}
}

func TestPairingPersistenceAcrossRestart(t *testing.T) {
	// 授权表落盘：A 重启后无需重新批准 B
	rootA := t.TempDir()
	a, err := NewNode(Options{Name: "bench-a", Key: "k", Root: rootA, NoUDP: true,
		Forward:       func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true, Line: "ok"}) },
		PairRetryWait: 200 * time.Millisecond, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	a.pairs.Knock("deadbeef", "future-peer", "9.9.9.9:8802", false) // 先敲门进 pending
	if _, err := a.ApprovePeer("deadbeef"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := os.Stat(filepath.Join(rootA, ".mesh-peers.json")); err != nil {
		t.Fatalf("授权表未落盘: %v", err)
	}
	a2, err := NewNode(Options{Name: "bench-a", Key: "k", Root: rootA, NoUDP: true,
		Forward:       func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true, Line: "ok"}) },
		PairRetryWait: 200 * time.Millisecond, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if !a2.pairs.Approved("deadbeef") {
		t.Fatal("重启后授权丢失")
	}
}

func TestAutoLinkFromBeacon(t *testing.T) {
	// 广播路径的自动链接：B 通过注入 beacon 学到 A（不经静态种子），链接循环
	// 自动敲门。A auto-approve → B 视角转正。
	rootA, rootB := t.TempDir(), t.TempDir()
	a, err := NewNode(Options{Name: "bench-a", Key: "k", Root: rootA, NoUDP: true, AutoApprove: true,
		Forward:         func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true, Line: "beacon-ok"}) },
		PairRetryWait:   200 * time.Millisecond,
		LinkDialBackoff: 100 * time.Millisecond,
		Logf:            func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	b, err := NewNode(Options{Name: "bench-b", Key: "k", Root: rootB, NoUDP: true,
		Forward:         func(req ctl.Request, respond func(ctl.Response)) { respond(ctl.Response{OK: true}) },
		PairRetryWait:   200 * time.Millisecond,
		LinkDialBackoff: 100 * time.Millisecond,
		Logf:            func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	// 注入 beacon（模拟 B 收到 A 的广播）→ 注册表 + 自动链接
	b.reg.UpsertBeacon(Beacon{ID: a.Self().ID, Name: "bench-a", Port: a.Self().Port}, net.IPv4(127, 0, 0, 1))
	b.ensureLink(a.Self().ID, fmt.Sprintf("127.0.0.1:%d", a.Self().Port))

	waitPeerAuthed(t, b, a.Self().ID, 5*time.Second)
	var got string
	_, err = b.Call("bench-a", ctl.Request{Cmd: "status"}, func(r ctl.Response) bool { got = r.Line; return true })
	if err != nil || got != "beacon-ok" {
		t.Fatalf("beacon 自动链接后操作失败: %v %q", err, got)
	}
}

func TestPairAckFrameShape(t *testing.T) {
	// 配对帧 JSON 形状稳定（跨版本兼容面）
	ack := pairAckFrame{ID: "ab12", Name: "a", Approved: true}
	b, err := json.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id":"ab12"`, `"name":"a"`, `"approved":true`} {
		if !containsAll(string(b), want) {
			t.Fatalf("ack JSON 缺 %s: %s", want, b)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestPairStoreCorruptFileAndAmbiguity(t *testing.T) {
	root := t.TempDir()
	// 损坏文件 → 明确报错（不静默清空授权表）
	if err := os.WriteFile(filepath.Join(root, ".mesh-peers.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPairStore(root); err == nil {
		t.Fatal("损坏的授权表必须报错")
	}

	root2 := t.TempDir()
	st, _ := LoadPairStore(root2)
	st.Knock("aa01", "bench-x", "1.1.1.1:1", false)
	st.Knock("aa02", "bench-y", "1.1.1.1:2", false)
	if _, err := st.Approve("aa"); err == nil || !strings.Contains(err.Error(), "歧义") {
		t.Fatalf("歧义前缀必须报错: %v", err)
	}
	// 名字精确匹配可解歧义
	if id, err := st.Approve("bench-x"); err != nil || id != "aa01" {
		t.Fatalf("名字匹配失败: %v %s", err, id)
	}
}

func TestNodePairInfoSurfaces(t *testing.T) {
	a, b := pairNodePair(t, false, false)
	testutil.WaitFor(t, 3*time.Second, func() bool {
		_, pending, _ := a.PairInfo()
		return len(pending) == 1
	}, "PairInfo 应显示待授权")
	if _, err := a.ApprovePeer(b.Self().ID); err != nil {
		t.Fatal(err)
	}
	approved, _, _ := a.PairInfo()
	if len(approved) != 1 || approved[0].ID != b.Self().ID {
		t.Fatalf("批准后 PairInfo 错误: %+v", approved)
	}
}
