//go:build linux

package device

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/mickeyzzc/serialtap/internal/config"
	serial "go.bug.st/serial"
	"regexp"
)

// symlinkIndex: /dev/serial/by-{id,path} → map[tty名]条目名
func symlinkIndex(dir string) map[string]string {
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		tgt, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out[filepath.Base(tgt)] = e.Name()
	}
	return out
}

// sysfsID: 从 /sys/class/tty/<tty>/device 解析出 USB VID:PID。
// 注意不能用 filepath.Join 拼 "device/.."——Join 的词法清理会在内核穿越
// symlink 之前把 ".." 折叠掉（ttyUSB 的 device 指向自己的 sysfs 目录而非
// USB 接口目录，需要向上走两级才到含 idVendor 的 USB 设备目录）。
func sysfsID(tty string) (vid, pid string) {
	return sysfsIDAt("/sys/class/tty", tty)
}

func sysfsIDAt(base, tty string) (vid, pid string) {
	real, err := filepath.EvalSymlinks(filepath.Join(base, tty, "device"))
	if err != nil {
		return "", ""
	}
	dir := real
	for i := 0; i < 4; i++ {
		b, err1 := os.ReadFile(filepath.Join(dir, "idVendor"))
		p, err2 := os.ReadFile(filepath.Join(dir, "idProduct"))
		if err1 == nil && err2 == nil {
			return strings.TrimSpace(string(b)), strings.TrimSpace(string(p))
		}
		dir = filepath.Dir(dir)
	}
	return "", ""
}

// Enumerate: 当前所有 USB 串口（带身份）。只有存在 /dev/serial/by-path 条目的口才算
// USB 串口 —— 这会滤掉主板上的 ttyS*。exclude 任一命中（tty/key/by-id/name）即忽略。
func Enumerate(exclude []*regexp.Regexp, names []config.NameRule) ([]DeviceInfo, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil, err
	}
	return buildDevices(ports,
		symlinkIndex("/dev/serial/by-id"),
		symlinkIndex("/dev/serial/by-path"),
		sysfsID, exclude, names), nil
}
