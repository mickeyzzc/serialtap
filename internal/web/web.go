// Package web 实现 serialtap 的 Web 观测面板：设备接入/采集状态、实时串口
// 日志尾随（SSE）、事件流（签名命中 + 生命周期）、日志文件浏览、暂停/恢复/
// 代理操作。只读状态来自 daemon.Status() 与日志目录本身；操作经注入的
// commander 复用 ctl 通道的同一条处理路径 —— 面板不引入第二套控制逻辑，
// 也不引入任何业务判断（serialtap 定位不变）。
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/flash"
)

//go:embed index.html
var indexHTML []byte

// Commander: 单响应命令通道（status/pause/resume/proxy），由 CLI 层桥接到
// ctl 处理函数。flash/release 等流式/长操作不进面板（走 CLI）。
type Commander func(ctl.Request) (ctl.Response, error)

// StatusProvider: 当前设备状态（daemon.Status）。
type StatusProvider func() []ctl.DevState

// Flasher: 代理刷固件执行体（即 daemon.Flash —— 面板经 Web 上传镜像后调用）。
type Flasher func(pattern string, all bool, spec flash.Spec, out func(line string)) error

// Option: Start 的可选项（保持既有调用点签名不变）。
type Option func(*Server)

// WithFlasher: 启用面板刷机（上传镜像 → 代理刷写 → SSE 进度流）。
func WithFlasher(f Flasher) Option { return func(s *Server) { s.flasher = f } }

// Server: 面板 HTTP 服务（默认只听本机回环）。
type Server struct {
	root        string
	status      StatusProvider
	commander   Commander
	flasher     Flasher
	mesh        MeshService // nil = mesh 未启用（聚合/远程尾随隐藏）
	meshFlasher MeshFlasher // nil = 面板远程刷机不可用
	job         flashJob    // 当前/最近一次刷机任务（单任务槽，opMu 天然串行）
	srv         *http.Server
}

// PickAddr: 归一化面板地址。空/缺省 → 默认本机回环端口；"off" → ""（关闭）。
func PickAddr(v string) string {
	if v == "" {
		return "127.0.0.1:8801"
	}
	if v == "off" || v == "disabled" {
		return ""
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		return "127.0.0.1:8801"
	}
	return v
}

// Start: 起面板（addr 为空 = 关闭，返回 no-op Server）。logf 仅报告启动/退出。
func Start(addr, root string, status StatusProvider, commander Commander,
	logf func(string, ...any), opts ...Option) *Server {
	addr = PickAddr(addr)
	s := &Server{root: root, status: status, commander: commander}
	s.job.subs = map[chan flashEvent]bool{}
	for _, o := range opts {
		o(s)
	}
	if addr == "" {
		return s
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/cmd", s.handleCmd)
	mux.HandleFunc("/api/flash", s.handleFlash)              // POST 上传镜像并启动刷写
	mux.HandleFunc("/api/flash/stream", s.flashStream)       // SSE 进度（历史回放+实时）
	mux.HandleFunc("/api/devices/", s.handleDevice)          // {name}/files|tail|live
	mux.HandleFunc("/api/live", s.handleLiveMulti)           // 单条 SSE 多路设备尾随（浏览器连接池友好）
	mux.HandleFunc("/api/mesh/status", s.handleMeshStatus)   // mesh 聚合（未启用时 enabled:false）
	mux.HandleFunc("/api/mesh/devices/", s.handleMeshDevice) // {peer}/{name}/live（远程 SSE 尾随）
	s.srv = &http.Server{Addr: addr, Handler: mux}
	go func() {
		logf("[web] 观测面板: http://%s/", addr)
		if !isLoopbackBind(addr) {
			// 非回环绑定 = 面板全部操作（暂停/刷机/命令/尾随）暴露给 LAN 内
			// 任意主机且无认证 —— 只应在可信网络（如 WPA2 家庭内网）使用
			logf("[web] ⚠ 监听非回环地址 %s —— 局域网内任意主机可访问面板全部操作（无认证），请确认网络可信", addr)
		}
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("[web] 面板退出: %v", err)
		}
	}()
	return s
}

// isLoopbackBind: 监听地址是否仅回环（host 为空 = 全接口，算非回环）。
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false // ":8801" 形态 = 全接口
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() || host == "localhost"
}

// Close: 停止服务（零值安全）。
func (s *Server) Close() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // 面板升级后浏览器别再端出旧页面
	_, _ = w.Write(indexHTML)
}

