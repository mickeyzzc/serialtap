// Package web 的 mesh 扩展：面板聚合视图 / 远程日志尾随 / 远程刷机。
// 面板依旧零业务逻辑——聚合查询与尾随/刷机执行体全部由 cli 注入（mesh.Node
// 适配下面两个接口），面板只做展示与转发。
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/flash"
)

// MeshService: mesh 节点能力面（cli 注入 mesh.Node；nil = mesh 未启用）。
type MeshService interface {
	AggregateStatus(timeout time.Duration) []ctl.PeerStatus
	TailStream(ctx context.Context, peerName, device, kind string, onData func([]byte)) error
}

// MeshFlasher: 远程刷机执行体（peer 侧 flash，镜像先经加密信道上传）。
type MeshFlasher func(peer, pattern string, all bool, spec flash.Spec, out func(line string)) error

// WithMesh: 启用面板 mesh 视图（peer 聚合 + 远程尾随）。
func WithMesh(m MeshService) Option { return func(s *Server) { s.mesh = m } }

// WithMeshFlasher: 启用面板远程刷机（/api/flash 带 peer 字段时走它）。
func WithMeshFlasher(f MeshFlasher) Option { return func(s *Server) { s.meshFlasher = f } }

var meshPeerRe = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// meshPingEvery: 远程尾随 SSE 的心跳周期。包级变量只为测试能缩短（-race 下
// 压出数据帧与心跳的真实交错），生产值 15s 与本地尾随一致。
var meshPingEvery = 15 * time.Second

// handleMeshStatus: GET /api/mesh/status → {enabled, peers}。未启用时
// enabled=false（面板据此隐藏 mesh 区）。
func (s *Server) handleMeshStatus(w http.ResponseWriter, _ *http.Request) {
	if s.mesh == nil {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, map[string]any{
		"enabled": true,
		"peers":   s.mesh.AggregateStatus(5 * time.Second),
	})
}

// handleMeshDevice: /api/mesh/devices/{peer}/{name}/live?kind=serial|events ——
// SSE 桥接 peer 的日志尾随（帧格式与本地 serveLive 完全一致，前端同管线）。
func (s *Server) handleMeshDevice(w http.ResponseWriter, r *http.Request) {
	if s.mesh == nil {
		http.Error(w, "mesh 未启用", http.StatusServiceUnavailable)
		return
	}
	rest := r.URL.Path[len("/api/mesh/devices/"):]
	// 形如 {peer}/{name}/live；peer 允许 host:port 形态（不含 /），name 为目录安全名
	i := strings.Index(rest, "/")
	if i <= 0 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	peer, rest := rest[:i], rest[i+1:]
	j := strings.LastIndex(rest, "/")
	if j <= 0 || j == len(rest)-1 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	name, action := rest[:j], rest[j+1:]
	if action != "live" || !meshPeerRe.MatchString(peer) || !safeNameRe.MatchString(name) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "serial" && kind != "events" {
		kind = "serial"
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// 尾随数据帧（TailStream goroutine）与 15s 心跳 ping（主循环）并发写
	// 同一 ResponseWriter —— 必须互斥（对齐 handleLiveMulti，那边是 -race
	// 实测过的教训；http.ResponseWriter 非并发安全）。
	var mu sync.Mutex
	send := func(b []byte) bool {
		if len(b) == 0 {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		payload, _ := json.Marshal(string(b))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	ping := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	heart := time.NewTicker(meshPingEvery)
	defer heart.Stop()
	if !ping() { // 首帧立刻发，逼出缓冲路径的 early flush
		return
	}

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer close(done)
		_ = s.mesh.TailStream(ctx, peer, name, kind, func(chunk []byte) {
			send(chunk)
		})
	}()
	for {
		select {
		case <-r.Context().Done():
			<-done // 等 TailStream 退出，避免其仍在写响应体时 handler 已返回（同 handleLiveMulti 的竞态）
			return
		case <-done:
			return
		case <-heart.C:
			if !ping() {
				return
			}
		}
	}
}
