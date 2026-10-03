package mesh

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Peer: 注册表里的一个远端节点。
type Peer struct {
	ID       string // 空 = 静态种子（还没握上手）
	Name     string
	Addr     string    // "host:port"（TCP 拨号用）
	Static   bool      // 配置里的静态种子（不过期）
	LastSeen time.Time // beacon 最近一次（静态种子 = 最近一次成功握手）
}

// IsFresh: 广播学到的 peer 在 3 个广播间隔内见过才算在线（静态种子恒真）。
func (p Peer) IsFresh(ttl time.Duration) bool {
	return p.Static || time.Since(p.LastSeen) < ttl
}

// Registry: peer 注册表。来源两路：beacon（UpsertBeacon，带指纹校验后的
// 回调进来）与静态种子（SetStatic，Addr 拨号成功握手后补身份）。
type Registry struct {
	mu    sync.Mutex
	peers map[string]*Peer // key = ID（静态种子握手前用 "static:"+Addr 占位）
	ttl   time.Duration
}

func NewRegistry(ttl time.Duration) *Registry {
	return &Registry{peers: map[string]*Peer{}, ttl: ttl}
}

// UpsertBeacon: 由 beacon 学到/刷新 peer。srcIP 来自 UDP 包源地址（比
// beacon 里的自报更可信——自报可能没填）。
func (r *Registry) UpsertBeacon(b Beacon, srcIP net.IP) {
	addr := net.JoinHostPort(srcIP.String(), fmt.Sprintf("%d", b.Port))
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.peers[b.ID]; ok {
		p.Name, p.Addr, p.LastSeen = b.Name, addr, time.Now()
		return
	}
	r.peers[b.ID] = &Peer{ID: b.ID, Name: b.Name, Addr: addr, LastSeen: time.Now()}
}

// UpsertStatic: 静态种子合并（配置 mesh_peers）。已握上手的静态项（有
// 真实 ID）保留身份只刷新 Addr；未握手的以 "static:<addr>" 占位。
func (r *Registry) SetStatic(addrs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		seen[a] = true
		var existing *Peer
		for _, p := range r.peers {
			if p.Addr == a {
				existing = p
				break
			}
		}
		if existing != nil {
			existing.Static = true
			continue
		}
		key := "static:" + a
		if p, ok := r.peers[key]; ok {
			p.Static = true
			continue
		}
		r.peers[key] = &Peer{Addr: a, Static: true}
	}
	// 撤销不再配置的静态标记：beacon 学到的不删（只去掉 Static）；
	// 未握手的占位条目（key = "static:<addr>"）直接摘除
	for key, p := range r.peers {
		if !p.Static {
			continue
		}
		if strings.HasPrefix(key, "static:") {
			if !seen[p.Addr] {
				delete(r.peers, key)
			}
		} else if !seen[p.Addr] {
			p.Static = false
		}
	}
}

// LearnedIdent: 静态种子握上手后回填身份（从占位 key 迁到真实 ID）。
func (r *Registry) LearnedIdent(addr string, id Ident) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := "static:" + addr
	if p, ok := r.peers[key]; ok {
		delete(r.peers, key)
		p.ID, p.Name, p.LastSeen = id.ID, id.Name, time.Now()
		r.peers[id.ID] = p
		return
	}
	if p, ok := r.peers[id.ID]; ok {
		p.Name, p.Addr, p.LastSeen = id.Name, addr, time.Now()
		return
	}
	r.peers[id.ID] = &Peer{ID: id.ID, Name: id.Name, Addr: addr, LastSeen: time.Now(), Static: true}
}

// Touch: 已知 peer 刷新活跃时间（拨号成功时）。
func (r *Registry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.peers[id]; ok {
		p.LastSeen = time.Now()
	}
}

// Expire: 摘除过期的 beacon peer（静态种子与握手过的静态项保留）。
// 返回被摘除的 ID（日志用）。
func (r *Registry) Expire() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []string
	for id, p := range r.peers {
		if p.Static {
			continue
		}
		if time.Since(p.LastSeen) >= r.ttl {
			delete(r.peers, id)
			removed = append(removed, id)
		}
	}
	return removed
}

// List: 全量快照（按 Name 排序，静态占位用 Addr 排）。
func (r *Registry) List() []Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Peer, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		ki, kj := out[i].Name, out[j].Name
		if ki == "" {
			ki = out[i].Addr
		}
		if kj == "" {
			kj = out[j].Addr
		}
		return ki < kj
	})
	return out
}

// Match: 按名字/ID 精确或唯一前缀匹配（CLI --peer 的容错）；也接受
// "host:port" 直接按地址匹配（静态种子未握手时唯一可达方式）。
// 匹配不上或歧义时报错并列出已知 peer —— 错误信息就是排障指引。
func (r *Registry) Match(prefix string) (Peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prefix == "" {
		return Peer{}, fmt.Errorf("缺少 peer 名")
	}
	var hits []Peer
	for _, p := range r.peers {
		if p.Addr == prefix || p.Name == prefix || p.ID == prefix ||
			strings.HasPrefix(p.Name, prefix) || strings.HasPrefix(p.ID, prefix) {
			hits = append(hits, *p)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.Name + "(" + h.ID + ")"
		}
		return Peer{}, fmt.Errorf("peer 前缀 %q 歧义: %s", prefix, strings.Join(names, ", "))
	}
	// 没有注册表命中但长得像地址 → 直接当地址拨（不进注册表的临时 peer）
	if _, _, err := net.SplitHostPort(prefix); err == nil {
		return Peer{Addr: prefix}, nil
	}
	return Peer{}, fmt.Errorf("未找到 peer %q（serialtap mesh status 查看已知节点）", prefix)
}

// ByKey: 按内部键查（链接循环用：beacon peer 键=id，静态占位键="static:addr"）。
func (r *Registry) ByKey(key string) (Peer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.peers[key]; ok {
		return *p, true
	}
	return Peer{}, false
}

// ByAddr: 直接按地址找（转发兜底：peer 名填了 host:port 也能走）。
func (r *Registry) ByAddr(addr string) (Peer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.peers {
		if p.Addr == addr {
			return *p, true
		}
	}
	return Peer{}, false
}
