package mesh

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// 代理隧道：proxy 透传端点只听 peer 的 127.0.0.1，跨机访问必须经 mesh。
// 发起侧在本地开 127.0.0.1 监听（OpenTunnel），接受的用户 TCP 连接与
// mesh 信道（ftWire 裸字节帧）双向泵送；peer 侧 handleDial 把对端设备的
// proxy 端点桥进信道。本地 `at --peer` / 通用 `mesh forward` 都走这条路径。

type dialFrame struct {
	Pattern string `json:"pattern"` // 设备匹配正则（peer 侧 ProxyStart）
}
type dialAckFrame struct {
	OK        bool   `json:"ok"`
	Device    string `json:"device,omitempty"`
	DeviceKey string `json:"device_key,omitempty"`
	Err       string `json:"err,omitempty"`
}

// tunnel: 发起侧的本地监听 + peer 信道。
type tunnel struct {
	node     *Node
	pattern  string
	ln       net.Listener
	ch       *Channel
	device   string
	devKey   string
	mu       sync.Mutex
	closed   bool
	stopOnce sync.Once
}

// handleDial: peer 侧——ProxyStart 匹配设备 → 拨本地透传端点 → 回 ack →
// 占用连接进入裸字节双向泵。任一侧断开即收尾（ProxyStop 归还端口）。
func (n *Node) handleDial(ch *Channel, payload []byte) {
	var df dialFrame
	if err := json.Unmarshal(payload, &df); err != nil || df.Pattern == "" {
		_ = ch.Send(ftDialA, mustJSON(dialAckFrame{OK: false, Err: "mesh 隧道请求非法"}))
		return
	}
	if n.readOnly() {
		_ = ch.Send(ftDialA, mustJSON(dialAckFrame{OK: false, Err: "对端以只读模式接入（mesh_access=ro），数据隧道被拒"}))
		return
	}
	if n.opt.Proxy == nil {
		_ = ch.Send(ftDialA, mustJSON(dialAckFrame{OK: false, Err: "节点无代理能力"}))
		return
	}
	endpoint, device, devKey, err := n.opt.Proxy.ProxyStart(df.Pattern)
	if err != nil {
		_ = ch.Send(ftDialA, mustJSON(dialAckFrame{OK: false, Err: err.Error()}))
		return
	}
	local, err := net.DialTimeout("tcp", endpoint, 3*time.Second)
	if err != nil {
		_, _ = n.opt.Proxy.ProxyStop(devKey)
		_ = ch.Send(ftDialA, mustJSON(dialAckFrame{OK: false, Err: "连接本机透传端点失败: " + err.Error()}))
		return
	}
	defer func() {
		_ = local.Close()
		_, _ = n.opt.Proxy.ProxyStop(devKey)
	}()
	if err := ch.Send(ftDialA, mustJSON(dialAckFrame{OK: true, Device: device, DeviceKey: devKey})); err != nil {
		return
	}
	n.logf("[mesh] 隧道建立 %s ← %s（%s）", device, ch.PeerAddr(), endpoint)

	// 设备 → 发起侧
	toPeer := make(chan error, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			nr, rerr := local.Read(buf)
			if nr > 0 {
				if serr := ch.Send(ftWire, buf[:nr]); serr != nil {
					toPeer <- serr
					return
				}
			}
			if rerr != nil {
				toPeer <- rerr
				return
			}
		}
	}()
	// 发起侧 → 设备
	fromPeer := make(chan error, 1)
	go func() {
		for {
			t, p, rerr := ch.Recv()
			if rerr != nil {
				fromPeer <- rerr
				return
			}
			if t != ftWire {
				continue
			}
			if _, werr := local.Write(p); werr != nil {
				fromPeer <- werr
				return
			}
		}
	}()
	select {
	case <-toPeer:
		_ = ch.Close() // 解除 fromPeer 的 Recv 阻塞
		<-fromPeer
	case <-fromPeer:
		_ = local.Close() // 解除 toPeer 的 Read 阻塞
		<-toPeer
	}
	n.logf("[mesh] 隧道结束 %s（%s）", device, ch.PeerAddr())
}

