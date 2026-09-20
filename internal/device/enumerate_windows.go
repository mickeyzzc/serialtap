//go:build windows

package device

import (
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/windows/registry"

	"github.com/mickeyzzc/serialtap/internal/config"
	serial "go.bug.st/serial"
)

// Windows 枚举：端口清单来自 HKLM\HARDWARE\DEVICEMAP\SERIALCOMM（活设备，
// serial.GetPortsList 即读它），USB 元数据（VID/PID/实例 ID）来自注册表 Enum 树。
// 纯注册表查询，无需 CGO/SetupAPI。
//
// 身份语义（对齐 Linux by-path）：
//   - Key = USB 实例 ID。有序列号的设备实例即序列号（换口不变，日志续写）；
//     无序列号的设备（两只同型号 CH340）Windows 按物理位置生成实例
//     （形如 5&2f5a5d5&0&2，末位是父集线器端口）—— 同型号不撞车、换口即换身份。
//   - ByID = 完整 PNP 实例路径（USB\VID_xxxx&PID_yyyy\<实例>），供正则匹配/展示。

// usbMeta: 一个 USB 串口的注册表元数据。
type usbMeta struct {
	vid, pid, instance string
}

// Enumerate: 当前所有 USB 串口（带身份）。非 USB 串口（PCIe/主板串口）
// 在注册表中无 USB Enum 条目，被过滤 —— 与 Linux 的 by-path 过滤语义一致。
func Enumerate(exclude []*regexp.Regexp, names []config.NameRule) ([]DeviceInfo, error) {
	ports, err := serial.GetPortsList() // "COM3" 等活设备清单
	if err != nil {
		return nil, err
	}
	meta := usbSerialMetaAt(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\USB`)
	return devicesFromMeta(ports, meta, exclude, names), nil
}

// devicesFromMeta: 端口清单 + 注册表元数据 → 设备身份（可注入，测试用）。
func devicesFromMeta(ports []string, meta map[string]usbMeta,
	exclude []*regexp.Regexp, names []config.NameRule) []DeviceInfo {
	byID := map[string]string{}
	byPath := map[string]string{}
	ids := map[string]usbMeta{}
	for _, p := range ports {
		tty := filepath.Base(p) // COM3
		m, ok := meta[tty]
		if !ok {
			continue
		}
		byID[tty] = "USB\\VID_" + strings.ToUpper(m.vid) + "&PID_" + strings.ToUpper(m.pid) + "\\" + m.instance
		byPath[tty] = m.instance
		ids[tty] = m
	}
	idf := func(tty string) (string, string) {
		if m, ok := ids[tty]; ok {
			return m.vid, m.pid
		}
		return "", ""
	}
	return buildDevices(ports, byID, byPath, idf, exclude, names)
}

// usbSerialMetaAt: 扫 <enumRoot>\VID_xxxx&PID_yyyy\<实例>\Device Parameters\PortName，
// 返回 COM 名 → 元数据。根键与 enumRoot 均可注入（生产 LOCAL_MACHINE，
// 测试用 CURRENT_USER 下的假树）。
// Enum 树会残留已拔设备的条目，但 PortName 只对当前口清单（SERIALCOMM）生效。
func usbSerialMetaAt(rootKey registry.Key, enumRoot string) map[string]usbMeta {
	out := map[string]usbMeta{}
	usbKey, err := registry.OpenKey(rootKey, enumRoot,
		registry.READ|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return out
	}
	defer usbKey.Close()
	vidpids, err := usbKey.ReadSubKeyNames(-1)
	if err != nil {
		return out
	}
	for _, vp := range vidpids {
		vid, pid, ok := parseVIDPID(vp)
		if !ok {
			continue // 非 VID_&PID_ 形态（少见的杂项枚举条目）
		}
		vpKey, err := registry.OpenKey(rootKey, enumRoot+`\`+vp,
			registry.READ|registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		instances, err := vpKey.ReadSubKeyNames(-1)
		vpKey.Close()
		if err != nil {
			continue
		}
		for _, inst := range instances {
			dpPath := enumRoot + `\` + vp + `\` + inst + `\Device Parameters`
			dpKey, err := registry.OpenKey(rootKey, dpPath, registry.READ)
			if err != nil {
				continue
			}
			portName, _, err := dpKey.GetStringValue("PortName")
			dpKey.Close()
			if err != nil || !strings.HasPrefix(strings.ToUpper(portName), "COM") {
				continue
			}
			if _, dup := out[portName]; dup {
				continue // 同口多驱动条目（如换过驱动），取第一个
			}
			out[portName] = usbMeta{vid: vid, pid: pid, instance: inst}
		}
	}
	return out
}

// parseVIDPID: "VID_1A86&PID_7523" → ("1a86", "7523", true)。
func parseVIDPID(vp string) (vid, pid string, ok bool) {
	f := strings.Split(vp, "&")
	if len(f) != 2 || !strings.HasPrefix(f[0], "VID_") || !strings.HasPrefix(f[1], "PID_") {
		return "", "", false
	}
	vid = strings.ToLower(strings.TrimPrefix(f[0], "VID_"))
	pid = strings.ToLower(strings.TrimPrefix(f[1], "PID_"))
	if len(vid) != 4 || len(pid) != 4 {
		return "", "", false
	}
	return vid, pid, true
}
