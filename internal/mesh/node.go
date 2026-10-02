package mesh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
)

// ProxyAPI: 节点为远端隧道提供本地代理端点的能力（由 daemon 实现——
// mesh 不 import daemon，依赖单向）。
type ProxyAPI interface {
	ProxyStart(pattern string) (endpoint, device, key string, err error)
	ProxyStop(pattern string) (n int, err error)
}

// Options: 节点配置与注入缝（测试注 BeaconTargets/NoUDP，生产走广播）。
type Options struct {
	Name        string
	Port        int         // 0 = 随机端口（测试）
	Key         string      // 预共享密钥口令
	AnnounceS   int         // beacon 间隔秒（0 = 5）
	StaticPeers []string    // 静态种子 "host:port"
	Root        string      // 日志根（node id 持久化 + 上传落盘 + 尾随源）
	Forward     ctl.Handler // 本机业务分发（cli 的 handler 闭包）——mesh 是它的第三个前端
	Proxy       ProxyAPI    // 隧道用（daemon 的 ProxyStart/Stop）
	Logf        func(string, ...any)
	// 测试缝
	BeaconTargets func(port int) []string // nil = 定向广播（生产）
	NoUDP         bool                    // 纯 TCP 节点（测试）
}

// Node: mesh 节点 = TCP 服务端 + UDP beacon + 客户端拨号器 + 注册表。
type Node struct {
	opt  Options
	sec  *Secrets
	self Ident
	reg  *Registry

	ln     net.Listener
	udp    *net.UDPConn
	ann    *Announcer
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	connMu sync.Mutex
	closed bool
	conns  map[net.Conn]struct{} // Close 时逐个关闭，解除 serveConn 的 Recv 阻塞（同 ctl.Server #18 纪律）

	mu      sync.Mutex
	uploads map[string]map[string]string // sid → name → 已落盘绝对路径
	dlAuth  map[string]string            // token → 可下载绝对路径（只登记本节点自建产物）
	tunnels map[string]*tunnel           // pattern → 隧道
	callSeq uint64
}

// NewNode: 构造并启动（监听 + beacon + 注册表清扫）。Close 时全停。
func NewNode(opt Options) (*Node, error) {
	if opt.Key == "" {
		return nil, fmt.Errorf("mesh_key 为空：mesh 需要预共享密钥（serialtap mesh keygen 生成后配到所有 PC）")
	}
	sec, err := DeriveSecrets(opt.Key)
	if err != nil {
		return nil, err
	}
	announce := time.Duration(opt.AnnounceS) * time.Second
	if opt.AnnounceS <= 0 {
		announce = 5 * time.Second
	}
	name := opt.Name
	if name == "" {
		if h, herr := os.Hostname(); herr == nil {
			name = h
		} else {
			name = "serialtap"
		}
	}
	n := &Node{
		opt:     opt,
		sec:     sec,
		reg:     NewRegistry(3 * announce),
		uploads: map[string]map[string]string{},
		dlAuth:  map[string]string{},
		tunnels: map[string]*tunnel{},
		conns:   map[net.Conn]struct{}{},
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())

	id, err := loadOrCreateID(opt.Root)
	if err != nil {
		return nil, err
	}
	n.self = Ident{ID: id, Name: name, Port: opt.Port}

	if err := n.start(announce); err != nil {
		n.Close()
		return nil, err
	}
	return n, nil
}

func (n *Node) logf(format string, args ...any) {
	if n.opt.Logf != nil {
		n.opt.Logf(format, args...)
	}
}

