package daemon

// board 请求的参数校验门（issue #63）。独立成文件：只测 Board 对 Plan
// 错误的中止行为，不触碰其他守护编排用例。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/board"
	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

// —— issue #63：Plan 校验失败的 board 请求必须整体报错中止 ——
// 此前校验错只落事件流、执行照旧，坏 addr 会被当 0 读。
func TestBoardAbortsOnInvalidPlan(t *testing.T) {
	root := t.TempDir()
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root, device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	testutil.WaitFor(t, 3*time.Second, func() bool { return d.Collectors() == 1 }, "采集器未起")

	out := filepath.Join(root, "x.bin")
	err = d.Board("fakeA", false, board.Spec{
		Action: board.ActionDump, Addr: "zz", Size: "0x10", OutPath: out,
	}, func(string) {})
	if err == nil {
		t.Fatal("坏 addr 的 board 请求必须报错")
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatal("坏请求不应产出 dump 文件")
	}
	// 校验发生在让口之前：采集器不应进入 suspended/flashing
	if c := d.collectors["kA"]; c == nil || c.State() != "collecting" {
		t.Fatalf("校验门不应扰动端口: %+v", d.Status())
	}
}