// —— 设备名安全：面板 URL 里的 name 必须是目录安全名（SanitizeName 产物）——

var safeNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (s *Server) deviceDir(name string) (string, bool) {
	if !safeNameRe.MatchString(name) || strings.Contains(name, "..") {
		return "", false
	}
	dir := filepath.Join(s.root, name)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", false
	}
	return dir, true
}

// logSortKey: kind-YYYYMMDD[.NNN].log → (day, suffix)。基础文件 suffix=0。
// 字典序在此不可靠：".001" < ".log"（'0' < 'l'），轮转后会把旧基础文件当最新，
// 必须按 (日期字符串, 数字后缀) 语义比较。
func logSortKey(kind, name string) (string, int, bool) {
	n := strings.TrimPrefix(name, kind+"-")
	if !strings.HasSuffix(n, ".log") {
		return "", 0, false
	}
	n = strings.TrimSuffix(n, ".log")
	if i := strings.IndexByte(n, '.'); i >= 0 {
		sfx, err := strconv.Atoi(n[i+1:])
		if err != nil || sfx < 0 {
			return "", 0, false
		}
		return n[:i], sfx, true
	}
	return n, 0, true
}

// latestFile: dir 下 kind-YYYYMMDD[.NNN].log 的最新者（轮转语义：
// 同日内后缀越大越新 —— 写满基础文件后写入 .001/.002…）。
// 注意 size 不能取目录枚举的 Info()：Windows 对正在写入的文件返回滞后的
// 缓存大小（可数分钟不动），SSE 靠它差分判断新数据会被饿死 —— 获胜者
// 单独 os.Stat 取真实大小（按路径 Stat 是准的，已实测）。
func latestFile(dir, kind string) (string, int64, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, false
	}
	best, bestDay, bestSfx := "", "", -1
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, kind+"-") || !strings.HasSuffix(n, ".log") {
			continue
		}
		day, sfx, ok := logSortKey(kind, n)
		if !ok {
			continue
		}
		if day > bestDay || (day == bestDay && sfx > bestSfx) {
			best, bestDay, bestSfx = n, day, sfx
		}
	}
	if best == "" {
		return "", 0, false
	}
	fi, err := os.Stat(filepath.Join(dir, best))
	if err != nil {
		return best, 0, true
	}
	return best, fi.Size(), true
}

// deviceDTO: 面板设备载荷 = ctl.DevState + 日志目录富化（最新文件与大小，
// 前端用大小差分算写入速率）。
type deviceDTO struct {
	Name          string `json:"name"`
	Tty           string `json:"tty"`
	Key           string `json:"key"`
	State         string `json:"state"`
	Proxy         string `json:"proxy,omitempty"`          // 附加中的客户端地址
	ProxyEndpoint string `json:"proxy_endpoint,omitempty"` // 透传监听端点（空 = 未开）

	SerialFile  string `json:"serial_file,omitempty"`
	SerialSize  int64  `json:"serial_size"`
	EventsFile  string `json:"events_file,omitempty"`
	EventsSize  int64  `json:"events_size"`
	SerialBytes int64  `json:"serial_bytes"` // 设备目录全量日志总字节（估留存）

	Opens    int64 `json:"opens,omitempty"`     // 成功 open 次数（健康：1 = 从未断线重开）
	LastData int64 `json:"last_data,omitempty"` // 最近读到字节的 UnixMilli（0 = 尚无数据）
}

type snapshotDTO struct {
	Ts      int64       `json:"ts"`
	Root    string      `json:"root"`
	Devices []deviceDTO `json:"devices"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	dto := snapshotDTO{Ts: time.Now().UnixMilli(), Root: s.root, Devices: []deviceDTO{}}
	var devs []ctl.DevState
	if s.status != nil {
		devs = s.status()
	}
	for _, d := range devs {
		e := deviceDTO{Name: d.Name, Tty: d.Tty, Key: d.Key, State: d.State,
			Proxy: d.Proxy, ProxyEndpoint: d.ProxyEndpoint, Opens: d.Opens, LastData: d.LastData}
		if dir, ok := s.deviceDir(d.Name); ok {
			if f, sz, ok := latestFile(dir, "serial"); ok {
				e.SerialFile, e.SerialSize = f, sz
			}
			if f, sz, ok := latestFile(dir, "events"); ok {
				e.EventsFile, e.EventsSize = f, sz
			}
			e.SerialBytes = dirBytes(dir)
		}
		dto.Devices = append(dto.Devices, e)
	}
	writeJSON(w, dto)
}

// dirBytes: 目录下全部 .log 字节数（浏览参考值）。
func dirBytes(dir string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// fileEntry: 文件浏览项。
type fileEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime_ms"`
}

