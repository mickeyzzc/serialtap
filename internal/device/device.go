// Package device 负责 USB 串口设备的发现与稳定身份（by-path 为 key）。
package device

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mickeyzzc/serialtap/internal/config"
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

// 内置 VID:PID 命名规则（精确匹配）。by-id 规则未命中时套用：
// Linux 上 by-id 规则先命中（结果一致，冗余无害）；Windows/macOS 的 by-id 字符串
// 形态不同，枚举层拿到 VID:PID 时靠这对设备定型。
type vidRule struct{ VID, PID, Name string }

var builtinVIDRules = []vidRule{
	{VID: "1a86", PID: "7523", Name: "ch340"},
	{VID: "1a86", PID: "7522", Name: "ch343"},
	{VID: "1a86", PID: "55d3", Name: "ch343"},
	{VID: "303a", PID: "1001", Name: "esp32s3-jtag"}, // 原生 USB-JTAG/串口
}

func applyVIDRules(vid, pid string) string {
	if vid == "" || pid == "" {
		return ""
	}
	for _, r := range builtinVIDRules {
		if r.VID == vid && r.PID == pid {
			return r.Name
		}
	}
	return ""
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
			name = applyVIDRules(vid, pid)
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
