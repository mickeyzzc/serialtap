//go:build linux

package cli

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

// run 优雅退出：SIGTERM → 停采集器返回（--exclude .* 避免碰真实设备）。
func TestRunGracefulShutdownOnSIGTERM(t *testing.T) {
	root := t.TempDir()
	go func() {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
	t.Cleanup(func() { signal.Reset(syscall.SIGTERM, syscall.SIGINT) })
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"run", "--root", root, "--exclude", ".*", "--poll-ms", "50"})
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run 应优雅退出: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run 未随 SIGTERM 退出")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root 目录应已创建: %v", err)
	}
}

func TestAttachLifecycle(t *testing.T) {
	root := t.TempDir()
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{Chunks: [][]byte{[]byte("attached line\n")}}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	go func() {
		time.Sleep(500 * time.Millisecond)
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()
	t.Cleanup(func() { signal.Reset(syscall.SIGTERM, syscall.SIGINT) })
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"attach", "/dev/fakeTTY", "--name", "attachtest", "--root", root})
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("attach 应优雅退出: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach 未随 SIGTERM 退出")
	}
	day := time.Now().Format("20060102")
	ser := testutil.ReadFile(t, filepath.Join(root, "attachtest", "serial-"+day+".log"))
	if !strings.Contains(ser, "attached line") {
		t.Fatalf("attach 未落盘: %q", ser)
	}
}
