// Package device 负责 USB 串口设备的发现与稳定身份（by-path 为 key）。
package device

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mickeyzzc/serialtap/internal/config"
	serial "go.bug.st/serial"
)

// DeviceInfo: 一个 USB 串口设备的完整身份。
// Key 用 by-path（物理 USB 口恒定、同型号适配器不撞车 —— CH340 的 by-id 没有序列号，
// 两只 CH340 的 by-id 完全相同，不能当 key）。Name 供日志目录用。
type DeviceInfo struct {
	Tty    string // /dev/ttyUSB0
	Key    string // by-path 基名；非 USB 串口无 by-path 时退化为 "tty:<名>"
	ByPath string // by-path 基名（同 Key，冗余便于排查）
	ByID   string // by-id 基名（可读身份，同型号可能重复，仅作展示/命名）
	VID    string
	PID    string
	Name   string
}

// 内置命名规则（by-id 正则 → 名）。配置 Names 优先，未命中再走这里。
// 注意 Espressif 原生 USB-JTAG 的 by-id 含 MAC（如 ..._XX:XX:XX:XX:XX:XX-if00），
// seeed 与 n16r8 同为 ESP32-S3 无法从芯片区分 —— 要精确到板子请在配置里按 MAC 序列号细分。
var builtinNameRules = []config.NameRule{
	{Match: `USB_Serial-if00`, Name: "ch340"},           // 1a86:7523 — ai-thinker 板
	{Match: `USB_Single_Serial`, Name: "ch343"},         // 1a86:7522/55d3 — luatos 板
	{Match: `Espressif_USB_JTAG`, Name: "esp32s3-jtag"}, // 原生 USB-JTAG — seeed/n16r8
}

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

func applyNameRules(rules []config.NameRule, byID string) string {
	for _, r := range rules {
		if r.Match == "" || r.Name == "" {
			continue
		}
		re, err := regexp.Compile(r.Match)
		if err != nil || !re.MatchString(byID) {
			continue
		}
		return r.Name
	}
	return ""
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

// buildDevices: Enumerate 的可测核心（端口清单/索引/ID 查询全部注入）。
func buildDevices(ports []string, byID, byPath map[string]string,
	id func(string) (string, string), exclude []*regexp.Regexp, names []config.NameRule) []DeviceInfo {
	var out []DeviceInfo
	for _, p := range ports {
		tty := filepath.Base(p)
		pathID, isUSB := byPath[tty]
		if !isUSB {
			continue
		}
		idv := byID[tty]
		vid, pid := id(tty)
		name := applyNameRules(names, idv)
		if name == "" {
			name = applyNameRules(builtinNameRules, idv)
		}
		if name == "" {
			if idv != "" {
				name = idv
			} else {
				name = tty
			}
		}
		d := DeviceInfo{
			Tty: p, Key: pathID, ByPath: pathID, ByID: idv,
			VID: vid, PID: pid, Name: SanitizeName(name),
		}
		if matchAny(exclude, d.Tty, d.Key, d.ByID, d.Name) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func matchAny(pats []*regexp.Regexp, fields ...string) bool {
	for _, p := range pats {
		for _, f := range fields {
			if f != "" && p.MatchString(f) {
				return true
			}
		}
	}
	return false
}

func CompilePatterns(pats []string) (compiled []*regexp.Regexp, bad []string) {
	for _, p := range pats {
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			bad = append(bad, p)
			continue
		}
		compiled = append(compiled, re)
	}
	return compiled, bad
}

// SanitizeName: 设备名 → 目录安全名（设备名的唯一规范化入口，
// logstore/日志目录与显示名共用）。
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "dev"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
