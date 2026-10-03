package mesh

import (
	"crypto/cipher"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// 测试全程低迭代派生（生产 600k 是一次性成本，测试里纯拖慢）。
func TestMain(m *testing.M) {
	old := pbkdf2Iters
	pbkdf2Iters = 1000
	defer func() { pbkdf2Iters = old }()
	m.Run()
}

func testSecrets(t *testing.T, passphrase string) *Secrets {
	t.Helper()
	sec, err := deriveSecrets(passphrase, pbkdf2Iters)
	if err != nil {
		t.Fatalf("派生密钥失败: %v", err)
	}
	return sec
}

// 两个真实 TCP 连接做一次完整握手，返回（客户端信道, 服务端信道, 客户端视角对端, 服务端视角对端）。
func handshakePair(t *testing.T, clientKey, serverKey string, client Ident, server Ident) (*Channel, *Channel, Ident, Ident) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type result struct {
		ch   *Channel
		peer Ident
		err  error
	}
	cliCh := make(chan result, 1)
	go func() {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			cliCh <- result{err: err}
			return
		}
		ch, peer, err := DialChannel(conn, testSecrets(t, clientKey), client)
		cliCh <- result{ch: ch, peer: peer, err: err}
	}()
	srvConn, err := ln.Accept()
	if err != nil {
		t.Fatalf("接受连接失败: %v", err)
	}
	srvCh, srvPeer, err := AcceptChannel(srvConn, testSecrets(t, serverKey), server)
	cr := <-cliCh
	if cr.err != nil {
		t.Fatalf("客户端握手失败: %v", cr.err)
	}
	if err != nil {
		t.Fatalf("服务端握手失败: %v", err)
	}
	t.Cleanup(func() {
		_ = cr.ch.Close()
		_ = srvCh.Close()
	})
	return cr.ch, srvCh, cr.peer, srvPeer
}

func TestDeriveSecrets(t *testing.T) {
	if _, err := DeriveSecrets(""); err == nil {
		t.Fatal("空口令必须被拒绝")
	}
	sec := testSecrets(t, "same-pass")
	// 同口令同身份 → 指纹稳定；不同身份 → 指纹不同
	fp1a, fp1b := sec.Fingerprint("node-aaaa"), sec.Fingerprint("node-aaaa")
	if fp1a != fp1b {
		t.Fatalf("指纹不稳定: %s != %s", fp1a, fp1b)
	}
	if sec.Fingerprint("node-bbbb") == fp1a {
		t.Fatal("不同身份指纹不应相同")
	}
	if !sec.ValidFingerprint("node-aaaa", fp1a) {
		t.Fatal("合法指纹未通过校验")
	}
	if sec.ValidFingerprint("node-aaaa", "deadbeefdeadbeef") {
		t.Fatal("伪造指纹不应通过校验")
	}
	// 不同口令 → 不同指纹（mesh 隔离）
	other := testSecrets(t, "other-pass")
	if other.Fingerprint("node-aaaa") == fp1a {
		t.Fatal("不同口令不应产生相同指纹")
	}
}

func TestChannelHandshakeRoundtrip(t *testing.T) {
	cli, srv, cliPeer, srvPeer := handshakePair(t, "k", "k",
		Ident{ID: "cid", Name: "cli"}, Ident{ID: "sid", Name: "srv"})
	if cliPeer.ID != "sid" || cliPeer.Name != "srv" {
		t.Fatalf("客户端看到的对端身份错误: %+v", cliPeer)
	}
	if srvPeer.ID != "cid" || srvPeer.Name != "cli" {
		t.Fatalf("服务端看到的对端身份错误: %+v", srvPeer)
	}
	// 双向收发
	if err := cli.Send(ftPing, mustJSON(map[string]int64{"t": 42})); err != nil {
		t.Fatalf("客户端发送失败: %v", err)
	}
	ft, payload, err := srv.Recv()
	if err != nil || ft != ftPing {
		t.Fatalf("服务端接收失败: %v 类型=%q", err, ft)
	}
	var m map[string]int64
	if err := json.Unmarshal(payload, &m); err != nil || m["t"] != 42 {
		t.Fatalf("载荷错误: %s", payload)
	}
	if err := srv.Send(ftPong, mustJSON(map[string]int64{"t": 42})); err != nil {
		t.Fatalf("服务端发送失败: %v", err)
	}
	if ft, _, err = cli.Recv(); err != nil || ft != ftPong {
		t.Fatalf("客户端接收失败: %v 类型=%q", err, ft)
	}
}

