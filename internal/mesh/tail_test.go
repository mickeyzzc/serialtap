package mesh

import (
	"os"
	"path/filepath"
	"testing"
)

// 回归：latestTailFile 必须按 (日期, 数字后缀) 语义取最新——同日内 .001/.002
// 比基础文件新（写满后写入序号文件）。原实现是全路径字典序：'l' > '0' 把
// 基础文件排最大，轮转发生后永远跟在读满的旧文件上——初始回放一次末尾后
// 远端尾随永久静默（真机：相机板当天日志轮转后远端数据"断了"的根因）。
func TestLatestTailFileRotation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cam")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 只有基础文件 → 基础文件
	mk("serial-20261005.log")
	if got, ok := latestTailFile(root, "cam", "serial"); !ok || got != filepath.Join(dir, "serial-20261005.log") {
		t.Fatalf("基础文件场景: got %s ok=%v", got, ok)
	}
	// 轮转出 .001 → .001 最新（字典序会错选基础文件）
	mk("serial-20261005.001.log")
	if got, _ := latestTailFile(root, "cam", "serial"); got != filepath.Join(dir, "serial-20261005.001.log") {
		t.Fatalf("轮转 .001 场景: 期望 .001，得 %s", got)
	}
	// 再轮转 .002 → .002
	mk("serial-20261005.002.log")
	if got, _ := latestTailFile(root, "cam", "serial"); got != filepath.Join(dir, "serial-20261005.002.log") {
		t.Fatalf("轮转 .002 场景: 期望 .002，得 %s", got)
	}
	// 跨天：新日期的基础文件 > 旧日期的任何序号文件
	mk("serial-20261006.log")
	if got, _ := latestTailFile(root, "cam", "serial"); got != filepath.Join(dir, "serial-20261006.log") {
		t.Fatalf("跨天场景: 期望新日期基础文件，得 %s", got)
	}
	// events kind 混入不受影响
	mk("events-20261005.log")
	if got, _ := latestTailFile(root, "cam", "serial"); got != filepath.Join(dir, "serial-20261006.log") {
		t.Fatalf("events 混入场景: got %s", got)
	}
	if got, _ := latestTailFile(root, "cam", "events"); got != filepath.Join(dir, "events-20261005.log") {
		t.Fatalf("events kind: got %s", got)
	}
}
