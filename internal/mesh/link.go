package mesh

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// 自动链接：beacon/静态种子学到一个 peer 就起一条链接循环——拨号 → 握手
// → ftPair（敲门）→ 根据应答决定保活或重试。链接即在场证明与授权状态
// 同步：对端批准后 ≤ 重试间隔内自动转正，无需人工干预发起侧。

const (
	linkRetryUnauthed = 30 * time.Second // 未授权重试敲门间隔
	linkPingEvery     = 30 * time.Second // 已授权链接的心跳间隔
	linkBackoffMin    = 5 * time.Second
	linkBackoffMax    = 60 * time.Second
)

// linkLoop: 单 peer 的自动链接循环。
type linkLoop struct {
	node *Node
	key  string // 注册表键（peer id 或 "static:addr"）
	addr string // 首次拨号地址（后续以注册表登记为准）
	stop chan struct{}
}

// ensureLink: 起链接循环（幂等；key 已有循环则只刷新地址）。
func (n *Node) ensureLink(key, addr string) {
	n.linkMu.Lock()
	defer n.linkMu.Unlock()
	if l, ok := n.links[key]; ok {
		if addr != "" {
			l.addr = addr
		}
		return
	}
	l := &linkLoop{node: n, key: key, addr: addr, stop: make(chan struct{})}
	n.links[key] = l
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		l.run()
	}()
}

func (l *linkLoop) run() {
	n := l.node
	backoff := linkBackoffMin
	if n.opt.LinkDialBackoff > 0 {
		backoff = n.opt.LinkDialBackoff
	}
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-l.stop:
			return
		default:
		}
		// 地址以注册表最新为准（beacon 可能换了端口）
		addr := l.resolvedAddr()
		if addr == "" {
			return // peer 已从注册表消失且无静态地址
		}
		ok := l.linkOnce(addr)
		wait := linkRetryUnauthed
		if n.opt.PairRetryWait > 0 {
			wait = n.opt.PairRetryWait
		}
		if !ok {
			wait = backoff
			backoff *= 2
			if backoff > linkBackoffMax {
				backoff = linkBackoffMax
			}
		}
		select {
		case <-n.ctx.Done():
			return
		case <-l.stop:
			return
		case <-time.After(wait):
		}
	}
}

// resolvedAddr: 注册表优先，静态种子兜底。
func (l *linkLoop) resolvedAddr() string {
	if p, ok := l.node.reg.ByKey(l.key); ok && p.Addr != "" {
		return p.Addr
	}
	if p, ok := l.node.reg.ByAddr(l.addr); ok {
		return p.Addr
	}
	if strings.HasPrefix(l.key, "static:") {
		return l.addr
	}
	return ""
}

// linkOnce: 拨号→握手→敲门。返回 true=已批准保活后被断（重置退避）；
// false=未批准/拨号失败（走退避）。已批准期间阻塞保活直到连接断开。
func (l *linkLoop) linkOnce(addr string) bool {
	n := l.node
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	ch, ident, err := DialChannel(conn, n.sec, n.self)
	if err != nil {
		_ = conn.Close()
		return false // 密钥不匹配等：退避重试
	}
	n.reg.Touch(ident.ID)
	if p, ok := n.reg.ByAddr(addr); ok && (p.Static || p.ID == "") {
		n.reg.LearnedIdent(addr, ident)
	}
	defer func() { _ = ch.Close() }()

	if err := ch.Send(ftPair, mustJSON(pairFrame{ID: n.self.ID, Name: n.self.Name, Port: n.self.Port})); err != nil {
		return false
	}
	t, payload, err := ch.Recv()
	if err != nil || t != ftPairA {
		return false
	}
	var ack pairAckFrame
	if err := json.Unmarshal(payload, &ack); err != nil {
		return false
	}
	n.setLinkState(ident.ID, ack.Approved)
	if !ack.Approved {
		n.logf("[mesh] → %s(%s)：已敲门待对端授权（对端 approve 后自动转正）", ident.Name, ident.ID)
		return true // 敲门成功：不算拨号失败，按未授权间隔重试
	}
	n.logf("[mesh] ↔ %s(%s) 链接建立（对端已授权）", ident.Name, ident.ID)

	// 保活：周期 ping + 读侧探针。对端断链/被撤销掐线时 Recv 立即报错——
	// 不用等下一个 ping 周期才发现（撤销授权要求秒级生效）。
	ping := time.NewTicker(linkPingEvery)
	defer ping.Stop()
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		for {
			if _, _, err := ch.Recv(); err != nil {
				return // pong 之外的任何帧也吞掉：链接信道只承载活性
			}
		}
	}()
	for {
		select {
		case <-n.ctx.Done():
			return true
		case <-l.stop:
			return true
		case <-dead:
			return true
		case <-ping.C:
			if err := ch.Send(ftPing, mustJSON(map[string]int64{"t": time.Now().Unix()})); err != nil {
				return true
			}
		}
	}
}

// setLinkState: 更新"对端是否已授权本节点"认知（Call 快速失败提示）。
func (n *Node) setLinkState(peerID string, authed bool) {
	n.linkMu.Lock()
	n.linkSt[peerID] = linkState{PeerAuthed: authed, LastAck: time.Now()}
	n.linkMu.Unlock()
}

// peerAuthedFresh: 对端授权状态已知且新鲜（2 个重试周期内）。
func (n *Node) peerAuthedFresh(peerID string) (authed, known bool) {
	n.linkMu.Lock()
	defer n.linkMu.Unlock()
	st, ok := n.linkSt[peerID]
	if !ok || time.Since(st.LastAck) > 2*linkRetryUnauthed {
		return false, false
	}
	return st.PeerAuthed, true
}

// stopLink: 按 key 停链接循环。
func (n *Node) stopLink(key string) {
	n.linkMu.Lock()
	l, ok := n.links[key]
	if ok {
		delete(n.links, key)
	}
	n.linkMu.Unlock()
	if ok {
		select {
		case <-l.stop:
		default:
			close(l.stop)
		}
	}
}

// —— ctl 面向的配对操作 ——

// PairInfo: 配对总览（approved/pending/revoked 分组）。
func (n *Node) PairInfo() (approved, pending, revoked []PairEntry) {
	return n.pairs.Overview()
}

// ApprovePeer / RevokePeer: 授权/撤销（选择器 = id 前缀/名字）。撤销立即
// 断开该 peer 的活动连接——闸门只拦新帧，老连接必须掐掉才即时生效。
func (n *Node) ApprovePeer(selector string) (string, error) { return n.pairs.Approve(selector) }
func (n *Node) RevokePeer(selector string) (string, error) {
	id, err := n.pairs.Revoke(selector)
	if err != nil {
		return "", err
	}
	n.dropPeerConns(id)
	return id, nil
}

// gateMessage: Call 侧的快速失败提示——对端明确未授权本节点时不必白拨。
func gateMessage(peerID, peerName, selfID string) string {
	return fmt.Sprintf("对端 %s(%s) 尚未授权本节点（%s）—— 在对端机器执行 serialtap mesh approve %s 后重试",
		peerName, peerID, selfID, selfID)
}
