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
//   - 复合设备（ESP32 原生 USB-JTAG/CDC）：串口挂在接口子键 VID_x&PID_y&MI_00 下，
//     其实例是位置派生的；父设备键下若只有唯一实例（即板子的序列号/MAC，
//     如 AA:BB:CC:DD:EE:FF）则用父实例作 Key（换口日志续写），否则退回接口实例。
//   - ByID = 完整 PNP 实例路径（USB\VID_xxxx&PID_yyyy\[&MI_zz\]<实例>），供正则匹配/展示。

// usbMeta: 一个 USB 串口的注册表元数据。
type usbMeta struct {
	vid, pid, instance string
	pnp                string // 完整 PNP 实例路径（by-id 用）
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
		byID[tty] = m.pnp
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

// usbSerialMetaAt: 扫 <enumRoot>\VID_xxxx&PID_yyyy[&MI_zz]\<实例>\Device Parameters\PortName，
// 返回 COM 名 → 元数据。根键与 enumRoot 均可注入（生产 LOCAL_MACHINE，
// 测试用 CURRENT_USER 下的假树）。复合设备的串口挂在 &MI_00 接口子键下
// （ESP32 原生 USB 实测：USB\VID_303A&PID_1001&MI_00\7&af7bb08&2&0000）。
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
		vid, pid, mi, ok := parseVIDPID(vp)
		if !ok {
			continue // 非 VID_&PID_[&MI_zz] 形态（少见的杂项枚举条目）
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
		// 复合接口子键（&MI_zz）下的串口：父设备键下唯一实例时，
		// 用父实例（板子序列号/MAC）作身份 —— 换口不变，日志续写
		parentInst := ""
		if mi != "" {
			parentInst = singleInstance(rootKey, enumRoot, strings.TrimSuffix(vp, "&"+mi))
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
			key, pnp := inst, "USB\\"+vp+"\\"+inst
			if parentInst != "" {
				key = parentInst
				pnp = "USB\\" + strings.TrimSuffix(vp, "&"+mi) + "\\" + parentInst
			}
			out[portName] = usbMeta{vid: vid, pid: pid, instance: key, pnp: pnp}
		}
	}
	return out
}

// singleInstance: <enumRoot>\<vidpid> 下若恰好一个实例则返回它（复合设备找父序列号）。
func singleInstance(rootKey registry.Key, enumRoot, vidpid string) string {
	k, err := registry.OpenKey(rootKey, enumRoot+`\`+vidpid,
		registry.READ|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ""
	}
	defer k.Close()
	insts, err := k.ReadSubKeyNames(-1)
	if err != nil || len(insts) != 1 {
		return ""
	}
	return insts[0]
}

// parseVIDPID: "VID_1A86&PID_7523" → vid/pid；复合接口形态 "VID_303A&PID_1001&MI_00"
// 额外返回 MI 段（"MI_00"，父键名去掉它即为父设备键）。
func parseVIDPID(vp string) (vid, pid, mi string, ok bool) {
	f := strings.Split(vp, "&")
	if len(f) < 2 || !strings.HasPrefix(f[0], "VID_") || !strings.HasPrefix(f[1], "PID_") {
		return "", "", "", false
	}
	vid = strings.ToLower(strings.TrimPrefix(f[0], "VID_"))
	pid = strings.ToLower(strings.TrimPrefix(f[1], "PID_"))
	if len(vid) != 4 || len(pid) != 4 {
		return "", "", "", false
	}
	if len(f) == 2 {
		return vid, pid, "", true
	}
	if len(f) == 3 && strings.HasPrefix(f[2], "MI_") {
		return vid, pid, f[2], true
	}
	return "", "", "", false
}
