// Package ctl 实现守护进程的本地控制通道：unix socket + JSON 行协议。
// 供 CLI（flash/release/status/pause/resume）与守护进程通信。
package ctl

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mickeyzzc/serialtap/internal/flash"
)

// Request: 客户端请求（一行 JSON）。
type Request struct {
	Cmd       string     `json:"cmd"`                  // status | pause | resume | release | flash | proxy | reopen | reset
	Pattern   string     `json:"pattern,omitempty"`    // 设备匹配正则（tty/name/key/by-id 任一）
	ForMs     int64      `json:"for_ms,omitempty"`     // release: 限时自动回采
	UntilIdle bool       `json:"until_idle,omitempty"` // release: 端口空闲后自动回采
	Spec      flash.Spec `json:"spec,omitempty"`       // flash: 刷写参数
	Action    string     `json:"action,omitempty"`     // proxy: start | stop
	All       bool       `json:"all,omitempty"`        // flash/reopen/reset: 模式匹配多台仍逐台执行（默认拒绝，防误伤在测设备）
}

// DevState: status 返回的设备状态。
type DevState struct {
	Name          string `json:"name"`
	Tty           string `json:"tty"`
	Key           string `json:"key"`
	State         string `json:"state"`                    // collecting | paused | suspended | flashing
	Proxy         string `json:"proxy,omitempty"`          // 透传会话客户端地址（空 = 无会话）
	ProxyEndpoint string `json:"proxy_endpoint,omitempty"` // 透传监听端点（空 = 未开端点；注意与 Proxy 客户端地址区分）
}

// Response: 服务端响应（一行 JSON；flash 会流式多行）。
type Response struct {
	OK        bool       `json:"ok"`
	Error     string     `json:"error,omitempty"`
	Event     string     `json:"event,omitempty"` // flash-log | flash-done
	Line      string     `json:"line,omitempty"`
	Code      int        `json:"code,omitempty"`
	Devices   []DevState `json:"devices,omitempty"`
	Endpoint  string     `json:"endpoint,omitempty"`   // proxy start: 透传 TCP 端点
	Device    string     `json:"device,omitempty"`     // proxy start: 返回端点所属设备名
	DeviceKey string     `json:"device_key,omitempty"` // proxy start: 返回端点所属设备 key
}

// Handler: 请求处理。respond 可多次调用（flash 流式输出），最后一次带总结性状态。
type Handler func(req Request, respond func(Response))

// Server: unix socket 控制服务。
type Server struct {
	ln     net.Listener
	wg     sync.WaitGroup
	stopCh chan struct{}
}

// DefaultSocketPath: 平台相关（socketpath_unix.go / socketpath_windows.go）。

// Listen: 建立监听（socket 权限 0600，同用户专用）。
// socket 已被活着的实例持有 → 拒绝（防止第二实例偷走控制通道）；
// 仅残留文件（上次异常退出，无人监听）才清理接管。
func Listen(path string) (*Server, error) {
	if _, err := os.Stat(path); err == nil {
		if c, derr := net.DialTimeout("unix", path, time.Second); derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("控制 socket 已被另一个 serialtap 实例占用: %s", path)
		}
		_ = os.Remove(path) // 死 socket
	}
	// Windows 默认路径在 %LOCALAPPDATA%\serialtap 下，父目录可能不存在
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("控制 socket 目录创建失败: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("控制 socket 监听失败: %w", err)
	}
	// Windows 的 AF_UNIX 无文件权限语义，Chmod 仅在类 Unix 上有实际效果
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &Server{ln: ln, stopCh: make(chan struct{})}, nil
}

// Serve: 接受循环（阻塞）。Close 后返回。
func (s *Server) Serve(h Handler) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return // 正常关闭
			default:
				time.Sleep(10 * time.Millisecond) // 防 Accept 错误热自旋
				continue
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn, h)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn, h Handler) {
	defer func() { _ = conn.Close() }()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var req Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			writeJSON(conn, Response{OK: false, Error: "bad request: " + err.Error()})
			continue
		}
		respond := func(r Response) { writeJSON(conn, r) }
		h(req, respond)
	}
}

// Close: 停止接受并等待在途连接结束，删除 socket 文件。
func (s *Server) Close() {
	close(s.stopCh)
	_ = s.ln.Close()
	s.wg.Wait()
	_ = os.Remove(s.ln.Addr().String())
}

func writeJSON(conn net.Conn, r Response) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(b, '\n'))
}

// Send: 客户端 —— 发送请求并消费响应流直到 flash-done（或首个非流响应）。
func Send(sockPath string, req Request, onEvent func(Response) bool) error {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("连不上守护进程（%s）：未运行 serialtap run？%w", sockPath, err)
	}
	defer func() { _ = conn.Close() }()
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Response
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if onEvent == nil || onEvent(r) {
			return nil // 回调返回 true = 结束消费
		}
	}
	return sc.Err()
}
