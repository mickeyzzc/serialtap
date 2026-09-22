package daemon

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/flash"
	"github.com/mickeyzzc/serialtap/internal/pause"
)

// —— release（临时让口）与代理刷固件编排 ——

type releaseSpec struct {
	until     time.Time // 限时回采（零值=不限时）
	untilIdle bool      // 端口空闲自动回采
	idleSince time.Time // 连续空闲起点
}

const idleQuietS = 3 // until_idle 的连续空闲确认秒数

// matches: 按正则匹配采集器（tty/name/key/by-id 任一），按 key 排序返回。
// map 迭代顺序每次调用都随机——排序后 flash/release/proxy 的多设备处理
// 顺序（含 ProxyStart 返回"第一个"端点、wifipulse 取"第一台"设备）才是
// 确定的、可复现的，多板同芯片时不靠运气。
func (d *daemon) matches(pattern string) ([]string, []*collector.Collector) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, nil
	}
	var keys []string
	cs := map[string]*collector.Collector{}
	for k, c := range d.collectors {
		for _, f := range []string{c.Tty(), c.DeviceName(), c.Key(), c.ByID()} {
			if f != "" && re.MatchString(f) {
				keys = append(keys, k)
				cs[k] = c
				break
			}
		}
	}
	sort.Strings(keys)
	out := make([]*collector.Collector, 0, len(keys))
	for _, k := range keys {
		out = append(out, cs[k])
	}
	return keys, out
}

