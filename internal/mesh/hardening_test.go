package mesh

// TDD（#71）加固批次：会话重放（hello salt 去重）、同 IP 多节点的注册表
// 端口精确回填、聚合状态超时路径的竞态与连接回收、配对 pending TTL、
// Windows 保留设备名、单连接单尾随。

import (
	"crypto/cipher"
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

// TestAcceptChannelDuplicateSaltRejected: 同一 hello 字节（同 salt）的第二次
// 连接必须被拒——salt 重放意味着密钥流与首连接完全相同，攻击者可拼贴帧。
func TestAcceptChannelDuplicateSaltRejected(t *testing.T) {
	sec := testSecrets(t, "k")
	salt := []byte("0123456789abcdef")
	// 手工构造客户端字节序列：hello（明文）+ ident（密封），两次连接完全相同
	cc := &captureConn{}
	if err := writeFrame(cc, ftHello, mustJSON(helloFrame{V: protoVersion, Salt: b64(salt)})); err != nil {
		t.Fatalf("hello 写失败: %v", err)
	}
	c2s, s2c, err := deriveConnKeys(sec.master, salt)
	if err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	cli := &Channel{conn: cc, aeads: [2]cipher.AEAD{c2s, s2c}, role: 0}
	if err := cli.send(ftIdent, mustJSON(Ident{ID: "a", Name: "a", TS: time.Now().Unix()})); err != nil {
		t.Fatalf("ident 发送失败: %v", err)
	}
	var stream []byte
	for _, w := range cc.snapshot() {
		stream = append(stream, w...)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var salts saltCache
	try := func() error {
		t.Helper()
		cliConn, derr := net.Dial("tcp", ln.Addr().String())
		if derr != nil {
			t.Fatal(derr)
		}
		go func() {
			_, _ = cliConn.Write(stream)
		}()
		srvConn, aerr := ln.Accept()
		if aerr != nil {
			t.Fatal(aerr)
		}
		defer func() { _ = srvConn.Close() }()
		_, _, gerr := AcceptChannel(srvConn, sec, Ident{ID: "s", Name: "s"}, &salts)
		_ = cliConn.Close()
		return gerr
	}
	if err := try(); err != nil {
		t.Fatalf("首次握手必须成功: %v", err)
	}
	if err := try(); err == nil {
		t.Fatal("重复 salt 的重放连接必须被拒绝")
	}
}

// TestRegisterIncomingSameIPPortPrecise: 同 IP 两个节点（本机多实例/NAT 后
// 双节点）——X 的来话只能回填 X 的条目，不得按 IP 段把 peers[X] 改写成
// Y 的端口。
func TestRegisterIncomingSameIPPortPrecise(t *testing.T) {
	n := startNode(t, "bench-x", t.TempDir(), func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true})
	}, nil)
	n.reg.UpsertBeacon(Beacon{ID: "aaaa1111", Name: "x", Port: 10001}, net.IPv4(127, 0, 0, 1))
	n.reg.UpsertBeacon(Beacon{ID: "bbbb2222", Name: "y", Port: 10002}, net.IPv4(127, 0, 0, 1))

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", n.Self().Port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ch, _, err := DialChannel(conn, testSecrets(t, "test-mesh-key"), Ident{ID: "aaaa1111", Name: "x-renamed", Port: 10001})
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	_ = ch.Close()

	testutil.WaitFor(t, 3*time.Second, func() bool {
		for _, pe := range n.reg.List() {
			if pe.ID == "aaaa1111" {
				return pe.Name == "x-renamed"
			}
		}
		return false
	}, "X 条目应回填来话身份")
	var x, y Peer
	for _, pe := range n.reg.List() {
		switch pe.ID {
		case "aaaa1111":
			x = pe
		case "bbbb2222":
			y = pe
		}
	}
	if x.Addr != "127.0.0.1:10001" {
		t.Fatalf("X 条目地址不得被改写: %s", x.Addr)
	}
	if y.ID != "bbbb2222" || y.Name != "y" || y.Addr != "127.0.0.1:10002" {
		t.Fatalf("同 IP 的 Y 条目被来话改写: %+v", y)
	}
}

// TestAggregateStatusTimeoutReleasesConn: 楔死 peer（握手后不应答）+ 短超时
// ——超时路径不写共享 ps（-race 无告警），且 Call 的整体截止让连接最终被
// 关闭（楔死服务端可观察到对端关闭）。
func TestAggregateStatusTimeoutReleasesConn(t *testing.T) {
	sec := testSecrets(t, "test-mesh-key")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	closed := make(chan struct{}, 16)
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_, _, herr := AcceptChannel(c, sec, Ident{ID: "wedge", Name: "wedge"}, nil)
			if herr != nil {
				_ = c.Close()
				continue
			}
			go func() {
				buf := make([]byte, 64)
				for {
					if _, rerr := c.Read(buf); rerr != nil {
						closed <- struct{}{}
						return
					}
				}
			}()
		}
	}()

	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true})
	}, nil)
	p.b.reg.SetStatic([]string{ln.Addr().String()})

	agg := p.b.AggregateStatus(300 * time.Millisecond)
	found := false
	for _, ps := range agg {
		if ps.Addr == ln.Addr().String() {
			found = true
			if ps.State != "offline" || !strings.Contains(ps.Err, "超时") {
				t.Fatalf("楔死 peer 必须超时 offline: %+v", ps)
			}
		}
	}
	if !found {
		t.Fatalf("楔死 peer 未出现在聚合: %+v", agg)
	}
	// 连接最终被关（Call 整体截止 → defer close）——服务端观察到 Read 出错
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("楔死连接未被关闭（goroutine/conn 泄漏）")
	}
}

