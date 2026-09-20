package analyze

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickeyzzc/serialtap/internal/config"
)

func TestLogsChronologicalFirstLast(t *testing.T) {
	dir := t.TempDir()
	newer := filepath.Join(dir, "b.log")
	older := filepath.Join(dir, "a.log")
	os.WriteFile(newer, []byte("[2026-06-01 10:00:00.000] rst:0x2 (X)\n"), 0o644)
	os.WriteFile(older, []byte("[2026-01-01 08:00:00.000] rst:0x1 (Y)\n"), 0o644)

	var buf bytes.Buffer
	// 故意按"新文件在前"传入 —— FIRST 列必须取最早时间
	if err := Logs(&buf, []string{newer, older}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	f := strings.Index(out, "2026-01-01")
	l := strings.Index(out, "2026-06-01")
	if f < 0 || l < 0 || f > l {
		t.Fatalf("FIRST 应早于 LAST: %s", out)
	}
}

func TestLogsNoMatchAndShowLines(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "empty.log")
	os.WriteFile(f, []byte("I (1) all: fine\n[2026-01-01 00:00:00.000] W (2) w: warn\n"), 0o644)

	var buf bytes.Buffer
	if err := Logs(&buf, []string{f}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "未命中任何签名") {
		t.Fatalf("无命中提示缺失: %q", buf.String())
	}
	// 缺文件 → 报错
	if err := Logs(&bytes.Buffer{}, []string{filepath.Join(dir, "nope.log")}, false); err == nil {
		t.Fatal("缺文件应报错")
	}
}

func TestBacktraceAddrs(t *testing.T) {
	addrs := backtraceAddrs("Backtrace: 0x400D0C5A:0x3FFB7D40 0x4008C71E:0x3FFB7D60")
	if len(addrs) != 2 || addrs[0] != "0x400D0C5A" || addrs[1] != "0x4008C71E" {
		t.Fatalf("地址帧提取错误: %v", addrs)
	}
	addrs = backtraceAddrs("Backtrace: 0x4008:0x3ffb1c30 |<-0x4008")
	if len(addrs) != 2 || addrs[1] != "0x4008" {
		t.Fatalf("|<-PC 标记提取错误: %v", addrs)
	}
	if got := backtraceAddrs("no addresses here"); got != nil {
		t.Fatalf("无地址行应返回 nil: %v", got)
	}
}

func TestElfFromLogPath(t *testing.T) {
	cfg := config.Config{ElfMap: map[string]string{"esp32s3-jtag": "/x/y.elf"}}
	if got := elfFromLogPath("logs/esp32s3-jtag/serial-20260920.log", cfg); got != "/x/y.elf" {
		t.Fatalf("elf_map 映射失败: %q", got)
	}
	if got := elfFromLogPath("one.log", cfg); got != "" {
		t.Fatalf("短路径应返回空: %q", got)
	}
	if got := elfFromLogPath("logs/unknown/serial.log", cfg); got != "" {
		t.Fatalf("未映射设备应返回空: %q", got)
	}
}

func TestFindAddr2line(t *testing.T) {
	if p, err := findAddr2line("/bin/echo"); err != nil || p == "" {
		t.Fatalf("显式指定失败: %q %v", p, err)
	}
	if _, err := findAddr2line("/nonexistent/a2l"); err == nil {
		t.Fatal("无效路径应报错")
	}
	t.Setenv("ESP_ADDR2LINE", "/usr/local/my-a2l")
	if p, _ := findAddr2line(""); p != "/usr/local/my-a2l" {
		t.Fatalf("环境变量优先级错误: %q", p)
	}
}

func TestDecodeBacktraceWithFakeAddr2line(t *testing.T) {
	dir := t.TempDir()
	elf := filepath.Join(dir, "fake.elf")
	os.WriteFile(elf, []byte("ELF"), 0o644)
	a2l := filepath.Join(dir, "fake-addr2line")
	os.WriteFile(a2l, []byte("#!/bin/sh\necho \"$0 $@\"\n"), 0o755)

	log := filepath.Join(dir, "serial.log")
	os.WriteFile(log, []byte("[ts] Backtrace: 0x400D0C5A:0x3FFB7D40 0x4008C71E:0x3FFB7D60\n"), 0o644)

	if err := DecodeBacktrace(log, elf, a2l, config.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := DecodeBacktrace(log, "", a2l, config.Config{}); err == nil {
		t.Fatal("无 elf 应报错")
	}
	if err := DecodeBacktrace(log, filepath.Join(dir, "nope.elf"), a2l, config.Config{}); err == nil {
		t.Fatal("缺 elf 文件应报错")
	}
	if err := DecodeBacktrace(log, elf, filepath.Join(dir, "no-a2l"), config.Config{}); err == nil {
		t.Fatal("缺 addr2line 应报错")
	}
	plain := filepath.Join(dir, "plain.log")
	os.WriteFile(plain, []byte("nothing here\n"), 0o644)
	if err := DecodeBacktrace(plain, elf, a2l, config.Config{}); err != nil {
		t.Fatal(err)
	}
}
