//go:build windows

package cli

import "testing"

// 单实例守卫（#83）：首个调用持有互斥体，第二个实例必须报"已运行"。
// 本测试进程因此持有互斥体至退出——同进程内后续不应再有进入托盘的用例。
func TestTraySingleInstanceGuard(t *testing.T) {
	if trayAlreadyRunning() {
		t.Fatal("首个实例不应报已运行")
	}
	if !trayAlreadyRunning() {
		t.Fatal("第二实例必须报已运行")
	}
}
