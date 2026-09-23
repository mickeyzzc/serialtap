//go:build darwin

package device

import (
	"errors"
	"sync/atomic"
	"testing"
)

// 夹具按本机实测的 ioreg IOService 平面格式构造（节点每层缩进 2 列；
// 属性行 "Key" = Value；IOSerialBSDClient 挂在 USB 设备驱动的子树里）。
const ioregFixture = `+-o Root  <class IORegistryEntry, id 0x100000100, retain 28>
  | {
  |   "IOKitBuildVersion" = "Darwin Kernel Version 25.6.0"
  | }
  |
  +-o AppleT8103USBXHCI@01000000  <class AppleT8103USBXHCI, id 0x1000003a9, registered, matched, active, busy 0 (21 ms), retain 39>
  | {
  |   "locationID" = 16777216
  | }
  |
  +-o USB Serial@01420000  <class IOUSBHostDevice, id 0x100000f00, registered, matched, active, busy 0 (4 ms), retain 22>
  | {
  |   "USB Product Name" = "USB Serial"
  |   "idVendor" = 6790
  |   "idProduct" = 29987
  |   "USB Serial Number" = "A50285BI"
  |   "locationID" = 21102592
  | }
  |
  | +-o AppleUSBACMData  <class AppleUSBACMData, id 0x100000f02, registered, matched, active, retain 9>
  | | {
  | |   "IOClass" = "AppleUSBACMData"
  | | }
  | |
  | | +-o IOSerialBSDClient  <class IOSerialBSDClient, id 0x100000f03, registered, matched, active, busy 0 (0 ms), retain 5>
  | |   {
  | |     "IOCalloutDevice" = "/dev/cu.usbmodem14201"
  | |     "IODialinDevice" = "/dev/tty.usbmodem14201"
  | |   }
  | |
  +-o ESP32-S3@02100000  <class IOUSBHostDevice, id 0x100000f10, registered, matched, active, busy 0 (2 ms), retain 19>
  | {
  |   "idVendor" = 12346
  |   "idProduct" = 4097
  |   "USB Serial Number" = "3C:6E:F7:A1:B2:C3"
  |   "locationID" = 34603008
  | }
  |
  | +-o IOSerialBSDClient  <class IOSerialBSDClient, id 0x100000f13, registered, matched, active, busy 0 (0 ms), retain 5>
  |   {
  |     "IOCalloutDevice" = "/dev/cu.usbmodem2101"
  |   }
  |
  +-o wlan-debug  <class AppleSamsungSerial, id 0x1000006eb, !registered, !matched, active, busy 0, retain 7>
    {
      "IOClass" = "AppleSamsungSerial"
    }
    |
    +-o IOSerialBSDClient  <class IOSerialBSDClient, id 0x1000006ee, registered, matched, active, busy 0 (16 ms), retain 5>
      {
        "IOCalloutDevice" = "/dev/cu.wlan-debug"
      }
`

func TestParseIORegUSBIdentity(t *testing.T) {
	got := parseIOReg(ioregFixture)
	if len(got) != 2 {
		t.Fatalf("应识别 2 个 USB 串口客户端: %+v", got)
	}
	ch := got["cu.usbmodem14201"]
	if ch.vid != "1a86" || ch.pid != "7523" || ch.serial != "A50285BI" || ch.loc != "01420000" {
		t.Fatalf("CH340 身份错误: %+v", ch)
	}
	esp := got["cu.usbmodem2101"]
	if esp.vid != "303a" || esp.pid != "1001" || esp.serial != "3C:6E:F7:A1:B2:C3" || esp.loc != "02100000" {
		t.Fatalf("ESP32-S3 身份错误: %+v", esp)
	}
	// 无 USB 祖先的本机串口：不产出身份（不进表）
	if _, ok := got["cu.wlan-debug"]; ok {
		t.Fatal("非 USB 串口不应进身份表")
	}
}

func TestParseIORegMalformedLines(t *testing.T) {
	got := parseIOReg("not a registry\n\"orphan\" = 1\n +-o odd indent  <class X>\n+-o ok  <class X>\n  \"k\" = 1\n")
	if len(got) != 0 {
		t.Fatalf("坏输入应产出空表: %+v", got)
	}
}

func TestEnumerateDarwinEndToEnd(t *testing.T) {
	defer func() {
		identMu.Lock()
		identCache = map[string]usbIdentity{}
		identMu.Unlock()
	}()
	var ioregRuns atomic.Int32
	identCache = map[string]usbIdentity{}
	oldIOReg, oldPorts := ioregSource, portsSource
	ioregSource = func() (string, error) { ioregRuns.Add(1); return ioregFixture, nil }
	portsSource = func() ([]string, error) {
		return []string{"/dev/cu.usbmodem2101", "/dev/tty.usbmodem14201",
			"/dev/cu.usbmodem14201", "/dev/cu.Bluetooth-Incoming-Port"}, nil
	}
	defer func() { ioregSource, portsSource = oldIOReg, oldPorts }()

	devs, err := Enumerate(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("应只保留 cu.usb* 两个设备: %+v", devs)
	}
	byTty := map[string]DeviceInfo{}
	for _, d := range devs {
		byTty[d.Tty] = d
	}
	ch := byTty["/dev/cu.usbmodem14201"]
	if ch.Name != "ch340" || ch.Key != "usb-01420000" || ch.ByID != "usb-1a86_7523-A50285BI" {
		t.Fatalf("CH340 身份/命名错误: %+v", ch)
	}
	if ch.VID != "1a86" || ch.PID != "7523" {
		t.Fatalf("VID:PID 错误: %s:%s", ch.VID, ch.PID)
	}
	esp := byTty["/dev/cu.usbmodem2101"]
	if esp.Name != "esp32s3-jtag" || esp.Key != "usb-02100000" {
		t.Fatalf("ESP32 身份/命名错误: %+v", esp)
	}
	if devs[0].Key >= devs[1].Key {
		t.Fatalf("未按 key 排序: %+v", devs)
	}

	// exclude：按名字精确滤掉一台
	excl, _ := CompilePatterns([]string{"^ch340$"})
	got, _ := Enumerate(excl, nil)
	if len(got) != 1 || got[0].Name != "esp32s3-jtag" {
		t.Fatalf("exclude 失败: %+v", got)
	}

	// 稳态轮询不重跑 ioreg（身份缓存命中）
	if n := ioregRuns.Load(); n != 1 {
		t.Fatalf("ioreg 应只跑一次，实际 %d 次", n)
	}

	// ioreg 失败 → tty 名兜底，不阻塞采集
	identCache = map[string]usbIdentity{}
	ioregSource = func() (string, error) { return "", errFakeIOReg }
	got, _ = Enumerate(nil, nil)
	if len(got) != 2 || got[0].Key != "usbmodem14201" {
		t.Fatalf("ioreg 失败应退化为 tty 名身份: %+v", got)
	}
}

var errFakeIOReg = errors.New("fake ioreg failure")
