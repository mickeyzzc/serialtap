//go:build darwin && cgo

package tray

import "testing"

func TestSSHWarn(t *testing.T) {
	if w := sshWarn(func(string) string { return "" }); w != "" {
		t.Fatalf("本机会话不应告警: %s", w)
	}
	get := func(k string) string {
		if k == "SSH_CONNECTION" {
			return "1.2.3.4 5555 10.0.0.5 22"
		}
		return ""
	}
	if w := sshWarn(get); w == "" {
		t.Fatal("SSH 会话应产生告警")
	}
	getTTY := func(k string) string {
		if k == "SSH_TTY" {
			return "/dev/ttys999"
		}
		return ""
	}
	if w := sshWarn(getTTY); w == "" {
		t.Fatal("SSH_TTY 会话应产生告警")
	}
}

func TestHostLogfNilSafe(t *testing.T) {
	var h Host
	h.logf("不 panic: %d", 42) // Logf 为 nil 应静默
	called := false
	h.Logf = func(string, ...any) { called = true }
	h.logf("x")
	if !called {
		t.Fatal("Logf 注入后应被调用")
	}
}
