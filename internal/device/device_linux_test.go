//go:build linux

package device

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/mickeyzzc/serialtap/internal/config"
)

func TestBuildDevicesFilteringAndNaming(t *testing.T) {
	ports := []string{"/dev/ttyS0", "/dev/ttyUSB0", "/dev/ttyACM1", "/dev/ttyACM0"}
	byID := map[string]string{
		"ttyUSB0": "usb-1a86_USB_Serial-if00-port0",
		"ttyACM1": "usb-1a86_USB_Single_Serial_ABC123-if00",
		"ttyACM0": "usb-Espressif_USB_JTAG_serial_debug_unit_XX-if00",
	}
	byPath := map[string]string{
		"ttyUSB0": "pci-0-usb-0:4:1.0-port0", // ttyS0 故意没有 by-path → 必须被滤掉
		"ttyACM1": "pci-0-usb-0:3.3:1.0",
		"ttyACM0": "pci-0-usb-0:3.2:1.0",
	}
	ids := map[string][2]string{
		"ttyUSB0": {"1a86", "7523"},
		"ttyACM1": {"1a86", "55d3"},
		"ttyACM0": {"303a", "1001"},
	}
	id := func(tty string) (string, string) {
		pair := ids[tty]
		return pair[0], pair[1]
	}
	devs := buildDevices(ports, byID, byPath, id, nil, nil)
	if len(devs) != 3 {
		t.Fatalf("ttyS0 应被过滤，得 %d 个: %+v", len(devs), devs)
	}
	names := map[string]string{}
	for _, d := range devs {
		names[d.Name] = d.Tty
	}
	if names["ch340"] != "/dev/ttyUSB0" || names["ch343"] != "/dev/ttyACM1" || names["esp32s3-jtag"] != "/dev/ttyACM0" {
		t.Fatalf("内置命名错误: %+v", devs)
	}
	if devs[0].Key >= devs[1].Key || devs[1].Key >= devs[2].Key {
		t.Fatalf("未按 key 排序: %+v", devs)
	}
}

func TestBuildDevicesConfigNamesWinAndExclude(t *testing.T) {
	ports := []string{"/dev/ttyUSB0"}
	byID := map[string]string{"ttyUSB0": "usb-1a86_USB_Serial-if00-port0"}
	byPath := map[string]string{"ttyUSB0": "k1"}
	id := func(string) (string, string) { return "1a86", "7523" }

	devs := buildDevices(ports, byID, byPath, id, nil,
		[]config.NameRule{{Match: `USB_Serial-if00`, Name: "my-board"}})
	if len(devs) != 1 || devs[0].Name != "my-board" {
		t.Fatalf("配置命名未生效: %+v", devs)
	}

	excl, _ := CompilePatterns([]string{`USB_Serial-if00`})
	if got := buildDevices(ports, byID, byPath, id, excl, nil); len(got) != 0 {
		t.Fatalf("exclude by-id 失败: %+v", got)
	}
	excl2, _ := CompilePatterns([]string{`^my-board$`})
	if got := buildDevices(ports, byID, byPath, id, excl2,
		[]config.NameRule{{Match: "USB_Serial", Name: "my-board"}}); len(got) != 0 {
		t.Fatalf("exclude name 失败: %+v", got)
	}

	byID2 := map[string]string{"ttyUSB0": "weird-device"}
	if got := buildDevices(ports, byID2, byPath, id, nil, nil); len(got) != 1 || got[0].Name != "weird-device" {
		t.Fatalf("by-id 兜底命名失败: %+v", got)
	}
}

func TestSymlinkIndex(t *testing.T) {
	dir := t.TempDir()
	os.Symlink("../../ttyUSB0", filepath.Join(dir, "usb-1a86_Serial-if00-port0"))
	os.Symlink("../../ttyACM0", filepath.Join(dir, "usb-JTAG-if00"))
	idx := symlinkIndex(dir)
	if idx["ttyUSB0"] != "usb-1a86_Serial-if00-port0" || idx["ttyACM0"] != "usb-JTAG-if00" {
		t.Fatalf("索引错误: %+v", idx)
	}
	if got := symlinkIndex(filepath.Join(dir, "nope")); len(got) != 0 {
		t.Fatal("不存在目录应返回空")
	}
}

