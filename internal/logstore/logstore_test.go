package logstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSizeRotation(t *testing.T) {
	root := t.TempDir()
	w, err := NewDeviceWriter(root, "sizedev", 1)
	if err != nil {
		t.Fatal(err)
	}
	w.maxBytes = 200 // 直接压小阈值触发轮转
	for i := 0; i < 20; i++ {
		if err := w.WriteLine(strings.Repeat("a", 30)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	m, _ := filepath.Glob(filepath.Join(root, "sizedev", "serial-*.log"))
	if len(m) < 2 {
		t.Fatalf("大小轮转未触发: %v", m)
	}
}

func TestDualChannelFiles(t *testing.T) {
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
	if !strings.HasPrefix(string(ser), "[") {
		t.Fatalf("serial 行缺时间戳前缀: %q", ser)
	}
	ev, err := os.ReadFile(filepath.Join(root, "evdev", "events-"+day+".log"))
	if err != nil || !strings.Contains(string(ev), "[collector] started") {
		t.Fatalf("events 日志缺失: %v %q", err, ev)
	}
	if w.Name() != "evdev" || !strings.HasSuffix(w.Dir(), "evdev") {
		t.Fatalf("Dir/Name 错误: %q %q", w.Name(), w.Dir())
	}
}

func TestSweepRetention(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "dev1", "serial-20200101.log")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("x"), 0o644)
	past := time.Now().AddDate(0, 0, -30)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(root, "dev1", "serial-today.log")
	os.WriteFile(fresh, []byte("x"), 0o644)

	if n, err := SweepRetention(root, 0); err != nil || n != 0 {
		t.Fatalf("retention=0 应为无操作: n=%d err=%v", n, err)
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
	if n, err := SweepRetention(filepath.Join(root, "nope"), 14); err != nil || n != 0 {
		t.Fatalf("不存在根目录应安静返回: n=%d err=%v", n, err)
	}
}

func TestStampFormat(t *testing.T) {
	ts := Stamp(time.Date(2026, 9, 20, 11, 22, 33, 444000000, time.UTC))
	if len(ts) != len("2026-09-20 11:22:33.444") {
		t.Fatalf("Stamp 格式长度异常: %q", ts)
	}
}

// —— issue #5：重启后大小轮转后缀不得与既有文件撞车 ——
func TestSizeRotationSuffixSkipsExistingAfterRestart(t *testing.T) {
	root := t.TempDir()
	day := time.Now().Format("20060102")
	dir := filepath.Join(root, "rdev")
	os.MkdirAll(dir, 0o755)
	// 模拟"上次运行遗留的 .001"
	os.WriteFile(filepath.Join(dir, "serial-"+day+".001.log"), []byte("OLD"), 0o644)

	w, _ := NewDeviceWriter(root, "rdev", 1)
	w.maxBytes = 50
	for i := 0; i < 10; i++ {
		if err := w.WriteLine(strings.Repeat("a", 20)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	old, err := os.ReadFile(filepath.Join(dir, "serial-"+day+".001.log"))
	if err != nil || string(old) != "OLD" {
		t.Fatalf("遗留 .001 被污染: %q err=%v", old, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "serial-"+day+".002.log")); err != nil {
		t.Fatalf("新轮转文件应为 .002: %v", err)
	}
}

// —— issue #5：events 通道也要按大小轮转 ——
func TestEventsRotateBySize(t *testing.T) {
	root := t.TempDir()
	w, _ := NewDeviceWriter(root, "evrot", 1)
	w.eventsMaxBytes = 60 // 直接压小阈值
	for i := 0; i < 10; i++ {
		if err := w.WriteEvent(strings.Repeat("e", 20)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	day := time.Now().Format("20060102")
	m, _ := filepath.Glob(filepath.Join(root, "evrot", "events-"+day+"*.log"))
	if len(m) < 2 {
		t.Fatalf("events 未按大小轮转: %v", m)
	}
}
