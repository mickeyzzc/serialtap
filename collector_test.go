package main

import (
	"strings"
	"testing"
)

func TestLineAssemblerSplitsAcrossChunks(t *testing.T) {
	var a lineAssembler
	lines := a.feed([]byte("rst:0xc (SW_C"))
	if len(lines) != 0 {
		t.Fatalf("半行不应提前吐出: %v", lines)
	}
	lines = a.feed([]byte("PU_RESET)\nE (102) cam: boom\r\npartial tail"))
	want := []string{"rst:0xc (SW_CPU_RESET)", "E (102) cam: boom"}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("行拼装错误: %v want %v", lines, want)
	}
	if got := a.flush(); got != "partial tail" {
		t.Fatalf("flush 残余错误: %q", got)
	}
}

func TestLineAssemblerFloodProtection(t *testing.T) {
	var a lineAssembler
	big := strings.Repeat("x", maxTail+100) // 无换行的泥石流
	lines := a.feed([]byte(big))
	if len(lines) != 1 || len(lines[0]) != len(big) {
		t.Fatalf("泥石流保护失败: %d 行", len(lines))
	}
	if len(a.tail) != 0 {
		t.Fatalf("缓冲未清空")
	}
}

func TestSignatureEngineFamilySignatures(t *testing.T) {
	eng := NewSignatureEngine(nil)
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

func TestSignatureEngineExtra(t *testing.T) {
	eng := NewSignatureEngine([]string{`csi_heap_low`})
	if name, ok := eng.Match("csi_motion: csi_heap_low 108B"); !ok || name != "extra-0" {
		t.Fatalf("追加签名未生效: %s %v", name, ok)
	}
}

func TestSanitizeName(t *testing.T) {
	if got := SanitizeName("usb-1a86_USB Serial/我们"); strings.ContainsAny(got, "/ 我们") {
		t.Fatalf("非法字符残留: %q", got)
	}
	if SanitizeName("...") == "" {
		t.Fatalf("不应返回空名")
	}
}
