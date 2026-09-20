package device

import (
	"testing"

	"github.com/mickeyzzc/serialtap/internal/config"
)

// 命名链：配置 names → 内置 by-id 规则 → 内置 VID:PID 规则 → by-id 基名 → tty 名。
// VID 规则是 Windows/macOS 的主力定型途径（它们的 by-id 字符串形态与 Linux 不同）。
func TestBuildDevicesNameChain(t *testing.T) {
	ports := []string{"/dev/ttyA", "/dev/ttyB", "/dev/ttyC", "/dev/ttyD"}
	byID := map[string]string{
		"ttyA": "usb-1a86_USB_Serial-if00-port0", // 命中内置 by-id 规则 → ch340
		"ttyB": "some-custom-id",                 // 命中配置规则 → custom
		"ttyC": "",                               // 无 by-id；VID 规则 → esp32s3-jtag
	}
	byPath := map[string]string{"ttyA": "pA", "ttyB": "pB", "ttyC": "pC", "ttyD": "pD"}
	idf := func(tty string) (string, string) {
		switch tty {
		case "ttyA":
			return "1a86", "7523"
		case "ttyC":
			return "303a", "1001"
		case "ttyD":
			return "10c4", "ea60" // 未知芯片：回退 by-id 基名/tty 名
		}
		return "", ""
	}
	devs := buildDevices(ports, byID, byPath, idf, nil,
		[]config.NameRule{{Match: `some-custom-id`, Name: "custom"}})

	byKey := map[string]DeviceInfo{}
	for _, d := range devs {
		byKey[d.Key] = d
	}
	want := map[string]string{"pA": "ch340", "pB": "custom", "pC": "esp32s3-jtag"}
	for key, name := range want {
		if d, ok := byKey[key]; !ok || d.Name != name {
			t.Fatalf("key=%s 期望名 %q，实际 %+v", key, name, byKey[key])
		}
	}
	if d := byKey["pD"]; d.Name != "ttyD" {
		t.Fatalf("未知芯片应回退 tty 名: %+v", d)
	}
}

func TestApplyVIDRules(t *testing.T) {
	for _, c := range []struct{ vid, pid, want string }{
		{"1a86", "7523", "ch340"},
		{"1a86", "7522", "ch343"},
		{"1a86", "55d3", "ch343"},
		{"303a", "1001", "esp32s3-jtag"},
		{"10c4", "ea60", ""}, // CP210x：无内置规则，回退链继续
		{"1A86", "7523", ""}, // 大小写敏感：枚举层负责统一小写
	} {
		if got := applyVIDRules(c.vid, c.pid); got != c.want {
			t.Fatalf("applyVIDRules(%s,%s) = %q, want %q", c.vid, c.pid, got, c.want)
		}
	}
	if applyVIDRules("", "7523") != "" || applyVIDRules("1a86", "") != "" {
		t.Fatal("VID/PID 任一为空不应命中")
	}
}

func TestSanitizeName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"esp32s3-jtag", "esp32s3-jtag"},
		{"a b/c\\d:e", "a-b-c-d-e"},
		{"---", "dev"}, // 全非法 → 回退
		{"", "dev"},
		{"a/b\\c*d", "a-b-c-d"},
	} {
		if got := SanitizeName(c.in); got != c.want {
			t.Fatalf("SanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 幂等：sanitize(sanitize(x)) == sanitize(x)（含中文等非 ASCII）
	if got := SanitizeName("usb-1a86_USB Serial/我们"); got != SanitizeName(got) || len(got) == 0 {
		t.Fatalf("sanitize 不幂等或为空: %q", got)
	}
}
