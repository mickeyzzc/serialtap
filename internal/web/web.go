// Package web 实现 serialtap 的 Web 观测面板：设备接入/采集状态、实时串口
// 日志尾随（SSE）、事件流（签名命中 + 生命周期）、日志文件浏览、暂停/恢复/
// 代理操作。只读状态来自 daemon.Status() 与日志目录本身；操作经注入的
// commander 复用 ctl 通道的同一条处理路径 —— 面板不引入第二套控制逻辑，
// 也不引入任何业务判断（serialtap 定位不变）。
package web

import (
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
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
)

//go:embed index.html
var indexHTML []byte

// Commander: 单响应命令通道（status/pause/resume/proxy），由 CLI 层桥接到
// ctl 处理函数。flash/release 等流式/长操作不进面板（走 CLI）。
type Commander func(ctl.Request) (ctl.Response, error)

// StatusProvider: 当前设备状态（daemon.Status）。
type StatusProvider func() []ctl.DevState

// Server: 面板 HTTP 服务（默认只听本机回环）。
type Server struct {
	root      string
	status    StatusProvider
	commander Commander
	srv       *http.Server
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
	logf func(string, ...any)) *Server {
	addr = PickAddr(addr)
	s := &Server{root: root, status: status, commander: commander}
	if addr == "" {
		return s
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/cmd", s.handleCmd)
	mux.HandleFunc("/api/devices/", s.handleDevice) // {name}/files|tail|live
	s.srv = &http.Server{Addr: addr, Handler: mux}
	go func() {
		logf("[web] 观测面板: http://%s/", addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("[web] 面板退出: %v", err)
		}
	}()
	return s
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
func latestFile(dir, kind string) (string, int64, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, false
	}
	best, bestDay, bestSfx, size := "", "", -1, int64(0)
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
			size = 0
			if fi, err := e.Info(); err == nil {
				size = fi.Size()
			}
		}
	}
	return best, size, best != ""
}

// deviceDTO: 面板设备载荷 = ctl.DevState + 日志目录富化（最新文件与大小，
// 前端用大小差分算写入速率）。
type deviceDTO struct {
	Name  string `json:"name"`
	Tty   string `json:"tty"`
	Key   string `json:"key"`
	State string `json:"state"`
	Proxy string `json:"proxy,omitempty"`            // 附加中的客户端地址
	ProxyEndpoint string `json:"proxy_endpoint,omitempty"` // 透传监听端点（空 = 未开）

	SerialFile  string `json:"serial_file,omitempty"`
	SerialSize  int64  `json:"serial_size"`
	EventsFile  string `json:"events_file,omitempty"`
	EventsSize  int64  `json:"events_size"`
	SerialBytes int64  `json:"serial_bytes"` // 设备目录全量日志总字节（估留存）
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
			Proxy: d.Proxy, ProxyEndpoint: d.ProxyEndpoint}
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
		if fi, err := e.Info(); err == nil {
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

	const initBytes = 16 * 1024
	curName, curSize, has := latestFile(dir, kind)
	if has {
		b, err := tailBytes(filepath.Join(dir, curName), initBytes)
		if err == nil && !send(b) {
			return
		}
	}
	tick := time.NewTicker(700 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heart.C:
			if !ping() {
				return
			}
		case <-tick.C:
			name, size, ok := latestFile(dir, kind)
			if !ok {
				continue
			}
			if !has || name != curName || size < curSize { // 轮转/跨天 → 重发末尾
				b, err := tailBytes(filepath.Join(dir, name), initBytes)
				if err != nil || !send(b) {
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
			if !send(b) {
				return
			}
			curSize = size
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
	case "status", "pause", "resume", "proxy":
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