// OpenTunnel: 发起侧——向 peer 建隧道并开本地回环监听。返回本地端点
// （业务工具/`at` 直连它）与对端确认的设备身份。隧道持续到用户连接结束
// 后自动收尾，或 CloseTunnel 主动关。
func (n *Node) OpenTunnel(peerName, pattern string) (endpoint, device, devKey string, err error) {
	peer, err := n.reg.Match(peerName)
	if err != nil {
		return "", "", "", err
	}
	conn, err := net.DialTimeout("tcp", peer.Addr, 3*time.Second)
	if err != nil {
		return "", "", "", fmt.Errorf("连不上 peer %s: %w", peer.NameOrAddr(), err)
	}
	ch, ident, err := DialChannel(conn, n.sec, n.self)
	if err != nil {
		_ = conn.Close()
		return "", "", "", fmt.Errorf("与 peer %s 握手失败: %w", peerName, err)
	}
	n.reg.Touch(ident.ID)
	if err := ch.Send(ftDial, mustJSON(dialFrame{Pattern: pattern})); err != nil {
		_ = ch.Close()
		return "", "", "", err
	}
	t, payload, rerr := ch.Recv()
	if rerr != nil {
		_ = ch.Close()
		return "", "", "", rerr
	}
	if t != ftDialA {
		_ = ch.Close()
		return "", "", "", fmt.Errorf("隧道握手收到意外帧 %q", t)
	}
	var ack dialAckFrame
	if jerr := json.Unmarshal(payload, &ack); jerr != nil {
		_ = ch.Close()
		return "", "", "", jerr
	}
	if !ack.OK {
		_ = ch.Close()
		return "", "", "", fmt.Errorf("peer 建隧道失败: %s", ack.Err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = ch.Close()
		return "", "", "", err
	}
	tun := &tunnel{node: n, pattern: pattern, ln: ln, ch: ch, device: ack.Device, devKey: ack.DeviceKey}
	n.mu.Lock()
	n.tunnels[pattern] = tun
	n.mu.Unlock()

	go tun.serve()
	n.logf("[mesh] 本地隧道端点 %s → peer %s 设备 %s", ln.Addr().String(), ident.Name, ack.Device)
	return ln.Addr().String(), ack.Device, ack.DeviceKey, nil
}

// CloseTunnel: 按设备匹配正则关隧道（proxy stop 的 mesh 语义）。
func (n *Node) CloseTunnel(pattern string) int {
	n.mu.Lock()
	tun, ok := n.tunnels[pattern]
	if ok {
		delete(n.tunnels, pattern)
	}
	n.mu.Unlock()
	if !ok {
		return 0
	}
	tun.close()
	return 1
}

// serve: 逐个接受本地连接（会话串行——proxy 端点单客户端）。用户连接
// 断开即整隧道收尾（`at` 一问一答的用法即如此）。
func (t *tunnel) serve() {
	defer t.node.dropTunnel(t)
	conn, err := t.ln.Accept()
	if err != nil {
		t.stopOnce.Do(func() { t.close() })
		return
	}
	t.bridge(conn)
	t.stopOnce.Do(func() { t.close() })
}

// dropTunnel: serve 结束时从节点表摘除自己（幂等）。
func (n *Node) dropTunnel(t *tunnel) {
	n.mu.Lock()
	if cur, ok := n.tunnels[t.pattern]; ok && cur == t {
		delete(n.tunnels, t.pattern)
	}
	n.mu.Unlock()
	t.stopOnce.Do(func() { t.close() })
}

// bridge: 单个用户连接 ↔ mesh 信道的双向泵，任一侧断开即返回。
func (t *tunnel) bridge(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	toUser := make(chan error, 1)
	go func() { // 信道 → 用户连接
		for {
			typ, payload, rerr := t.ch.Recv()
			if rerr != nil {
				toUser <- rerr
				return
			}
			if typ != ftWire {
				continue
			}
			if _, werr := conn.Write(payload); werr != nil {
				toUser <- werr
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for { // 用户连接 → 信道
		nr, rerr := conn.Read(buf)
		if nr > 0 {
			if serr := t.ch.Send(ftWire, buf[:nr]); serr != nil {
				_ = conn.Close()
				<-toUser
				return
			}
		}
		if rerr != nil {
			_ = t.ch.Close()
			<-toUser
			return
		}
	}
}

func (t *tunnel) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()
	_ = t.ln.Close()
	_ = t.ch.Close()
}
