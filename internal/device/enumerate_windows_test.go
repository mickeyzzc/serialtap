//go:build windows

package device

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// 注册表 fixture 放 HKCU 下（无需管理员），树形清理。
const testEnumRoot = `Software\serialtap-test`

func mustCreateKey(t *testing.T, path string) {
	t.Helper()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("建注册表键失败 %s: %v", path, err)
	}
	k.Close()
}

func mustSetString(t *testing.T, path, name, val string) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("开注册表键失败 %s: %v", path, err)
	}
	defer k.Close()
	if err := k.SetStringValue(name, val); err != nil {
		t.Fatalf("写 %s\\%s 失败: %v", path, name, err)
	}
}

// regDeleteTree: 深度优先递归删除（registry.DeleteKey 只能删空键）。
func regDeleteTree(path string) {
	k, err := registry.OpenKey(registry.CURRENT_USER, path,
		registry.READ|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return
	}
	sub, _ := k.ReadSubKeyNames(-1)
	k.Close()
	for _, s := range sub {
		regDeleteTree(path + `\` + s)
	}
	_ = registry.DeleteKey(registry.CURRENT_USER, path)
}

// 假的 USB Enum 树：CH340（无序列号 → 位置实例）、ESP32-S3（MAC 实例）、
// 一个畸形键、一个非 COM 的 PortName —— 后两者必须被忽略。
func buildFakeEnumTree(t *testing.T) string {
	t.Helper()
	root := testEnumRoot + `\` + filepath.Base(t.TempDir())
	mustCreateKey(t, root+`\USB\VID_1A86&PID_7523\5&deadbeef&0&2\Device Parameters`)
	mustSetString(t, root+`\USB\VID_1A86&PID_7523\5&deadbeef&0&2\Device Parameters`, "PortName", "COM77")

	mustCreateKey(t, root+`\USB\VID_303A&PID_1001\48:27:E2:AA:BB:CC\Device Parameters`)
	mustSetString(t, root+`\USB\VID_303A&PID_1001\48:27:E2:AA:BB:CC\Device Parameters`, "PortName", "COM88")

	// 非 COM 的 PortName（某些杂项设备）→ 忽略
	mustCreateKey(t, root+`\USB\VID_1A86&PID_7523\9&ffff&0&9\Device Parameters`)
	mustSetString(t, root+`\USB\VID_1A86&PID_7523\9&ffff&0&9\Device Parameters`, "PortName", "NOTACOM")

	// 畸形 VID:PID 键名 → 忽略
	mustCreateKey(t, root+`\USB\SOMETHINGELSE\inst\Device Parameters`)
	mustSetString(t, root+`\USB\SOMETHINGELSE\inst\Device Parameters`, "PortName", "COM66")

	t.Cleanup(func() { regDeleteTree(testEnumRoot) })
	return root + `\USB`
}

func TestUSBSerialMetaAt(t *testing.T) {
	meta := usbSerialMetaAt(registry.CURRENT_USER, buildFakeEnumTree(t))
	if len(meta) != 2 {
		t.Fatalf("应识别 2 个 USB 串口: %+v", meta)
	}
	m77, ok := meta["COM77"]
	if !ok || m77.vid != "1a86" || m77.pid != "7523" || m77.instance != "5&deadbeef&0&2" {
		t.Fatalf("COM77 元数据错误: %+v", m77)
	}
	m88, ok := meta["COM88"]
	if !ok || m88.vid != "303a" || m88.pid != "1001" || m88.instance != "48:27:E2:AA:BB:CC" {
		t.Fatalf("COM88 元数据错误: %+v", m88)
	}
	if _, bad := meta["COM66"]; bad {
		t.Fatal("畸形 VID:PID 键不应入表")
	}
	if _, bad := meta["NOTACOM"]; bad {
		t.Fatal("非 COM 的 PortName 不应入表")
	}
}

// 端口清单 + 注册表元数据 → 完整身份：Key=实例、名字走 VID 规则、
// 非 USB 串口（无元数据）被过滤。
func TestDevicesFromMeta(t *testing.T) {
	meta := map[string]usbMeta{
		"COM77": {vid: "1a86", pid: "7523", instance: "5&deadbeef&0&2"},
		"COM88": {vid: "303a", pid: "1001", instance: "48:27:E2:AA:BB:CC"},
	}
	devs := devicesFromMeta([]string{"COM77", "COM88", "COM99"}, meta, nil, nil)
	if len(devs) != 2 {
		t.Fatalf("COM99（非 USB）应被过滤: %+v", devs)
	}
	for _, d := range devs {
		switch d.Tty {
		case "COM77":
			if d.Key != "5&deadbeef&0&2" || d.Name != "ch340" || !strings.HasPrefix(d.ByID, `USB\VID_1A86&PID_7523\`) {
				t.Fatalf("COM77 身份错误: %+v", d)
			}
		case "COM88":
			if d.Key != "48:27:E2:AA:BB:CC" || d.Name != "esp32s3-jtag" || !strings.HasPrefix(d.ByID, `USB\VID_303A&PID_1001\`) {
				t.Fatalf("COM88 身份错误: %+v", d)
			}
		}
	}
}
