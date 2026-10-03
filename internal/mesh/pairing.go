package mesh

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 配对授权：mesh 的第二道门。第一道是 PSK（能解开帧=持钥），第二道是
// 被链接侧对来话节点 id 的显式批准——持钥但未授权的 peer 只能敲门
// （ftPair），一切操作帧（req/upload/download/tail/dial）都被拒。
// 授权表持久化在 <root>/.mesh-peers.json，守护进程单一写者。

// PairEntry: 单个 peer 的授权记录。
type PairEntry struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Addr string    `json:"addr"` // 最近一次敲门的来源
	At   time.Time `json:"at"`   // 最近一次敲门/批准时间
}

// pairFile: 持久化形状。
type pairFile struct {
	Approved map[string]PairEntry `json:"approved"`
	Pending  map[string]PairEntry `json:"pending"`
	Revoked  map[string]PairEntry `json:"revoked"` // 显式拒绝过的（不再弹提示，敲门前直接拒）
}

// PairStore: 授权表（线程安全；文件是唯一持久层，内存为工作集）。
type PairStore struct {
	mu   sync.Mutex
	path string
	f    pairFile
}

// LoadPairStore: 读 <root>/.mesh-peers.json（不存在 = 空表）。
func LoadPairStore(root string) (*PairStore, error) {
	st := &PairStore{path: filepath.Join(root, ".mesh-peers.json"), f: emptyPairFile()}
	data, err := os.ReadFile(st.path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &st.f); err != nil {
		return nil, fmt.Errorf("mesh 授权表损坏（%s）: %w", st.path, err)
	}
	if st.f.Approved == nil {
		st.f.Approved = map[string]PairEntry{}
	}
	if st.f.Pending == nil {
		st.f.Pending = map[string]PairEntry{}
	}
	if st.f.Revoked == nil {
		st.f.Revoked = map[string]PairEntry{}
	}
	return st, nil
}

func emptyPairFile() pairFile {
	return pairFile{Approved: map[string]PairEntry{}, Pending: map[string]PairEntry{}, Revoked: map[string]PairEntry{}}
}

