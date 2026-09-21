// Package daemon 实现热插拔守护循环：轮询枚举、起停采集器、暂停清单热重载。
package daemon

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/logstore"
	"github.com/mickeyzzc/serialtap/internal/pause"
	"github.com/mickeyzzc/serialtap/internal/signature"
)

// daemon: run 命令的可测核心。每 tick 枚举设备、起停采集器、热重载暂停清单。
type daemon struct {
	cfg        config.Config
	excl       []*regexp.Regexp
	eng        *signature.SignatureEngine
	pause      *pause.PauseState
	pauseMTime time.Time
	collectors map[string]*collector.Collector
	namesUsed  map[string]bool
	enum       func() ([]device.DeviceInfo, error)
	logf       func(format string, args ...any)
	wg         sync.WaitGroup // 采集器 Run 协程追踪（shutdown 等待，防泄漏）
	mu         sync.Mutex     // 保护 releases
	releases   map[string]releaseSpec
	proxyMu    sync.Mutex              // 保护 proxies
	proxies    map[string]net.Listener // 设备 key → 透传监听（见 proxy.go）
	// opMu 串行化 Flash/Release/ResumeAll（TryLock fail-fast，见 flash.go）
	opMu sync.Mutex
}

func discardLog(string, ...any) {}

// New: 构造守护核心。enum/logf 为注入点（生产用默认实现，测试注入假件）。
func New(cfg config.Config, excl []*regexp.Regexp,
	enum func() ([]device.DeviceInfo, error), logf func(string, ...any)) (*daemon, error) {
	if logf == nil {
		logf = discardLog
	}
	if enum == nil {
		enum = func() ([]device.DeviceInfo, error) { return device.Enumerate(excl, cfg.Names) }
	}
	pfile, mtime, err := pause.LoadPauseFile(cfg.Root)
	if err != nil {
		return nil, err
	}
	return &daemon{
		cfg:        cfg,
		excl:       excl,
		eng:        signature.New(cfg.ExtraSigs),
		pause:      pfile,
		pauseMTime: mtime,
		collectors: map[string]*collector.Collector{},
		namesUsed:  map[string]bool{},
		proxies:    map[string]net.Listener{},
		enum:       enum,
		logf:       logf,
	}, nil
}

// tick: 单轮巡检（枚举 diff、起停采集器、暂停清单热重载）。
func (d *daemon) Tick() {
	infos, err := d.enum()
	if err != nil {
		d.logf("[watch] 枚举失败: %v", err)
	}
	seen := map[string]bool{}
	for _, inf := range infos {
		seen[inf.Key] = true
		if _, ok := d.collectors[inf.Key]; ok {
			continue
		}
		// 重名设备加后缀（同型号适配器 by-id 无序列号时可能撞名）
		name := inf.Name
		for i := 2; d.namesUsed[name]; i++ {
			name = fmt.Sprintf("%s-%d", inf.Name, i)
		}
		d.namesUsed[name] = true
		inf.Name = name
		w, err := logstore.NewDeviceWriter(d.cfg.Root, name, d.cfg.RotateMB)
		if err != nil {
			d.logf("[watch] 建目录失败 %s: %v", name, err)
			continue
		}
		c := collector.NewCollector(inf, d.cfg, w, d.eng, d.pause, d.logf)
		d.collectors[inf.Key] = c
		d.logf("[watch] 设备接入 %s → %s (key=%s by-id=%s vid:pid=%s:%s)",
			inf.Tty, name, inf.Key, inf.ByID, inf.VID, inf.PID)
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			c.Run()
		}()
	}
	for key, c := range d.collectors {
		if !seen[key] {
			d.logf("[watch] 设备移除 %s (key=%s)", c.Tty(), key)
			c.Stop()
			delete(d.collectors, key)
			delete(d.namesUsed, c.DeviceName())
		}
	}
	d.reloadPause()
	d.tickReleases()
}

// reloadPause: PAUSED 文件 mtime 变更 → 热替换暂停模式表。
func (d *daemon) reloadPause() {
	if fi, err := os.Stat(pause.PauseFilePath(d.cfg.Root)); err == nil {
		if !fi.ModTime().Equal(d.pauseMTime) {
			d.pauseMTime = fi.ModTime()
			np, _, err := pause.LoadPauseFile(d.cfg.Root)
			if err == nil {
				d.pause.ReplaceWith(np)
				d.logf("[watch] PAUSED 重载: %d 条模式", np.Len())
			}
		}
		return
	}
	if d.pause.Clear() {
		d.logf("[watch] PAUSED 移除 — 恢复采集")
	}
	d.pauseMTime = time.Time{}
}

// Shutdown: 停止全部采集器并等待退出（不留泄漏 goroutine）。
func (d *daemon) Shutdown() {
	d.proxyCloseAll()
	for _, c := range d.collectors {
		c.Stop()
	}
	d.wg.Wait()
}

// Collectors: 当前活跃采集器数量（巡检/测试用）。
func (d *daemon) Collectors() int { return len(d.collectors) }

// Name: 指定 key 的采集器设备名（测试用）。
func (d *daemon) Name(key string) string {
	if c, ok := d.collectors[key]; ok {
		return c.DeviceName()
	}
	return ""
}

// Paused: 指定设备名当前是否被暂停（巡检/测试用）。
func (d *daemon) Paused(name string) bool {
	return d.pause.Matches(device.DeviceInfo{Name: name})
}