func (n *Node) start(announce time.Duration) error {
	var err error
	n.ln, err = net.Listen("tcp", fmt.Sprintf(":%d", n.opt.Port))
	if err != nil {
		return fmt.Errorf("mesh TCP 监听失败（端口 %d）: %w", n.opt.Port, err)
	}
	port := n.ln.Addr().(*net.TCPAddr).Port
	n.self.Port = port

	if !n.opt.NoUDP {
		n.udp, err = ListenBeacons(port, n.self.ID, n.sec, func(b Beacon, from net.Addr) {
			if ua, ok := from.(*net.UDPAddr); ok {
				n.reg.UpsertBeacon(b, ua.IP)
			}
		}, n.logf)
		if err != nil {
			return err
		}
		targets := n.opt.BeaconTargets
		if targets == nil {
			targets = func(port int) []string {
				var out []string
				for _, a := range broadcastAddrs() {
					out = append(out, net.JoinHostPort(a, fmt.Sprintf("%d", port)))
				}
				return out
			}
		}
		self := Beacon{ID: n.self.ID, Name: n.self.Name, FP: n.sec.Fingerprint(n.self.ID)}
		n.ann = NewAnnouncer(self, port, announce, func() []string { return targets(port) }, n.logf)
	}
	n.reg.SetStatic(n.opt.StaticPeers)

	n.wg.Add(2)
	go func() {
		defer n.wg.Done()
		n.acceptLoop()
	}()
	go func() {
		defer n.wg.Done()
		sweep := time.NewTicker(announce)
		defer sweep.Stop()
		for {
			select {
			case <-n.ctx.Done():
				return
			case <-sweep.C:
				for _, id := range n.reg.Expire() {
					n.logf("[mesh] peer %s 过期摘除（连续 %s 未见 beacon）", id, 3*announce)
				}
			}
		}
	}()
	n.logf("[mesh] 节点 %s(%s) 监听 :%d", n.self.Name, n.self.ID, port)
	return nil
}

// Close: 全停（幂等）。
func (n *Node) Close() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	if n.ln != nil {
		_ = n.ln.Close()
	}
	n.connMu.Lock()
	n.closed = true
	for c := range n.conns {
		_ = c.Close()
	}
	n.connMu.Unlock()
	if n.udp != nil {
		_ = n.udp.Close()
	}
	if n.ann != nil {
		n.ann.Close()
	}
	n.mu.Lock()
	for _, t := range n.tunnels {
		t.close()
	}
	n.tunnels = map[string]*tunnel{}
	n.mu.Unlock()
	n.wg.Wait()
	n.cancel = nil
}

// Self: 本节点身份（id/name/port）。
func (n *Node) Self() Ident { return n.self }

// Peers: 注册表快照。
func (n *Node) Peers() []Peer { return n.reg.List() }

// —— 服务端 ——

func (n *Node) acceptLoop() {
	for {
		conn, err := n.ln.Accept()
		if err != nil {
			select {
			case <-n.ctx.Done():
				return
			default:
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}
		n.connMu.Lock()
		if n.closed {
			n.connMu.Unlock()
			_ = conn.Close()
			continue
		}
		n.wg.Add(1)
		n.conns[conn] = struct{}{}
		n.connMu.Unlock()
		go func() {
			defer func() {
				n.connMu.Lock()
				delete(n.conns, conn)
				n.connMu.Unlock()
				n.wg.Done()
			}()
			n.serveConn(conn)
		}()
	}
}

// serveConn: 单连接生命周期 = 握手 → 帧分发。审计纪律：只记命令名/对端/
// 字节数，帧体（可能含 NVS 值）永不落日志。
func (n *Node) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	ch, peer, err := AcceptChannel(conn, n.sec, n.self)
	if err != nil {
		n.logf("[mesh] 拒绝连接 %s: %v", conn.RemoteAddr(), err)
		return
	}
	n.registerIncoming(ch, peer)
	n.logf("[mesh] peer %s(%s) 接入 %s", peer.Name, peer.ID, ch.PeerAddr())
	ctx, cancel := context.WithCancel(n.ctx)
	defer cancel()

	for {
		t, payload, err := ch.Recv()
		if err != nil {
			return
		}
		switch t {
		case ftPing:
			if err := ch.Send(ftPong, payload); err != nil {
				return
			}
		case ftReq:
			if err := n.handleReq(ch, payload); err != nil {
				return
			}
		case ftUp:
			if err := n.handleUpload(ch, payload); err != nil {
				return
			}
		case ftDown:
			if err := n.handleDownload(ch, ctx, payload); err != nil {
				return
			}
		case ftTail:
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				_ = n.handleTail(ch, ctx, payload)
			}()
		case ftDial:
			n.handleDial(ch, payload) // 占用连接直到隧道结束
			return
		}
	}
}