func TestChannelWrongKey(t *testing.T) {
	// 密钥不匹配：两侧握手都必须失败（服务端解封客户端自报失败并关闭
	// 连接；客户端随之报错，不必等满握手超时）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().String()

	cliDone := make(chan error, 1)
	go func() {
		conn, derr := net.Dial("tcp", addr)
		if derr != nil {
			cliDone <- derr
			return
		}
		_, _, derr = DialChannel(conn, testSecrets(t, "client-key"), Ident{ID: "c", Name: "c"})
		cliDone <- derr
	}()
	srvConn, err := ln.Accept()
	if err != nil {
		t.Fatalf("接受失败: %v", err)
	}
	if _, _, err = AcceptChannel(srvConn, testSecrets(t, "server-key"), Ident{ID: "s", Name: "s"}); err == nil {
		t.Fatal("密钥不匹配时服务端握手必须失败")
	}
	select {
	case cliErr := <-cliDone:
		if cliErr == nil {
			t.Fatal("密钥不匹配时客户端握手必须失败")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("客户端握手未在超时内返回")
	}
}

func TestChannelLargeFrame(t *testing.T) {
	cli, srv, _, _ := handshakePair(t, "k", "k", Ident{ID: "a", Name: "a"}, Ident{ID: "b", Name: "b"})
	payload := make([]byte, 4<<20) // 4MB（上限 8MB 内）
	for i := range payload {
		payload[i] = byte(i)
	}
	go func() {
		if err := cli.Send(ftWire, payload); err != nil {
			t.Errorf("大帧发送失败: %v", err)
		}
	}()
	_ = srv.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	ft, got, err := srv.Recv()
	if err != nil {
		t.Fatalf("大帧接收失败: %v", err)
	}
	if ft != ftWire || len(got) != len(payload) {
		t.Fatalf("大帧不一致: 类型=%q 长度=%d", ft, len(got))
	}
	for i := 0; i < len(got); i += 999983 { // 抽查（全比 4MB 太慢）
		if got[i] != payload[i] {
			t.Fatalf("大帧内容错误 @%d", i)
		}
	}
}

// —— 重放测试：手工构造客户端帧序列，重放其中一帧 ——

// captureConn: 只录写的假连接（客户端不发读）。
type captureConn struct {
	mu     sync.Mutex
	writes [][]byte
}

func (c *captureConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	return len(p), nil
}
func (c *captureConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *captureConn) Close() error             { return nil }
func (c *captureConn) LocalAddr() net.Addr      { return nil }
func (c *captureConn) RemoteAddr() net.Addr     { return nil }
func (c *captureConn) SetDeadline(time.Time) error {
	return nil
}
func (c *captureConn) SetReadDeadline(time.Time) error  { return nil }
func (c *captureConn) SetWriteDeadline(time.Time) error { return nil }
func (c *captureConn) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.writes))
	copy(out, c.writes)
	return out
}

// fakeConn: 预置字节流的只读连接。
type fakeConn struct{ buf []byte }

func (f *fakeConn) Read(p []byte) (int, error) {
	if len(f.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}
func (f *fakeConn) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeConn) Close() error                { return nil }
func (f *fakeConn) LocalAddr() net.Addr         { return nil }
func (f *fakeConn) RemoteAddr() net.Addr        { return nil }
func (f *fakeConn) SetDeadline(time.Time) error { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error {
	return nil
}
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func TestChannelReplayRejected(t *testing.T) {
	// 客户端帧序列: hello(明文) → ident(密封) → ping1 → ping2；
	// 服务端读到 ping1 后再收到重放的 ping1 → 帧序 AAD 不符，必须报错。
	sec := testSecrets(t, "k")
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
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
	if err := cli.send(ftPing, []byte("one")); err != nil {
		t.Fatalf("ping1 发送失败: %v", err)
	}
	ping1 := cc.snapshot()[2]
	if err := cli.send(ftPing, []byte("two")); err != nil {
		t.Fatalf("ping2 发送失败: %v", err)
	}
	writes := cc.snapshot()

	var stream []byte
	for _, w := range writes[1:] { // 跳过明文 hello（服务端此轮只解密封帧）
		stream = append(stream, w...)
	}
	stream = append(stream, ping1...) // 重放

	srv := &Channel{conn: &fakeConn{buf: stream}, aeads: [2]cipher.AEAD{c2s, s2c}, role: 1}
	ok := 0
	for {
		if _, _, err := srv.recv(); err != nil {
			break
		}
		ok++
	}
	if ok != 3 { // ident + ping1 + ping2；重放的第 4 帧必须被拒
		t.Fatalf("期望前 3 帧成功、第 4 帧被拒，实际成功 %d 帧", ok)
	}
}