// TestPairPendingTTLPruned: pending 敲门记录超 24h 在载入/落盘时被清；
// 新近 pending 保留。
func TestPairPendingTTLPruned(t *testing.T) {
	root := t.TempDir()
	st, err := LoadPairStore(root)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.f.Pending["dead01"] = PairEntry{ID: "dead01", Name: "old", At: time.Now().Add(-25 * time.Hour)}
	st.mu.Unlock()
	if err := st.saveLocked(); err != nil { // 落盘即清
		t.Fatal(err)
	}
	st2, err := LoadPairStore(root)
	if err != nil {
		t.Fatal(err)
	}
	st2.mu.Lock()
	_, hasOld := st2.f.Pending["dead01"]
	st2.mu.Unlock()
	if hasOld {
		t.Fatal("过期 pending 应在落盘/载入时被清理")
	}
	// 新近敲门保留
	st2.Knock("fresh1", "bench-new", "1.2.3.4:8802", false)
	st3, err := LoadPairStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, pending, _ := st3.Overview(); len(pending) != 1 {
		t.Fatalf("新近 pending 不应被清理: %+v", pending)
	}
}

// TestUploadRejectsWindowsReservedNames: Windows 保留设备名（含带扩展名的
// 形式，按首个 '.' 前的主名判断）不得作为上传落盘文件名。
func TestUploadRejectsWindowsReservedNames(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true})
	}, nil)
	for _, name := range []string{"con", "PRN", "aux", "nul", "COM3", "lpt1.bin", "NUL.txt"} {
		if _, err := p.b.uploadSession("bench-a", map[string][]byte{name: []byte("x")}); err == nil {
			t.Fatalf("保留设备名 %q 必须被拒", name)
		}
	}
	if _, err := p.b.uploadSession("bench-a", map[string][]byte{"console.bin": []byte("x")}); err != nil {
		t.Fatalf("普通名不应误伤: %v", err)
	}
}

// TestServeConnSingleTailPerConn: 同连接第二个 ftTail 被拒（EOF 空块），
// 不产生第二条尾随流（无第二个设备的数据到达）。
func TestServeConnSingleTailPerConn(t *testing.T) {
	p := newNodePair(t, func(req ctl.Request, respond func(ctl.Response)) {
		respond(ctl.Response{OK: true})
	}, nil)
	for _, dev := range []string{"dev1", "dev2"} {
		dir := filepath.Join(p.rootA, dev)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "serial-20260101.log"), []byte("data-of-"+dev+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.a.Self().Port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ch, _, err := DialChannel(conn, testSecrets(t, "test-mesh-key"), Ident{ID: "t", Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// 第一个尾随：收到 dev1 初始尾部
	if err := ch.Send(ftTail, mustJSON(tailFrame{Device: "dev1", Kind: "serial"})); err != nil {
		t.Fatal(err)
	}
	var acc string
	for acc == "" {
		_, payload, rerr := ch.Recv()
		if rerr != nil {
			t.Fatalf("第一尾随接收失败: %v", rerr)
		}
		var bf blockFrame
		if jerr := json.Unmarshal(payload, &bf); jerr != nil {
			t.Fatal(jerr)
		}
		if bf.B64 != "" {
			b, _ := unb64(bf.B64)
			acc += string(b)
		}
	}
	if !strings.Contains(acc, "data-of-dev1") {
		t.Fatalf("第一尾随数据错误: %q", acc)
	}

	// 同连接第二个尾随：必须被拒（EOF 空块），且 dev2 数据永不到达
	if err := ch.Send(ftTail, mustJSON(tailFrame{Device: "dev2", Kind: "serial"})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, payload, rerr := ch.Recv()
		if rerr != nil {
			t.Fatalf("等待拒绝帧失败: %v", rerr)
		}
		var bf blockFrame
		if jerr := json.Unmarshal(payload, &bf); jerr != nil {
			t.Fatal(jerr)
		}
		if bf.B64 != "" {
			b, _ := unb64(bf.B64)
			t.Fatalf("第二尾随不应产生数据流: %q", b)
		}
		if bf.EOF {
			return // 拒绝帧到达，且此前没有任何 dev2 数据块
		}
	}
	t.Fatal("第二个尾随请求未收到拒绝（EOF）帧")
}

// Call 对拒绝帧必须返回错误（End 帧裁决回归）：清掉链接状态强制走真拨号
// 路径（绕开 peerAuthedFresh 快速失败）后，未授权闸门的 OK:false+End 帧
// 此前被当"成功"静默吞掉——调用方只见零响应与 nil 错误。
func TestCallReturnsErrorOnRejectionFrame(t *testing.T) {
	a, b := pairNodePair(t, false, false)
	testutil.WaitFor(t, 3*time.Second, func() bool {
		_, pending, _ := a.pairs.Overview()
		return len(pending) == 1
	}, "A 未收到 B 的敲门")

	b.linkMu.Lock()
	b.linkSt = map[string]linkState{} // 制造"状态未知"窗口
	b.linkMu.Unlock()
	_, err := b.Call("bench-a", ctl.Request{Cmd: "status"}, func(ctl.Response) bool { return false })
	if err == nil {
		t.Fatal("拒绝帧被当成功返回 nil——End 帧裁决失效")
	}
	if !containsAll(err.Error(), "未授权") {
		t.Fatalf("拒绝错误应带未授权信息: %v", err)
	}

	// 批准后正常放行（OK 流不受影响）
	if _, err := a.ApprovePeer(b.Self().ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := b.Call("bench-a", ctl.Request{Cmd: "status"}, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("批准后操作未放行: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