func TestSysfsIDAtFakeTree(t *testing.T) {
	base := t.TempDir()
	usbDev := filepath.Join(base, "1-4")
	iface := filepath.Join(usbDev, "1-4:1.0")
	ttyDir := filepath.Join(iface, "ttyUSB0")
	os.MkdirAll(ttyDir, 0o755)
	os.WriteFile(filepath.Join(usbDev, "idVendor"), []byte("1a86\n"), 0o644)
	os.WriteFile(filepath.Join(usbDev, "idProduct"), []byte("7523\n"), 0o644)
	// 挂载点 <base>/ttyUSB0/device → 真实 tty 目录（两级之上才有 idVendor）
	os.MkdirAll(filepath.Join(base, "ttyUSB0"), 0o755)
	os.Symlink(ttyDir, filepath.Join(base, "ttyUSB0", "device"))

	vid, pid := sysfsIDAt(base, "ttyUSB0")
	if vid != "1a86" || pid != "7523" {
		t.Fatalf("sysfsIDAt 解析错误: %s:%s", vid, pid)
	}
	if v, p := sysfsIDAt(base, "ttyNONE"); v+p != "" {
		t.Fatal("不存在设备应返回空")
	}
}

func TestCompilePatternsSkipsBad(t *testing.T) {
	good, bad := CompilePatterns([]string{"ok-pattern", "[bad", ""})
	if len(good) != 1 || len(bad) != 1 || bad[0] != "[bad" {
		t.Fatalf("坏正则应被跳过: good=%d bad=%v", len(good), bad)
	}
}

func TestEnumerateRealNoError(t *testing.T) {
	// 真实 sysfs：任何机器上都不该报错（无设备 → 空表）
	devs, err := Enumerate(nil, nil)
	if err != nil {
		t.Fatalf("Enumerate 不应报错: %v", err)
	}
	for _, d := range devs {
		if d.Key == "" || d.Name == "" {
			t.Fatalf("设备身份不完整: %+v", d)
		}
		if regexp.MustCompile(`^/dev/tty`).FindString(d.Tty) == "" {
			t.Fatalf("tty 路径异常: %+v", d)
		}
	}
}

// （TestSanitizeName 已上移到跨平台的 device_test.go）

// PortHolders: 用假 /proc 树验证 fd 符号链接扫描（含"不报自己"语义）
func TestPortHoldersAtFakeProc(t *testing.T) {
	proc := t.TempDir()
	// 假进程 4242：fd 3 → /dev/ttyFAKE0；fd 4 → 别的
	pDir := filepath.Join(proc, "4242", "fd")
	os.MkdirAll(pDir, 0o755)
	os.Symlink("/dev/ttyFAKE0", filepath.Join(pDir, "3"))
	os.Symlink("/dev/null", filepath.Join(pDir, "4"))
	// 假进程 5353：不持有
	os.MkdirAll(filepath.Join(proc, "5353", "fd"), 0o755)

	got := PortHoldersAt(proc, "/dev/ttyFAKE0")
	if len(got) != 1 || got[0] != 4242 {
		t.Fatalf("持有者识别错误: %v", got)
	}
	if got := PortHoldersAt(proc, "/dev/ttyNONE"); len(got) != 0 {
		t.Fatalf("无人持有应返回空: %v", got)
	}
	// 非进程目录被忽略
	os.MkdirAll(filepath.Join(proc, "notapid", "fd"), 0o755)
	if got := PortHoldersAt(proc, "/dev/ttyFAKE0"); len(got) != 1 {
		t.Fatalf("非 PID 目录干扰: %v", got)
	}
}
