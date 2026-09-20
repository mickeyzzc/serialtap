package signature

import "testing"

func TestFamilySignatures(t *testing.T) {
	eng := New(nil)
	cases := []struct {
		line string
		want string
	}{
		{"rst:0xc (SW_CPU_RESET)", "reset-banner"},
		{"boot:0x3b (SPI_FAST_FLASH_BOOT)", "boot-mode"},
		{"E (102345) httpd: httpd_accept: accept (23)", "esp-log-error"}, // E ( 先于 accept 命中
		{"accept (23)", "lwip-accept-err"},
		{"Guru Meditation Error: Core  0 panic'ed (StoreProhibited)", "guru-meditation"},
		{"Backtrace: 0x4008:0x3ffb1c30 |<-0x4008", "backtrace"},
		{"W (1023) wifi: ctrl_sock recvapi", ""}, // W 级日志不该命中
		{"I (1023) health: fps=12.3", ""},        // 普通信息行不该命中
		{"esp_restart: rebooting in 1s", "restart-call"},
		{"task_wdt: watchdog triggered", "watchdog"},
	}
	for _, c := range cases {
		name, ok := eng.Match(c.line)
		if c.want == "" {
			if ok {
				t.Errorf("误命中: %q → %s", c.line, name)
			}
			continue
		}
		if !ok || name != c.want {
			t.Errorf("签名错误: %q → (%s,%v) want %s", c.line, name, ok, c.want)
		}
	}
}

func TestExtraSignatures(t *testing.T) {
	eng := New([]string{`csi_heap_low`})
	if name, ok := eng.Match("csi_motion: csi_heap_low 108B"); !ok || name != "extra-0" {
		t.Fatalf("追加签名未生效: %s %v", name, ok)
	}
}

func TestBadExtraRegexSkipped(t *testing.T) {
	eng := New([]string{"[bad"})
	if _, ok := eng.Match("anything [bad"); ok {
		t.Fatal("坏正则应被跳过而非误报")
	}
}

func TestNamesAndEmptyLine(t *testing.T) {
	eng := New(nil)
	if got := len(eng.Names()); got != len(defaultSigs) {
		t.Fatalf("Names 数量不符: %d != %d", got, len(defaultSigs))
	}
	if _, ok := eng.Match(""); ok {
		t.Fatal("空行不该命中")
	}
}