func (s *PairStore) saveLocked() error {
	data, err := json.MarshalIndent(&s.f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Knock: 敲门（来话握手成功即调用）。已批准→true；已拒绝→false 不改状态；
// 否则记 pending 并返回 false。autoApprove=true 时直接批准（PSK-only 旧行为）。
func (s *PairStore) Knock(id, name, addr string, autoApprove bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if _, ok := s.f.Approved[id]; ok {
		// 刷新展示信息（不改授权）
		p := s.f.Approved[id]
		p.Name, p.Addr, p.At = name, addr, now
		s.f.Approved[id] = p
		return true
	}
	if _, ok := s.f.Revoked[id]; ok {
		p := s.f.Revoked[id]
		p.Name, p.Addr, p.At = name, addr, now
		s.f.Revoked[id] = p
		return false
	}
	if autoApprove {
		s.f.Approved[id] = PairEntry{ID: id, Name: name, Addr: addr, At: now}
		_ = s.saveLocked()
		return true
	}
	s.f.Pending[id] = PairEntry{ID: id, Name: name, Addr: addr, At: now}
	_ = s.saveLocked()
	return false
}

// Approved: 授权判定（操作帧的闸门）。
func (s *PairStore) Approved(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.f.Approved[id]
	return ok
}

// Approve: 批准一个 pending/任意已知 id（按 id 唯一前缀或名字匹配）。
// 返回批准的 id。
func (s *PairStore) Approve(selector string) (string, error) {
	id, err := s.resolve(selector)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.f.Pending[id]
	if !ok {
		st = s.f.Revoked[id] // 从拒绝名单捞回来
	}
	if !ok {
		st = PairEntry{ID: id, At: time.Now()}
	}
	st.At = time.Now()
	s.f.Approved[id] = st
	delete(s.f.Pending, id)
	delete(s.f.Revoked, id)
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return id, nil
}

// Revoke: 撤销授权（批准→拒绝名单；pending→拒绝名单）。
func (s *PairStore) Revoke(selector string) (string, error) {
	id, err := s.resolve(selector)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.f.Pending[id]
	if st.ID == "" {
		st = s.f.Approved[id]
	}
	if st.ID == "" {
		return "", fmt.Errorf("未知 peer %q", selector)
	}
	st.At = time.Now()
	s.f.Revoked[id] = st
	delete(s.f.Approved, id)
	delete(s.f.Pending, id)
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return id, nil
}

// resolve: 选择器（id 唯一前缀 / 名字精确 / 名字唯一前缀）在全部已知
// （approved+pending+revoked）里找。
func (s *PairStore) resolve(selector string) (string, error) {
	if selector == "" {
		return "", fmt.Errorf("缺少 peer 选择器")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := map[string]PairEntry{}
	for k, v := range s.f.Approved {
		all[k] = v
	}
	for k, v := range s.f.Pending {
		all[k] = v
	}
	for k, v := range s.f.Revoked {
		all[k] = v
	}
	var hits []string
	for id, st := range all {
		if id == selector || st.Name == selector || hasPrefix(id, selector) || hasPrefix(st.Name, selector) {
			hits = append(hits, id)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return "", fmt.Errorf("选择器 %q 歧义（%d 个候选，请用完整 id）", selector, len(hits))
	}
	return "", fmt.Errorf("未知 peer %q（mesh pair 查看待授权/已授权名单）", selector)
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// Overview: 全部授权状态快照（展示用，按 Approved→Pending→Revoked 分组）。
func (s *PairStore) Overview() (approved, pending, revoked []PairEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.f.Approved {
		approved = append(approved, v)
	}
	for _, v := range s.f.Pending {
		pending = append(pending, v)
	}
	for _, v := range s.f.Revoked {
		revoked = append(revoked, v)
	}
	sortBy := func(l []PairEntry) { sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name }) }
	sortBy(approved)
	sortBy(pending)
	sortBy(revoked)
	return
}

// —— 配对帧 ——

// pairFrame: ftPair 密封载荷（链接发起侧→被链侧）。链接即心跳：批准后
// 连接保持，ftPing 保活；未批准则发起侧重试敲门（默认 30s）。
type pairFrame struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port"`
}
type pairAckFrame struct {
	ID       string `json:"id"` // 被链侧自报（发起侧登记用）
	Name     string `json:"name"`
	Approved bool   `json:"approved"`       // 被链侧是否已授权发起侧
	Auto     bool   `json:"auto,omitempty"` // 被链侧开着 auto_approve
}

// linkState: 发起侧对一个 peer 的链接认知（Call 快速失败提示用）。
type linkState struct {
	PeerAuthed bool // 对端已授权本节点（最近一次 pair ack）
	LastAck    time.Time
}

// handlePair: 被链侧——敲门/链接帧。授权判定 + 登记 pending。
func (n *Node) handlePair(ch *Channel, peer Ident, payload []byte) error {
	var pf pairFrame
	if err := json.Unmarshal(payload, &pf); err != nil || pf.ID == "" {
		return ch.Send(ftPairA, mustJSON(pairAckFrame{ID: n.self.ID, Name: n.self.Name}))
	}
	approved := n.pairs.Knock(pf.ID, peer.Name, ch.PeerAddr(), n.opt.AutoApprove)
	if !approved {
		n.logf("[mesh] 配对请求: %s(%s) @%s —— serialtap mesh approve %s 批准", peer.Name, pf.ID, ch.PeerAddr(), pf.ID)
	}
	return ch.Send(ftPairA, mustJSON(pairAckFrame{ID: n.self.ID, Name: n.self.Name, Approved: approved, Auto: n.opt.AutoApprove}))
}

// trackPairConn / untrackPairConn: 按-peer 活动连接登记（撤销授权即断链用）。
func (n *Node) trackPairConn(peerID string, conn net.Conn) {
	n.pairConnMu.Lock()
	defer n.pairConnMu.Unlock()
	if n.pairConns[peerID] == nil {
		n.pairConns[peerID] = map[net.Conn]struct{}{}
	}
	n.pairConns[peerID][conn] = struct{}{}
}

func (n *Node) untrackPairConn(peerID string, conn net.Conn) {
	n.pairConnMu.Lock()
	defer n.pairConnMu.Unlock()
	if m := n.pairConns[peerID]; m != nil {
		delete(m, conn)
		if len(m) == 0 {
			delete(n.pairConns, peerID)
		}
	}
}

// dropPeerConns: 断开某 peer 的全部活动连接（撤销授权时立即生效——对端
// 链接循环会自动重敲并感知被拒）。
func (n *Node) dropPeerConns(peerID string) {
	n.pairConnMu.Lock()
	conns := make([]net.Conn, 0, len(n.pairConns[peerID]))
	for c := range n.pairConns[peerID] {
		conns = append(conns, c)
	}
	n.pairConnMu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}
