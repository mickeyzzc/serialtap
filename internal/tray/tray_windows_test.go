//go:build windows

package tray

import (
	"bufio"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// daemonAlive 三态探活（#83 自动拉起守护的前置）：
// 无监听 / 正常应答 / 楔死（连上不应答）。
func TestDaemonAliveProbe(t *testing.T) {
	// 无监听 → 死
	if daemonAlive(filepath.Join(t.TempDir(), "nope.sock")) {
		t.Fatal("无监听应判死")
	}
	// 正常应答 → 活
	sock := filepath.Join(t.TempDir(), "alive.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			sc := bufio.NewScanner(c)
			if sc.Scan() {
				_, _ = c.Write([]byte("{\"ok\":true}\n"))
			}
			_ = c.Close()
		}
	}()
	if !daemonAlive(sock) {
		t.Fatal("正常应答的守护应判活")
	}
	// 楔死：accept 后不应答 → 死（守护 socket 占用守卫防双拉起）
	wedged := filepath.Join(t.TempDir(), "wedged.sock")
	ln2, err := net.Listen("unix", wedged)
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			time.Sleep(3 * time.Second) // 握住不答
			_ = c.Close()
		}
	}()
	if daemonAlive(wedged) {
		t.Fatal("楔死守护（连上不应答）应判死")
	}
}
