// Package tray 实现 Windows 托盘常驻：接入状态查看、按设备暂停/恢复（接入开关）、
// 打开日志。与守护进程只经 ctl socket 通信，不做本地直连。
// model.go 是平台无关的可测部分；systray 接线在 tray_windows.go。
package tray

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
)

// PatternFor: 设备名 → 精确匹配的锚定正则（ctl pause/resume 的 pattern 用）。
// 设备名可含 '.'（SanitizeName 放行），必须 QuoteMeta 防止正则元字符误伤。
func PatternFor(name string) string {
	return "^" + regexp.QuoteMeta(name) + "$"
}

// StateIcon: 状态图标字符。
func StateIcon(s string) string {
	switch s {
	case "collecting":
		return "●"
	case "paused":
		return "⏸"
	case "suspended":
		return "⏳"
	case "flashing":
		return "⚡"
	}
	return "·"
}

// DeviceTitle: 设备菜单标题，如 "⏸ esp32s3-jtag (COM3)"。
func DeviceTitle(d ctl.DevState) string {
	return StateIcon(d.State) + " " + d.Name + " (" + d.Tty + ")"
}

// Collecting: checkbox 勾选态（勾选 = 正在采集）。
func Collecting(s string) bool { return s == "collecting" }

// SnapshotHash: 菜单重建的变更检测（状态无变化时不重建菜单）。
func SnapshotHash(devs []ctl.DevState, daemonOK bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ok=%v|", daemonOK)
	for _, d := range devs {
		fmt.Fprintf(&b, "%s;%s;%s|", d.Name, d.Tty, d.State)
	}
	return b.String()
}

// LatestSerialLog: root/<name>/ 下最新的全量日志。文件名含定宽日期，
// 字典序即时间序；当日基础文件（无轮转后缀）在同日中排序最大，
// 因此"取字典序最大的 serial-*.log"即最新。
func LatestSerialLog(root, name string) string {
	dir := filepath.Join(root, name)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "serial-") || !strings.HasSuffix(n, ".log") {
			continue
		}
		if n > best {
			best = n
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(dir, best)
}

// QueryStatus: 带 3s 超时的 status 查询 —— 守护进程挂死时托盘不能被拖死，
// 超时按"未运行"处理。
func QueryStatus(sockPath string) ([]ctl.DevState, bool) {
	type res struct {
		devs []ctl.DevState
		ok   bool
	}
	ch := make(chan res, 1)
	go func() {
		var devs []ctl.DevState
		err := ctl.Send(sockPath, ctl.Request{Cmd: "status"}, func(r ctl.Response) bool {
			if r.OK {
				devs = r.Devices
			}
			return true
		})
		ch <- res{devs, err == nil}
	}()
	select {
	case r := <-ch:
		return r.devs, r.ok
	case <-time.After(3 * time.Second):
		return nil, false
	}
}

// SendPause/SendResume: 发 pause/resume（pattern 空 = 全部设备）。
// 结果不做即时反馈 —— 轮询会把新状态刷进菜单。
func SendPause(sockPath, pattern string) error {
	return ctl.Send(sockPath, ctl.Request{Cmd: "pause", Pattern: pattern}, nil)
}

func SendResume(sockPath, pattern string) error {
	return ctl.Send(sockPath, ctl.Request{Cmd: "resume", Pattern: pattern}, nil)
}
