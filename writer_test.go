package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterDailyRotationAndSizeCap(t *testing.T) {
	root := t.TempDir()
	w, err := NewDeviceWriter(root, "testdev", 0) // 0MB → maxBytes=0 → 不按大小轮转？见下
	if err != nil {
		t.Fatal(err)
	}
	// 大小轮转：用小上限
	w2, err := NewDeviceWriter(root, "sizedev", 1)
	if err != nil {
		t.Fatal(err)
	}
	w2.maxBytes = 200 // 直接压小阈值触发轮转
	for i := 0; i < 20; i++ {
		if err := w2.WriteLine(strings.Repeat("a", 30)); err != nil {
			t.Fatal(err)
		}
	}
	w2.Close()
	m, _ := filepath.Glob(filepath.Join(root, "sizedev", "serial-*.log"))
	if len(m) < 2 {
		t.Fatalf("大小轮转未触发: %v", m)
	}
	_ = w
	w.Close()
}

func TestWriterEventsFile(t *testing.T) {
	root := t.TempDir()
	w, _ := NewDeviceWriter(root, "evdev", 64)
	defer w.Close()
	if err := w.WriteLine("hello world"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEvent("[collector] started"); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Format("20060102")
	ser, err := os.ReadFile(filepath.Join(root, "evdev", "serial-"+day+".log"))
	if err != nil || !strings.Contains(string(ser), "hello world") {
		t.Fatalf("serial 日志缺失: %v %q", err, ser)
	}
	ev, err := os.ReadFile(filepath.Join(root, "evdev", "events-"+day+".log"))
	if err != nil || !strings.Contains(string(ev), "[collector] started") {
		t.Fatalf("events 日志缺失: %v %q", err, ev)
	}
	if !strings.HasPrefix(string(ser), "[") {
		t.Fatalf("serial 行缺时间戳前缀: %q", ser)
	}
}

func TestSweepRetention(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "dev1", "serial-20200101.log")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 把 mtime 拨回 30 天前
	past := time.Now().AddDate(0, 0, -30)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(root, "dev1", "serial-today.log")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := SweepRetention(root, 14)
	if err != nil || n != 1 {
		t.Fatalf("清理数量错误: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("旧文件未删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("新文件被误删")
	}
}

func TestPauseStateMatching(t *testing.T) {
	root := t.TempDir()
	pauseFile := PauseFilePath(root)
	content := "# 注释\nch340\n.*ttyACM1\n"
	if err := os.WriteFile(pauseFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _, err := LoadPauseFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Matches(DeviceInfo{Name: "ch340", Tty: "/dev/ttyUSB0"}) {
		t.Fatal("ch340 应被暂停")
	}
	if !st.Matches(DeviceInfo{Tty: "/dev/ttyACM1", Name: "esp32s3-jtag"}) {
		t.Fatal("ttyACM1 正则应被暂停")
	}
	if st.Matches(DeviceInfo{Name: "esp32s3-jtag", Tty: "/dev/ttyACM0"}) {
		t.Fatal("不该暂停")
	}
	// 空状态（无文件）
	st2, _, _ := LoadPauseFile(t.TempDir())
	if st2.Matches(DeviceInfo{Name: "any"}) {
		t.Fatal("无 PAUSED 文件时不应有暂停")
	}
}
