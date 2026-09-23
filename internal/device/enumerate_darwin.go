//go:build darwin

package device

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/mickeyzzc/serialtap/internal/config"
	serial "go.bug.st/serial"
)

// —— macOS 设备身份 ——
//
// Linux 用 sysfs/by-path；macOS 无对应物，改从 IOKit 注册表（ioreg）取身份：
//   - Key（by-path 等价物）：USB locationID —— 物理口稳定、同型号适配器不撞车，
//     换口即换身份，语义与 by-path 一致
//   - ByID：usb-<vid>_<pid>[-<序列号>]，风格与 Linux by-id 对齐，
//     配置 names 规则（含内置 ch340/ch343 规则）可跨平台复用
//
// 解析 `ioreg -p IOService -l -w 0` 文本树（纯 Go、无 CGO）：IOSerialBSDClient
// 节点的 IOCalloutDevice 给出 cu 名，沿祖先链向上找最近的含 idVendor 的节点
// 即 USB 设备。只枚举 /dev/cu.usb*（滤掉蓝牙/wlan-debug 等本机串口）；
// 打开用 cu.* 而非 tty.*（tty.* 在 macOS 上 open 会阻塞等待载波）。

// usbIdentity: ioreg 里挖出的一个 USB 串口身份（vid/pid/loc 已是 4/8 位小写十六进制）。
type usbIdentity struct {
	vid, pid, serial, loc string
}

// ioregSource: ioreg 执行 seam（测试注入假输出）。portsSource 同理（端口清单 seam）。
var (
	ioregSource = func() (string, error) {
		b, err := exec.Command("ioreg", "-p", "IOService", "-l", "-w", "0").Output()
		return string(b), err
	}
	portsSource = serial.GetPortsList
)

// identCache: tty 基名 → 身份。ioreg 全树解析不便宜（数万行），
// 只在 /dev/cu.usb* 集合出现新名字（热插拔）时重跑，稳态轮询零开销。
var (
	identMu    sync.Mutex
	identCache = map[string]usbIdentity{}
)

// Enumerate: 当前所有 USB 串口（带身份）。ioreg 失败或某口在注册表里找不到
// USB 祖先时退化为 tty 名身份 —— 发现能力优先，绝不因身份缺失阻塞采集。
func Enumerate(exclude []*regexp.Regexp, names []config.NameRule) ([]DeviceInfo, error) {
	all, err := portsSource()
	if err != nil {
		return nil, err
	}
	var ports []string
	for _, p := range all {
		if strings.HasPrefix(p, "/dev/cu.usb") {
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		return nil, nil
	}
	idents := lookupIdents(ports)
	byID, byPath, ids := map[string]string{}, map[string]string{}, map[string][2]string{}
	for _, p := range ports {
		tty := filepath.Base(p)
		id := idents[tty]
		byPath[tty] = darwinKey(tty, id)
		byID[tty] = darwinByID(tty, id)
		ids[tty] = [2]string{id.vid, id.pid}
	}
	idf := func(tty string) (string, string) {
		pair := ids[tty]
		return pair[0], pair[1]
	}
	return buildDevices(ports, byID, byPath, idf, exclude, names), nil
}

func lookupIdents(ports []string) map[string]usbIdentity {
	identMu.Lock()
	idents := map[string]usbIdentity{}
	var missing []string
	for _, p := range ports {
		tty := filepath.Base(p)
		if id, ok := identCache[tty]; ok {
			idents[tty] = id
		} else {
			missing = append(missing, tty)
		}
	}
	identMu.Unlock()
	if len(missing) == 0 {
		return idents
	}
	if fresh, err := scanIOReg(); err == nil {
		for _, tty := range missing {
			idents[tty] = fresh[tty] // 没命中为零值 → tty 名兜底
		}
		identMu.Lock()
		for k, v := range idents {
			identCache[k] = v
		}
		identMu.Unlock()
	} else {
		for _, tty := range missing {
			idents[tty] = usbIdentity{}
		}
	}
	return idents
}

func darwinKey(tty string, id usbIdentity) string {
	if id.loc != "" {
		return "usb-" + id.loc
	}
	return strings.TrimPrefix(tty, "cu.")
}

func darwinByID(tty string, id usbIdentity) string {
	if id.vid != "" && id.pid != "" {
		s := fmt.Sprintf("usb-%s_%s", id.vid, id.pid)
		if id.serial != "" {
			s += "-" + id.serial
		}
		return s
	}
	return tty
}

// scanIOReg: 跑一次 ioreg 并解析出 tty 基名 → 身份。
func scanIOReg() (map[string]usbIdentity, error) {
	out, err := ioregSource()
	if err != nil {
		return nil, err
	}
	return parseIOReg(out), nil
}

type ioregNode struct {
	name  string
	props map[string]string
}

// parseIOReg: 解析 ioreg 文本树。节点行每层缩进 2 列（标记 +-o / \-o 恒在偶数列），
// 属性行 `"Key" = Value`；IOSerialBSDClient 的 IOCalloutDevice 触发祖先回溯。
func parseIOReg(text string) map[string]usbIdentity {
	var stack []*ioregNode
	out := map[string]usbIdentity{}
	for _, line := range strings.Split(text, "\n") {
		if name, depth, ok := parseNodeLine(line); ok {
			stack = append(stack[:min(depth, len(stack))], &ioregNode{name: name, props: map[string]string{}})
			continue
		}
		if k, v, ok := parsePropLine(line); ok && len(stack) > 0 {
			stack[len(stack)-1].props[k] = v
			if k == "IOCalloutDevice" && v != "" {
				if id, ok := usbAncestry(stack); ok {
					out[filepath.Base(v)] = id
				}
			}
		}
	}
	return out
}

func parseNodeLine(line string) (name string, depth int, ok bool) {
	m := strings.Index(line, "+-o ")
	a := strings.Index(line, "\\-o ")
	if a >= 0 && (m < 0 || a < m) {
		m = a
	}
	if m < 0 || m%2 != 0 {
		return "", 0, false
	}
	for _, r := range line[:m] {
		if r != ' ' && r != '|' && r != '+' && r != '-' && r != '\\' {
			return "", 0, false
		}
	}
	rest := line[m+4:]
	if i := strings.IndexByte(rest, ' '); i > 0 {
		rest = rest[:i]
	}
	return rest, m / 2, rest != ""
}

func parsePropLine(line string) (k, v string, ok bool) {
	q := strings.IndexByte(line, '"')
	if q < 0 {
		return "", "", false
	}
	rest := line[q+1:]
	e := strings.Index(rest, `" = `)
	if e < 0 {
		return "", "", false
	}
	k = rest[:e]
	v = strings.TrimSpace(rest[e+4:])
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	return k, v, k != ""
}

// usbAncestry: 从串口客户端向上找最近的含 idVendor 的祖先（即 USB 设备节点）。
func usbAncestry(stack []*ioregNode) (usbIdentity, bool) {
	for i := len(stack) - 2; i >= 0; i-- {
		p := stack[i].props
		if p["idVendor"] == "" {
			continue
		}
		return usbIdentity{
			vid:    hexNum(p["idVendor"], 4),
			pid:    hexNum(p["idProduct"], 4),
			serial: p["USB Serial Number"],
			loc:    hexNum(p["locationID"], 8),
		}, true
	}
	return usbIdentity{}, false
}

func hexNum(s string, width int) string {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%0*x", width, n)
}