// registerIncoming: 来话握手成功 → 刷新注册表（beacon 学过的 Touch；
// 静态种子的按源 IP 回填身份；未知节点按自报端口登记——对端 beacon 我们
// 可能还没收到，先能按名找到它）。
func (n *Node) registerIncoming(ch *Channel, peer Ident) {
	host, _, err := net.SplitHostPort(ch.PeerAddr())
	if err != nil {
		host = ch.PeerAddr()
	}
	n.reg.Touch(peer.ID)
	for _, sp := range n.reg.List() {
		if h, _, serr := net.SplitHostPort(sp.Addr); serr == nil && h == host {
			n.reg.LearnedIdent(sp.Addr, peer)
		}
	}
	if peer.Port > 0 {
		addr := net.JoinHostPort(host, fmt.Sprintf("%d", peer.Port))
		if _, ok := n.reg.ByAddr(addr); !ok {
			n.reg.LearnedIdent(addr, peer)
		}
	}
}

// reqFrame/respFrame: 请求/响应信封（v1 一连接一请求，id 恒为 1；留字段供演进）。
type reqFrame struct {
	ID  uint64      `json:"id"`
	Req ctl.Request `json:"req"`
}
type respFrame struct {
	ID   uint64        `json:"id"`
	Resp *ctl.Response `json:"resp,omitempty"`
	End  bool          `json:"end"`
	File *fileRef      `json:"file,omitempty"` // board dump 等产物（token 限定本节点自建产物）
}
type fileRef struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
}

// handleReq: 转发进本机 Forward（respond 多次=流式），handler 返回后发 End 帧。
// board dump 的 OutPath 为空 = mesh 远端模式：产物落在 .mesh-share/<随机>/，
// 随 End 帧下发一次性下载 token（下载只认 token 登记过的路径——持钥者不能
// 借此读 daemon 用户的任意文件）。
func (n *Node) handleReq(ch *Channel, payload []byte) error {
	var rf reqFrame
	if err := json.Unmarshal(payload, &rf); err != nil {
		resp := ctl.Response{OK: false, Error: "mesh 请求解析失败"}
		return ch.Send(ftResp, mustJSON(respFrame{ID: 1, Resp: &resp, End: true}))
	}
	n.logf("[mesh] 转发本机执行: cmd=%s peer=%s", rf.Req.Cmd, ch.PeerAddr())

	req := rf.Req
	var file *fileRef
	cleanup := func() {}
	if req.Cmd == "board" && req.Board != nil && req.Board.Action == "dump" && req.Board.OutPath == "" {
		dir := filepath.Join(n.opt.Root, ".mesh-share", randomHex(8))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			resp := ctl.Response{OK: false, Error: "mesh 产物目录创建失败: " + err.Error()}
			return ch.Send(ftResp, mustJSON(respFrame{ID: rf.ID, Resp: &resp, End: true}))
		}
		p := filepath.Join(dir, "dump.bin")
		req.Board.OutPath = p
		cleanup = func() {
			fi, serr := os.Stat(p)
			if serr != nil {
				_ = os.RemoveAll(dir)
				return
			}
			token := randomHex(16)
			n.mu.Lock()
			n.dlAuth[token] = p
			n.mu.Unlock()
			file = &fileRef{Token: token, Name: filepath.Base(p), Size: fi.Size()}
		}
	}
	n.opt.Forward(req, func(r ctl.Response) {
		_ = ch.Send(ftResp, mustJSON(respFrame{ID: rf.ID, Resp: &r}))
	})
	cleanup()
	return ch.Send(ftResp, mustJSON(respFrame{ID: rf.ID, End: true, File: file}))
}

// —— 客户端 ——

