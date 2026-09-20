//go:build darwin

package device

import (
	"os"
	"regexp"
	"strings"

	"github.com/mickeyzzc/serialtap/internal/config"
)

// macOS 枚举：无纯 Go 的 IOKit 途径（不引 CGO），身份直接建立在 cu.* 设备名上 ——
// macOS 的设备名本身承载身份：无序列号设备（如 CH340）的名字由系统按 USB 位置
// 编号生成（cu.usbserial-1420），有序列号设备名字内嵌序列号（cu.usbmodem<序列号>）。
// tty 基名即稳定 key：同型号不撞车、换口即换身份，与 Linux by-path 语义一致。
// 走 cu.*（callout）而非 tty.*：不阻塞在调制解调器控制线上，且天然去重。
// 蓝牙/内置串口（cu.Bluetooth-Modem、cu.debug 等）按前缀过滤。
var darwinUSBName = regexp.MustCompile(`^cu\.(usb|wch|SLAB|USA)`)

func Enumerate(exclude []*regexp.Regexp, names []config.NameRule) ([]DeviceInfo, error) {
	ents, err := os.ReadDir("/dev")
	if err != nil {
		return nil, err
	}
	var ports []string
	byID := map[string]string{}
	byPath := map[string]string{}
	for _, e := range ents {
		if e.IsDir() || !darwinUSBName.MatchString(e.Name()) {
			continue
		}
		base := strings.TrimPrefix(e.Name(), "cu.")
		ports = append(ports, "/dev/"+e.Name())
		byID[base] = base
		byPath[base] = base
	}
	// VID:PID 无纯 Go 途径；命名链剩 配置 names → by-id 基名。
	// 要精确命名（如 esp32s3-jtag）请在配置 names 里按 usbmodem/usbserial 前缀细分。
	return buildDevices(ports, byID, byPath, func(string) (string, string) { return "", "" },
		exclude, names), nil
}
