package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// —— CLI 分发层测试（runMain）——

func TestRunMainDispatch(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"version"}, 0},
		{nil, 2},
		{[]string{"no-such-cmd"}, 2},
		{[]string{"analyze"}, 1},          // 缺路径
		{[]string{"decode-backtrace"}, 1}, // 缺路径
		{[]string{"attach"}, 1},           // 缺 TTY
		{[]string{"pause", "--root", t.TempDir()}, 0},
		{[]string{"resume", "--root", t.TempDir()}, 0},
		{[]string{"list"}, 0}, // 真实 sysfs，有无设备都不算错
	}
	for _, c := range cases {
		if got := runMain(c.args); got != c.want {
			t.Fatalf("runMain(%v) = %d, want %d", c.args, got, c.want)
		}
	}
}

// run 优雅退出：SIGTERM → 停采集器返回（--exclude .* 避免碰真实设备）。
func TestCmdRunGracefulShutdownOnSIGTERM(t *testing.T) {
	root := t.TempDir()
	go func() {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
	t.Cleanup(func() { signal.Reset(syscall.SIGTERM, syscall.SIGINT) })
	done := make(chan error, 1)
	go func() {
		done <- cmdRun([]string{"--root", root, "--exclude", ".*", "--poll-ms", "50"})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cmdRun 应优雅退出: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdRun 未随 SIGTERM 退出")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root 目录应已创建: %v", err)
	}
}

// attach：假端口 + SIGTERM 优雅退出。
func TestCmdAttachLifecycle(t *testing.T) {
	root := t.TempDir()
	fp := &fakePort{chunks: [][]byte{[]byte("attached line\n")}}
	withFakePort(t, fp)
	go func() {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
	t.Cleanup(func() { signal.Reset(syscall.SIGTERM, syscall.SIGINT) })
	done := make(chan error, 1)
	go func() {
		done <- cmdAttach([]string{"/dev/fakeTTY", "--name", "attachtest", "--root", root})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cmdAttach 应优雅退出: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdAttach 未随 SIGTERM 退出")
	}
	day := time.Now().Format("20060102")
	ser := readLogFile(t, filepath.Join(root, "attachtest", "serial-"+day+".log"))
	if !strings.Contains(ser, "attached line") {
		t.Fatalf("attach 未落盘: %q", ser)
	}
	// 缺 TTY 参数报错
	if err := cmdAttach([]string{}); err == nil {
		t.Fatal("缺 TTY 应报错")
	}
}

func TestCmdPauseCLIDefaultRootFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "mylogs")
	os.MkdirAll(root, 0o755)
	// 不传 --root 时用默认配置 root（"logs"，相对当前目录）—— 这里只验证不崩
	if err := cmdPauseCLI([]string{"pause-arg"}, true); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll("logs") // 清理默认相对路径副作用
	os.RemoveAll("PAUSED")
}

// —— decode-backtrace ——
func TestCmdDecodeBacktraceWithFakeAddr2line(t *testing.T) {
	dir := t.TempDir()
	elf := filepath.Join(dir, "fake.elf")
	os.WriteFile(elf, []byte("ELF"), 0o644)
	a2l := filepath.Join(dir, "fake-addr2line")
	os.WriteFile(a2l, []byte("#!/bin/sh\necho \"$0 $@\"\n"), 0o755)

	log := filepath.Join(dir, "serial.log")
	os.WriteFile(log, []byte("[ts] Backtrace: 0x400D0C5A:0x3FFB7D40 0x4008C71E:0x3FFB7D60\n"), 0o644)

	if err := cmdDecodeBacktrace(log, elf, a2l, Config{}); err != nil {
		t.Fatal(err)
	}

	// 错误路径：无 elf 映射
	if err := cmdDecodeBacktrace(log, "", a2l, Config{}); err == nil {
		t.Fatal("无 elf 应报错")
	}
	// 错误路径：elf 不存在
	if err := cmdDecodeBacktrace(log, filepath.Join(dir, "nope.elf"), a2l, Config{}); err == nil {
		t.Fatal("缺 elf 文件应报错")
	}
	// 错误路径：addr2line 不可用
	if err := cmdDecodeBacktrace(log, elf, filepath.Join(dir, "no-a2l"), Config{}); err == nil {
		t.Fatal("缺 addr2line 应报错")
	}
	// 无 Backtrace 行的日志
	plain := filepath.Join(dir, "plain.log")
	os.WriteFile(plain, []byte("nothing here\n"), 0o644)
	if err := cmdDecodeBacktrace(plain, elf, a2l, Config{}); err != nil {
		t.Fatal(err)
	}
}

// —— 零散小面 ——
func TestMiscSmallSurfaces(t *testing.T) {
	usage() // 覆盖 usage 输出（写到 stderr）
	stdoutLog("hi %d", 1)
	discardLog("hi")

	// defaultOpenPort 对不存在设备返回错误而非 panic
	if _, err := defaultOpenPort("/dev/definitely-not-here", 115200); err == nil {
		t.Fatal("不存在设备应报错")
	}

	// SignatureEngine.Names
	if names := NewSignatureEngine(nil).Names(); len(names) != len(defaultSigs) {
		t.Fatalf("Names 数量不符: %d", len(names))
	}

	// writer Dir/Name
	w, _ := NewDeviceWriter(t.TempDir(), "dirdev", 64)
	defer w.Close()
	if w.Name() != "dirdev" || !strings.HasSuffix(w.Dir(), "dirdev") {
		t.Fatalf("Dir/Name 错误: %q %q", w.Name(), w.Dir())
	}
	// WriteLine 出错（目录被删）不 panic
	os.RemoveAll(w.Dir())
	_ = w.WriteLine("orphan line")
	_ = w.WriteEvent("orphan event")
}

func TestVersionConstant(t *testing.T) {
	if !strings.HasPrefix(Version, "0.") {
		t.Fatalf("Version 格式异常: %s", Version)
	}
	fmt.Println() // 保持 fmt 引用（runMain 已用，此行防御性）
}
