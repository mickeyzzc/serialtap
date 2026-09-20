package pause

import (
	"os"
	"testing"

	"github.com/mickeyzzc/serialtap/internal/device"
)

func TestMatchesFields(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(PauseFilePath(root), []byte("# 注释\nch340\n.*ttyACM1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _, err := LoadPauseFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Matches(device.DeviceInfo{Name: "ch340", Tty: "/dev/ttyUSB0"}) {
		t.Fatal("ch340 应被暂停")
	}
	if !st.Matches(device.DeviceInfo{Tty: "/dev/ttyACM1", Name: "esp32s3-jtag"}) {
		t.Fatal("ttyACM1 正则应被暂停")
	}
	if st.Matches(device.DeviceInfo{Name: "esp32s3-jtag", Tty: "/dev/ttyACM0"}) {
		t.Fatal("不该暂停")
	}
	if st.Len() != 2 {
		t.Fatalf("模式条数错误: %d", st.Len())
	}
}

func TestNoFileMeansNoPause(t *testing.T) {
	st, mtime, err := LoadPauseFile(t.TempDir())
	if err != nil || !mtime.IsZero() {
		t.Fatalf("无 PAUSED 文件应零值 mtime: %v %v", mtime, err)
	}
	if st.Matches(device.DeviceInfo{Name: "any"}) {
		t.Fatal("无 PAUSED 文件时不应有暂停")
	}
}

func TestReplaceWithAndClear(t *testing.T) {
	a := NewPauseState()
	root := t.TempDir()
	os.WriteFile(PauseFilePath(root), []byte("x\n"), 0o644)
	b, _, _ := LoadPauseFile(root)
	if a.Len() != 0 {
		t.Fatal("初始应为空")
	}
	a.ReplaceWith(b)
	if a.Len() != 1 || !a.Matches(device.DeviceInfo{Name: "x"}) {
		t.Fatal("ReplaceWith 未生效")
	}
	if !a.Clear() {
		t.Fatal("Clear 应报告有变化")
	}
	if a.Clear() {
		t.Fatal("二次 Clear 应无变化")
	}
	if a.Len() != 0 {
		t.Fatal("Clear 未清空")
	}
}

func TestCLIFileEditing(t *testing.T) {
	root := t.TempDir()
	if err := PauseCLI(root, true, []string{"ch340", "ttyACM1"}); err != nil {
		t.Fatal(err)
	}
	st, _, err := LoadPauseFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Matches(device.DeviceInfo{Name: "ch340"}) || !st.Matches(device.DeviceInfo{Tty: "/dev/ttyACM1"}) {
		t.Fatal("pause 清单未生效")
	}
	if err := PauseCLI(root, false, []string{"ch340"}); err != nil {
		t.Fatal(err)
	}
	st, _, _ = LoadPauseFile(root)
	if st.Matches(device.DeviceInfo{Name: "ch340"}) || !st.Matches(device.DeviceInfo{Tty: "/dev/ttyACM1"}) {
		t.Fatal("resume 单条失败")
	}
	if err := PauseCLI(root, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PauseFilePath(root)); !os.IsNotExist(err) {
		t.Fatal("清空后 PAUSED 应删除")
	}
	if err := PauseCLI(root, true, nil); err != nil {
		t.Fatal(err)
	}
	st, _, _ = LoadPauseFile(root)
	if !st.Matches(device.DeviceInfo{Name: "whatever"}) {
		t.Fatal("无参 pause 应匹配全部")
	}
}