// Release: 让出匹配设备的串口给外部工具。forDur>0 限时自动回采；
// untilIdle=true 时端口连续空闲 idleQuietS 秒后自动回采。
// 空闲检测依赖 /proc（Linux）或 lsof（macOS）—— Windows 两者皆无，
// 显式拒绝而非静默误判（恒"无人占用"会导致 3s 后抢回口、打断外部工具）。
// 与 Flash/ResumeAll 互斥：刷写进行中时立即报错（fail-fast，不排队）。
func (d *daemon) Release(pattern string, forDur time.Duration, untilIdle bool) (int, error) {
	keys, cs := d.matches(pattern)
	if len(cs) == 0 {
		return 0, fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	if untilIdle && forDur <= 0 && !device.IdleDetectSupported() {
		return 0, fmt.Errorf("此平台不支持空闲自动回采（Windows 无 /proc/lsof 占用检测）；" +
			"请用 --for <时长> 限时回采，或让口后 serialtap resume 手动回采")
	}
	if !d.opMu.TryLock() {
		return 0, fmt.Errorf("另一个 flash/release 操作进行中，请稍后再试")
	}
	defer d.opMu.Unlock()
	spec := releaseSpec{untilIdle: untilIdle}
	if forDur > 0 {
		spec.until = time.Now().Add(forDur)
	}
	d.mu.Lock()
	if d.releases == nil {
		d.releases = map[string]releaseSpec{}
	}
	d.mu.Unlock()
	n := 0
	for i, c := range cs {
		if !c.Suspend(5 * time.Second) {
			continue // 端口没让出来（采集器卡死？）—— 跳过并留给看门狗
		}
		c.LogEvent("released for external tool (pattern=%q for=%s until_idle=%v)",
			pattern, forDur, untilIdle)
		d.mu.Lock()
		d.releases[keys[i]] = spec
		d.mu.Unlock()
		n++
	}
	return n, nil
}

// ResumeAll: 恢复匹配设备（清 PAUSED 文件条目 + 撤销 release + 直接 Resume）。
// pattern 为空 = 恢复全部（清空 PAUSED，与无参 pause 全停对称）。
// 刷写进行中拒绝 —— 否则 resume 会让采集器在 esptool 工作中途重新抢口。
func (d *daemon) ResumeAll(pattern string) (int, error) {
	if !d.opMu.TryLock() {
		return 0, fmt.Errorf("刷写进行中，resume 被拒绝（防止中途抢口），请稍后再试")
	}
	defer d.opMu.Unlock()
	n := 0
	pats := []string{}
	if pattern != "" {
		pats = []string{pattern}
	}
	if err := pause.PauseCLI(d.cfg.Root, false, pats); err == nil {
		n++
	}
	d.mu.Lock()
	var resumed []string
	for k := range d.releases {
		resumed = append(resumed, k)
	}
	d.mu.Unlock()
	for _, k := range resumed {
		if c, ok := d.collectors[k]; ok {
			c.Resume()
			c.LogEvent("release revoked by resume command")
			n++
		}
		d.mu.Lock()
		delete(d.releases, k)
		d.mu.Unlock()
	}
	// PAUSED 撤销后文件暂停设备由既有热重载路径自然恢复
	return n, nil
}

// tickReleases: 每轮巡检处理 release 的到期/空闲回采。
func (d *daemon) tickReleases() {
	now := time.Now()
	d.mu.Lock()
	var done []string
	for k, spec := range d.releases {
		c, ok := d.collectors[k]
		if !ok {
			done = append(done, k) // 设备已拔走
			continue
		}
		if !spec.until.IsZero() && now.After(spec.until) {
			done = append(done, k)
			continue
		}
		if spec.untilIdle {
			if holders := device.PortHolders(c.Tty()); len(holders) > 0 {
				spec.idleSince = time.Time{}
				d.releases[k] = spec
				continue
			}
			if spec.idleSince.IsZero() {
				spec.idleSince = now
				d.releases[k] = spec
				continue
			}
			if now.Sub(spec.idleSince) >= idleQuietS*time.Second {
				done = append(done, k)
			}
		}
	}
	d.mu.Unlock()
	for _, k := range done {
		d.mu.Lock()
		delete(d.releases, k)
		d.mu.Unlock()
		if c, ok := d.collectors[k]; ok {
			c.Resume()
			c.LogEvent("port idle/time expired — resuming collection")
		}
	}
}

// Status: 全部设备当前状态（按 key 排序——客户端按确定顺序拿到清单，
// "取第一台"类的消费方才不会每次调用换目标）。
func (d *daemon) Status() []ctl.DevState {
	keys := make([]string, 0, len(d.collectors))
	for k := range d.collectors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ctl.DevState, 0, len(keys))
	for _, k := range keys {
		c := d.collectors[k]
		state := c.State()
		if state == "collecting" && d.pause.Matches(devInfoOf(c)) {
			state = "paused"
		}
		out = append(out, ctl.DevState{
			Name: c.DeviceName(), Tty: c.Tty(), Key: c.Key(), State: state,
			Proxy: c.ProxyAddr(), ProxyEndpoint: d.proxyEndpointOf(k),
		})
	}
	return out
}

func devInfoOf(c *collector.Collector) device.DeviceInfo {
	return device.DeviceInfo{Name: c.DeviceName(), Tty: c.Tty(), Key: c.Key(), ByID: c.ByID()}
}

// flashMilestone: esptool 输出行 → 事件流里程碑（节流）。全量输出仍流式
// 给 ctl 客户端。两类判定：
//   - 前缀组（每镜像一条，全部记录）：Chip is / Wrote / Hash verified /
//     Hard resetting 等 —— 用前缀而非子串，防止 "Wrote ... (N compressed)"
//     被更早的 Compressed 关键词吞掉；
//   - 重试组（只记一次）：Connecting / error / Failed。
func flashMilestone(line string, seen map[string]bool) (string, bool) {
	clean := strings.TrimSpace(strings.ReplaceAll(line, "\r", " "))
	if clean == "" {
		return "", false
	}
	for _, p := range []string{
		"Chip is", "Running esptool", "Wrote ", "Hash of data verified",
		"Compressed ", "Leaving...", "Hard resetting", "A fatal error",
	} {
		if strings.HasPrefix(clean, p) {
			return clean, true
		}
	}
	lower := strings.ToLower(clean)
	for _, kw := range []string{"Connecting", "error", "Failed"} {
		if strings.Contains(lower, strings.ToLower(kw)) {
			if seen[kw] {
				return "", false
			}
			seen[kw] = true
			return clean, true
		}
	}
	// "Writing at 0x... (N %)" 只记整十进度
	if i := strings.Index(clean, " ("); i >= 0 {
		if j := strings.Index(clean, "%)"); j > i {
			var pct int
			if _, err := fmt.Sscanf(clean[i+2:j], "%d", &pct); err == nil && pct%10 == 0 {
				key := fmt.Sprintf("pct%d", pct)
				if seen[key] {
					return "", false
				}
				seen[key] = true
				return clean, true
			}
		}
	}
	return "", false
}