// handleDevice: /api/devices/{name}/(files|tail|live)。kind=serial|events。
func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/devices/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	name, action := parts[0], parts[1]
	dir, ok := s.deviceDir(name)
	if !ok {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	switch action {
	case "files":
		s.serveFiles(w, dir)
	case "tail":
		s.serveTail(w, r, dir)
	case "live":
		s.serveLive(w, r, dir)
	default:
		http.Error(w, "bad action", http.StatusBadRequest)
	}
}

func (s *Server) serveFiles(w http.ResponseWriter, dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := []fileEntry{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		fe := fileEntry{Name: e.Name()}
		// 同 latestFile：目录枚举的 Info() 在 Windows 上大小滞后，按路径 Stat
		if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
			fe.Size, fe.MTime = fi.Size(), fi.ModTime().UnixMilli()
		}
		out = append(out, fe)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	writeJSON(w, out)
}

// tailBytes: 读文件末尾 max 字节并对齐到行首（跨轮转边界的半个头行丢弃）。
func tailBytes(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	start := size - max
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, size-start)
	n, err := f.Read(buf)
	if err != nil {
		return nil, err
	}
	buf = buf[:n]
	if start > 0 { // 丢弃不完整的首行
		if i := indexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return buf, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func (s *Server) serveTail(w http.ResponseWriter, r *http.Request, dir string) {
	kind := r.URL.Query().Get("kind")
	if kind != "serial" && kind != "events" {
		kind = "serial"
	}
	name, _, ok := latestFile(dir, kind)
	if !ok {
		http.Error(w, "no log yet", http.StatusNoContent)
		return
	}
	max := int64(64 * 1024)
	if v := r.URL.Query().Get("bytes"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 4<<20 {
			max = n
		}
	}
	b, err := tailBytes(filepath.Join(dir, name), max)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

// serveLive: SSE 实时尾随。先发末尾 16KB，之后每 700ms 增量推送；
// 轮转/跨天（最新文件名变化或尺寸回缩）时重发末尾对齐。payload 为 JSON
// 字符串（内嵌换行安全），事件名 data。
func (s *Server) serveLive(w http.ResponseWriter, r *http.Request, dir string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "serial" && kind != "events" {
		kind = "serial"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 禁代理/中间层响应缓冲
	send := func(b []byte) bool {
		if len(b) == 0 {
			return true
		}
		payload, _ := json.Marshal(string(b))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	// SSE 心跳：文件长时间无新增时每 15s 发一行注释，(a) 探活中间层，
	// (b) 防止空闲连接被杀。注释帧（": ping"）不触发浏览器 onmessage，零噪音。
	heart := time.NewTicker(15 * time.Second)
	defer heart.Stop()
	ping := func() bool {
		if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !ping() { // 响应头后立刻首帧，逼出缓冲路径的 early flush
		return
	}
	tailLiveDir(r.Context(), dir, kind, send)
}

// tailLiveDir: 单设备日志目录的文件尾随循环（serveLive 与 handleLiveMulti
// 共用）。emit 返回 false（客户端断开/写失败）即退出。
func tailLiveDir(ctx context.Context, dir, kind string, emit func([]byte) bool) {
	const initBytes = 16 * 1024
	curName, curSize, has := latestFile(dir, kind)
	if has {
		b, err := tailBytes(filepath.Join(dir, curName), initBytes)
		if err == nil && !emit(b) {
			return
		}
	}
	tick := time.NewTicker(700 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			name, size, ok := latestFile(dir, kind)
			if !ok {
				continue
			}
			if !has || name != curName || size < curSize { // 轮转/跨天 → 重发末尾
				b, err := tailBytes(filepath.Join(dir, name), initBytes)
				if err != nil || !emit(b) {
					return
				}
				curName, curSize, has = name, size, true
				continue
			}
			if size == curSize {
				continue
			}
			b, err := readRange(filepath.Join(dir, name), curSize, size)
			if err != nil {
				continue
			}
			if !emit(b) {
				return
			}
			curSize = size
		}
	}
}

// liveFrame: /api/live 的载荷 —— 设备键 + 日志块。
type liveFrame struct {
	Dev   string `json:"dev"`
	Chunk string `json:"chunk"`
}

// handleLiveMulti: GET /api/live?kind=serial&devices=a,b,peer/c —— 单条 SSE
// 复用多路设备尾随。浏览器对同一 HTTP/1.1 host 只有 ~6 条并发连接，每设备
// 一条常开 SSE 会把连接池吃光（多台板子时页面卡死、第二开面板连首页都进不
// 来）；合成一条后连接数与设备数无关。帧格式 {"dev":"键","chunk":"..."}；
// 本地设备读日志目录，"peer/名" 复合键走 mesh 桥。设备集在连接时固定，
// 前端在设备增减时重连（走自身重连逻辑，成本是各设备 16KB 末尾回放）。
func (s *Server) handleLiveMulti(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "serial" && kind != "events" {
		kind = "serial"
	}
	type remoteKey struct{ peer, name string }
	var locals []string
	var remotes []remoteKey
	for _, dev := range strings.Split(r.URL.Query().Get("devices"), ",") {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			continue
		}
		if i := strings.Index(dev, "/"); i > 0 && s.mesh != nil {
			peer, name := dev[:i], dev[i+1:]
			if meshPeerRe.MatchString(peer) && safeNameRe.MatchString(name) {
				remotes = append(remotes, remoteKey{peer, name})
				continue
			}
		}
		if dir, ok := s.deviceDir(dev); ok { // 已过滤不存在/不安全名
			locals = append(locals, dev)
			_ = dir
		}
	}
	if len(locals) == 0 && len(remotes) == 0 {
		http.Error(w, "no known devices", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	var mu sync.Mutex
	send := func(dev string, b []byte) bool {
		if len(b) == 0 {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		payload, _ := json.Marshal(liveFrame{Dev: dev, Chunk: string(b)})
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	heart := time.NewTicker(15 * time.Second)
	defer heart.Stop()
	ping := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !ping() {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var wg sync.WaitGroup
	for _, name := range locals {
		dir, _ := s.deviceDir(name)
		wg.Add(1)
		go func(name, dir string) {
			defer wg.Done()
			tailLiveDir(ctx, dir, kind, func(b []byte) bool { return send(name, b) })
		}(name, dir)
	}
	for _, rm := range remotes {
		wg.Add(1)
		go func(peer, name string) {
			defer wg.Done()
			_ = s.mesh.TailStream(ctx, peer, name, kind, func(b []byte) { send(peer+"/"+name, b) })
		}(rm.peer, rm.name)
	}
	for {
		select {
		case <-r.Context().Done():
			// 先等全部尾随 goroutine 退出再返回：否则它们可能还在写响应体，
			// 与 net/http 的 finishRequest 产生数据竞争（-race 实测）。
			cancel()
			wg.Wait()
			return
		case <-heart.C:
			if !ping() {
				cancel()
				wg.Wait()
				return
			}
		}
	}
}

func readRange(path string, from, to int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(from, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, to-from)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

// eventsDTO: 全设备最近事件（签名命中 + 采集器生命周期 + 刷机标记）。
type eventsDTO struct {
	Device string   `json:"device"`
	Lines  []string `json:"lines"`
}

// handleEvents: 每台设备取最新 events 文件的末尾 N 行，按设备分组。
// 事件量低（除非故障刷屏），N=80 足够面板一屏。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	out := []eventsDTO{}
	if s.status != nil {
		for _, d := range s.status() {
			dir, ok := s.deviceDir(d.Name)
			if !ok {
				continue
			}
			name, _, has := latestFile(dir, "events")
			if !has {
				continue
			}
			b, err := tailBytes(filepath.Join(dir, name), 32*1024)
			if err != nil {
				continue
			}
			lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			if len(lines) > 80 {
				lines = lines[len(lines)-80:]
			}
			if len(lines) == 1 && lines[0] == "" {
				lines = nil
			}
			out = append(out, eventsDTO{Device: d.Name, Lines: lines})
		}
	}
	writeJSON(w, out)
}

// handleCmd: POST /api/cmd {"cmd":"pause|resume|proxy","pattern":"...","action":"stop?"}
// —— 桥到 ctl 通道同一处理路径。
func (s *Server) handleCmd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.commander == nil {
		http.Error(w, "command channel unavailable", http.StatusServiceUnavailable)
		return
	}
	var req ctl.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch req.Cmd {
	case "status", "pause", "resume", "proxy", "release", "reopen", "reset",
		"mesh-pair", "mesh-approve", "mesh-revoke":
	default:
		http.Error(w, "cmd not allowed from web (use CLI): "+req.Cmd, http.StatusBadRequest)
		return
	}
	resp, err := s.commander(req)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}
