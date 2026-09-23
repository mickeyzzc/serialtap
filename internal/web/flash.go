package web

// 面板刷机：浏览器上传镜像/flasher_args.json → 存到 root/.flash-upload/ →
// 调注入的 Flasher（即 daemon.Flash：让口→esptool→回采）→ SSE 推进度。
// 单任务槽 —— daemon 的 opMu 本就把 flash/release 互斥串行，面板侧直接
// 409 拒绝并发请求更友好。

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mickeyzzc/serialtap/internal/flash"
)

// flashEvent: 刷机进度事件（line=esptool 输出行；done=true 时 err 空=成功）。
type flashEvent struct {
	Line string `json:"line,omitempty"`
	Done bool   `json:"done,omitempty"`
	Err  string `json:"err,omitempty"`
}

// flashJob: 当前/最近一次任务的进度广播（历史回放 + 实时订阅）。
type flashJob struct {
	mu     sync.Mutex
	events []flashEvent
	active bool
	subs   map[chan flashEvent]bool
}

const flashHistoryCap = 4000 // 进度事件上限（进度条 \r 已按行拆分，够长刷写用）

func (j *flashJob) publish(ev flashEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.events) < flashHistoryCap {
		j.events = append(j.events, ev)
	}
	if ev.Done {
		j.active = false
	}
	for ch := range j.subs {
		select { // 订阅方卡死不拖累刷写
		case ch <- ev:
		default:
		}
	}
}

func (j *flashJob) snapshot() (events []flashEvent, active bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]flashEvent(nil), j.events...), j.active
}

// handleFlash: POST /api/flash（multipart）。
// 字段：pattern（必填）；all（可选 "true"）；args_file（文件，与其余互斥）；
// bins（多文件，任意次序）+ offsets（JSON 字符串数组，与 bins 一一对应）。
// 镜像落在守护进程侧（面板与守护同机），路径进事件流可审计。
func (s *Server) handleFlash(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.flasher == nil {
		http.Error(w, "flasher unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	pattern := strings.TrimSpace(r.FormValue("pattern"))
	if pattern == "" {
		http.Error(w, "pattern 必填", http.StatusBadRequest)
		return
	}
	all := r.FormValue("all") == "true"

	s.job.mu.Lock()
	if s.job.active {
		s.job.mu.Unlock()
		http.Error(w, "已有刷写进行中，稍后再试", http.StatusConflict)
		return
	}
	s.job.mu.Unlock()

	dir := filepath.Join(s.root, ".flash-upload", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	spec := flash.Spec{}
	if fhs := r.MultipartForm.File["args_file"]; len(fhs) > 0 {
		p, err := saveUpload(fhs[0], dir, "flasher_args.json")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		spec.ArgsFile = p
	} else {
		fhs := r.MultipartForm.File["bins"]
		if len(fhs) == 0 {
			http.Error(w, "需要 bins 文件或 args_file 文件", http.StatusBadRequest)
			return
		}
		var offsets []string
		if v := r.FormValue("offsets"); v != "" {
			if err := json.Unmarshal([]byte(v), &offsets); err != nil || len(offsets) != len(fhs) {
				http.Error(w, "offsets 需为与 bins 数量一致的 JSON 数组（如 [\"0x0\",\"0x8000\",\"0x10000\"]）",
					http.StatusBadRequest)
				return
			}
		} else {
			offsets = make([]string, len(fhs)) // 缺省全 0x0
		}
		for i, fh := range fhs {
			p, err := saveUpload(fh, dir, fmt.Sprintf("bin%d.img", i+1))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			off := strings.TrimSpace(offsets[i])
			if off == "" {
				off = "0x0"
			}
			spec.Bins = append(spec.Bins, flash.BinSpec{Path: p, Offset: off})
		}
	}

	// 翻新任务槽并异步执行（daemon opMu 兜底并发互斥）
	s.job.mu.Lock()
	s.job.events = nil
	s.job.active = true
	s.job.mu.Unlock()
	go func() {
		err := s.flasher(pattern, all, spec, func(line string) {
			s.job.publish(flashEvent{Line: line})
		})
		ev := flashEvent{Done: true}
		if err != nil {
			ev.Err = err.Error()
		}
		s.job.publish(ev)
	}()
	writeJSON(w, map[string]any{"ok": true, "upload_dir": dir})
}

// flashStream: SSE 刷机进度 —— 先回放历史（晚连的浏览器不缺上下文），
// 再实时跟随，直到 done 事件后关流。空闲心跳与日志尾随同款。
func (s *Server) flashStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(ev flashEvent) bool {
		b, _ := json.Marshal(ev)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
		return
	}
	fl.Flush()

	events, active := s.job.snapshot()
	for _, ev := range events {
		if !send(ev) {
			return
		}
	}
	if !active {
		return // 任务已结束（历史即全量），流自然关闭
	}
	ch := make(chan flashEvent, 64)
	s.job.mu.Lock()
	if s.job.subs == nil { // 测试直构 Server 时的懒初始化（生产路径 Start 已建）
		s.job.subs = map[chan flashEvent]bool{}
	}
	s.job.subs[ch] = true
	s.job.mu.Unlock()
	defer func() {
		s.job.mu.Lock()
		delete(s.job.subs, ch)
		s.job.mu.Unlock()
	}()
	heart := time.NewTicker(15 * time.Second)
	defer heart.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heart.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev := <-ch:
			if !send(ev) {
				return
			}
			if ev.Done {
				return
			}
		}
	}
}

// saveUpload: 落一个上传文件（文件名清洗为安全名，防路径穿越）。
func saveUpload(fh *multipart.FileHeader, dir, fallbackName string) (string, error) {
	name := filepath.Base(fh.Filename)
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 || strings.HasPrefix(b.String(), ".") {
		name = fallbackName
	} else {
		name = b.String()
	}
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	defer func() { _ = dst.Close() }()
	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	return dst.Name(), nil
}