// Flash: 代理刷固件 —— 让口 → 调 esptool（输出流式回调）→ 回采。
// 匹配多个设备时逐个刷，但**默认拒绝**（all=false）——多板同芯片时
// 未锚定的正则会把在测的板也拖进刷写序列（让口复位 + 错芯片镜像），
// 实测事故来源；确要批量刷传 all=true（CLI --all）。与 Release/ResumeAll
// 互斥（TryLock fail-fast）；刷写前撤销匹配设备的 pending release，
// 防止限时到期在 esptool 工作中途抢回口。
func (d *daemon) Flash(pattern string, all bool, spec flash.Spec, out func(line string)) error {
	keys, cs := d.matches(pattern)
	if len(cs) == 0 {
		return fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	if len(cs) > 1 && !all {
		names := make([]string, len(cs))
		for i, c := range cs {
			names[i] = c.DeviceName()
		}
		return fmt.Errorf("模式 %q 匹配 %d 台设备（%s）—— 批量刷写需显式确认（CLI --all / ctl 请求 all:true）；精确刷一台请锚定（如 ^%s$）",
			pattern, len(cs), strings.Join(names, ", "), cs[0].DeviceName())
	}
	if !d.opMu.TryLock() {
		return fmt.Errorf("另一个 flash/release 操作进行中，请稍后再试")
	}
	defer d.opMu.Unlock()

	// 预演：解析并回显将执行的 esptool 命令，不动端口、不切状态
	if spec.DryRun {
		for _, c := range cs {
			argv, err := flash.Plan(c.Tty(), spec)
			if err != nil {
				return fmt.Errorf("%s: %w", c.DeviceName(), err)
			}
			c.LogEvent("flash dry-run: %s", strings.Join(argv, " "))
			out(fmt.Sprintf("[%s] %s", c.DeviceName(), strings.Join(argv, " ")))
		}
		out(fmt.Sprintf("（dry-run：%d 台设备，未动端口）", len(cs)))
		return nil
	}

	// 撤销匹配设备的 pending release（限时到期会中途抢口）
	d.mu.Lock()
	for _, k := range keys {
		delete(d.releases, k)
	}
	d.mu.Unlock()

	timeout := time.Duration(d.cfg.FlashTimeoutS) * time.Second
	for _, c := range cs {
		c.LogEvent("proxy flash start (bins=%d args_file=%q timeout=%s)",
			len(spec.Bins), spec.ArgsFile, timeout)
		if argv, err := flash.Plan(c.Tty(), spec); err == nil {
			c.LogEvent("flash plan: %s", strings.Join(argv, " "))
		}
		if !c.Suspend(10 * time.Second) {
			c.LogEvent("proxy flash abort: port did not release")
			return fmt.Errorf("%s: 端口让出超时", c.DeviceName())
		}
		c.SetFlashing(true)
		seen := map[string]bool{}
		err := flash.Run(spec.Esptool, c.Tty(), spec, timeout, func(line string) {
			out(line)
			if kw, ok := flashMilestone(line, seen); ok {
				c.LogEvent("flash: %s", kw)
			}
		})
		c.SetFlashing(false)
		c.LogEvent("proxy flash finished (err=%v)", err)
		c.Resume()
		if err != nil {
			return fmt.Errorf("%s: %w", c.DeviceName(), err)
		}
	}
	return nil
}