// Call: 向 peer 转发一条 ctl 请求，流式回收响应直到 End 帧（或 onResp
// 返回 true 提前结束）。返回 End 帧携带的产物引用（通常为 nil）。
func (n *Node) Call(peerName string, req ctl.Request, onResp func(ctl.Response) bool) (*fileRef, error) {
	peer, err := n.reg.Match(peerName)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", peer.Addr, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连不上 peer %s(%s): %w", peer.NameOrAddr(), peer.Addr, err)
	}
	defer func() { _ = conn.Close() }()
	ch, ident, err := DialChannel(conn, n.sec, n.self)
	if err != nil {
		return nil, fmt.Errorf("与 peer %s 握手失败（密钥不一致？）: %w", peerName, err)
	}
	n.reg.Touch(ident.ID)
	if peer.Static || peer.ID == "" {
		n.reg.LearnedIdent(peer.Addr, ident)
	}

	n.mu.Lock()
	n.callSeq++
	id := n.callSeq
	n.mu.Unlock()
	if err := ch.Send(ftReq, mustJSON(reqFrame{ID: id, Req: req})); err != nil {
		return nil, err
	}
	n.logf("[mesh] → peer %s: cmd=%s", ident.Name, req.Cmd)
	for {
		t, payload, rerr := ch.Recv()
		if rerr != nil {
			return nil, fmt.Errorf("peer %s 响应中断: %w", ident.Name, rerr)
		}
		if t != ftResp {
			continue
		}
		var rf respFrame
		if jerr := json.Unmarshal(payload, &rf); jerr != nil || rf.ID != id {
			continue
		}
		if rf.Resp != nil && onResp != nil && onResp(*rf.Resp) {
			return rf.File, nil
		}
		if rf.End {
			return rf.File, nil
		}

	}
}

// Forward: ctl handler 的 peer 分支入口——CLI/web 带 peer 字段的请求都走这。
// flash 在这拦截做镜像上传；board dump 拦截做产物取回；其余原样转发。
func (n *Node) Forward(peerName string, req ctl.Request, respond func(ctl.Response)) error {
	switch req.Cmd {
	case "flash":
		return n.remoteFlash(peerName, req, respond)
	case "board":
		if req.Board != nil && req.Board.Action == "dump" && req.Board.OutPath != "" {
			return n.remoteBoardDump(peerName, req, respond)
		}
	}
	_, err := n.Call(peerName, req, func(r ctl.Response) bool {
		respond(r)
		return false
	})
	return err
}

// AggregateStatus: 并行查询全部 peer 的设备状态（本机设备由调用方另行取）。
// 每 peer 独立限时，失败/超时记入 Err/State，不拖垮整体。
func (n *Node) AggregateStatus(timeout time.Duration) []ctl.PeerStatus {
	peers := n.reg.List()
	out := make([]ctl.PeerStatus, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p Peer) {
			defer wg.Done()
			ps := ctl.PeerStatus{ID: p.ID, Name: p.NameOrAddr(), Addr: p.Addr, Static: p.Static}
			type res struct {
				devs []ctl.DevState
				err  error
			}
			resCh := make(chan res, 1)
			go func() {
				devs := []ctl.DevState{}
				_, cerr := n.Call(p.NameOrAddr(), ctl.Request{Cmd: "status"}, func(r ctl.Response) bool {
					if r.OK {
						devs = r.Devices
					} else if r.Error != "" {
						ps.Err = r.Error
					}
					return false
				})
				resCh <- res{devs: devs, err: cerr}
			}()
			start := time.Now()
			select {
			case r := <-resCh:
				ps.LatencyMs = time.Since(start).Milliseconds()
				if r.err != nil {
					ps.State, ps.Err = "offline", r.err.Error()
				} else {
					ps.State, ps.Devices = "online", r.devs
				}
			case <-time.After(timeout):
				ps.State, ps.Err = "offline", fmt.Sprintf("查询超时（>%s）", timeout)
			}
			out[i] = ps
		}(i, p)
	}
	wg.Wait()
	// 按名稳定排序
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// NameOrAddr: 展示名（未学到身份的静态占位用地址）。
func (p Peer) NameOrAddr() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Addr
}

// —— 小工具 ——

func loadOrCreateID(root string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(root, ".mesh-node-id")
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) == 8 {
			return id, nil
		}
	}
	id := randomHex(4)
	if err := os.WriteFile(p, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
