package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/mickeyzzc/serialtap/internal/board"
)

// Board: 对匹配设备执行读侧 board 操作（info/partitions/nvs/dump）。
// 编排纪律与 Flash 完全一致：opMu 互斥、gateMulti 防多台误伤、逐台
// 让口 → 执行 → 回采。读操作同样让板子复位（esptool download 模式进出）。
// **事件审计只记动作与地址，绝不记 NVS 值**（凭据可能明文）。
func (d *daemon) Board(pattern string, all bool, spec board.Spec, out func(line string)) error {
	keys, cs := d.matches(pattern)
	if len(cs) == 0 {
		return fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	if err := gateMulti(pattern, all, cs, "board 操作"); err != nil {
		return err
	}
	if !d.opMu.TryLock() {
		return fmt.Errorf("另一个 flash/release/board 操作进行中，请稍后再试")
	}
	defer d.opMu.Unlock()

	d.mu.Lock()
	for _, k := range keys {
		delete(d.releases, k)
	}
	d.mu.Unlock()

	timeout := time.Duration(d.cfg.FlashTimeoutS) * time.Second
	// web 直调路径不经 cli 的 config 合并：esptool 未指定时回落配置（PATH
	// 里可能撞上坏掉的启动器 shim——真机：pip 残壳静默 exit 1）
	if spec.Esptool == "" {
		spec.Esptool = d.cfg.Esptool
	}
	for _, c := range cs {
		c.LogEvent("board %s start (addr=%s size=%s show_secrets=%v)",
			spec.Action, spec.Addr, spec.Size, spec.ShowSecrets)
		if argv, err := board.Plan(c.Tty(), spec); err == nil {
			c.LogEvent("board plan: %s", strings.Join(argv, " "))
		}
		if !c.Suspend(10 * time.Second) {
			c.LogEvent("board %s abort: port did not release", spec.Action)
			return fmt.Errorf("%s: 端口让出超时", c.DeviceName())
		}
		c.SetFlashing(true)
		outFacts := func(line string) { c.FactsFeedProbe(line); out(line) } // 探测输出回填身份事实
		err := board.Exec(c.Tty(), spec, timeout, outFacts,
			board.EsptoolReader(c.Tty(), spec, timeout, outFacts))
		c.SetFlashing(false)
		c.LogEvent("board %s finished (err=%v)", spec.Action, err)
		c.Resume()
		if err != nil {
			return fmt.Errorf("%s: %w", c.DeviceName(), err)
		}
	}
	return nil
}
