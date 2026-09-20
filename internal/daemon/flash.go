package daemon

import (
	"fmt"
	"regexp"
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

// matches: 按正则匹配采集器（tty/name/key/by-id 任一）。
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
	out := make([]*collector.Collector, 0, len(keys))
	for _, k := range keys {
		out = append(out, cs[k])
	}
	return keys, out
}

// Release: 让出匹配设备的串口给外部工具。forDur>0 限时自动回采；
// untilIdle=true 时端口连续空闲 idleQuietS 秒后自动回采。
func (d *daemon) Release(pattern string, forDur time.Duration, untilIdle bool) (int, error) {
	keys, cs := d.matches(pattern)
	if len(cs) == 0 {
		return 0, fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
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
func (d *daemon) ResumeAll(pattern string) (int, error) {
	n := 0
	if pattern != "" {
		if err := pause.PauseCLI(d.cfg.Root, false, []string{pattern}); err == nil {
			n++
		}
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

// Status: 全部设备当前状态。
func (d *daemon) Status() []ctl.DevState {
	out := make([]ctl.DevState, 0, len(d.collectors))
	for _, c := range d.collectors {
		state := c.State()
		if state == "collecting" && d.pause.Matches(devInfoOf(c)) {
			state = "paused"
		}
		out = append(out, ctl.DevState{Name: c.DeviceName(), Tty: c.Tty(), Key: c.Key(), State: state})
	}
	return out
}

func devInfoOf(c *collector.Collector) device.DeviceInfo {
	return device.DeviceInfo{Name: c.DeviceName(), Tty: c.Tty(), Key: c.Key(), ByID: c.ByID()}
}

// Flash: 代理刷固件 —— 让口 → 调 esptool（输出流式回调）→ 回采。
// 匹配多个设备时逐个刷。
func (d *daemon) Flash(pattern string, spec flash.Spec, out func(line string)) error {
	_, cs := d.matches(pattern)
	if len(cs) == 0 {
		return fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	for _, c := range cs {
		c.LogEvent("proxy flash start (bins=%d args_file=%q)", len(spec.Bins), spec.ArgsFile)
		if !c.Suspend(10 * time.Second) {
			c.LogEvent("proxy flash abort: port did not release")
			return fmt.Errorf("%s: 端口让出超时", c.DeviceName())
		}
		c.SetFlashing(true)
		err := flash.Run(spec.Esptool, c.Tty(), spec, func(line string) {
			out(line)
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
