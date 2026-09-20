package collector

import (
	"sync"
	"sync/atomic"
	"time"
)

// SuspendResume: 程序化让出/收回串口的直接控制通道（release 与代理刷固件用），
// 与 PAUSED 文件的模式级暂停并存。Suspend 等到端口真正关闭后才返回 ——
// 调用方（esptool 等）拿到返回即可独占端口。
type SuspendResume struct {
	mu       sync.Mutex
	held     bool
	portOpen atomic.Bool
	state    atomic.Int32 // 0=collecting 1=suspended 2=flashing
}

const (
	stateCollecting int32 = iota
	stateSuspended
	stateFlashing
)

// Suspend: 请求让出端口并等待确认（端口已关闭）或超时。
func (c *Collector) Suspend(timeout time.Duration) bool {
	c.sr.mu.Lock()
	c.sr.held = true
	c.sr.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !c.sr.portOpen.Load() {
			c.sr.state.Store(stateSuspended)
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Resume: 收回端口（下一轮 open 即恢复采集）。
func (c *Collector) Resume() {
	c.sr.mu.Lock()
	c.sr.held = false
	c.sr.mu.Unlock()
	c.sr.state.Store(stateCollecting)
}

// Held: 当前是否被程序化让出。
func (c *Collector) Held() bool {
	c.sr.mu.Lock()
	defer c.sr.mu.Unlock()
	return c.sr.held
}

// SetFlashing: 代理刷固件进行中标记（状态展示用）。
func (c *Collector) SetFlashing(on bool) {
	if on {
		c.sr.state.Store(stateFlashing)
	} else {
		c.sr.state.Store(stateSuspended)
	}
}

// State: collecting | suspended | flashing。
func (c *Collector) State() string {
	switch c.sr.state.Load() {
	case stateSuspended:
		return "suspended"
	case stateFlashing:
		return "flashing"
	default:
		return "collecting"
	}
}

// waitHeldCleared: 让出期间等待恢复（文件暂停与程序化让出任一存在即等待）。
func (c *Collector) waitHeldCleared() bool {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.stop:
			return false
		case <-tick.C:
			if !c.Held() && !c.pause.Matches(c.dev) {
				return true
			}
		}
	}
}
