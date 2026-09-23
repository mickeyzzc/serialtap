// Package daemon 实现热插拔守护循环：轮询枚举、起停采集器、暂停清单热重载。
package daemon

import (
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"regexp"
	"strconv"
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

// Daemon: daemon 的导出别名（cli 的托盘接线需要具名类型；New 返回 *daemon）。
type Daemon = daemon

func discardLog(string, ...any) {}

// nameToken: 设备稳定身份（key + by-id，Windows 实例路径内嵌 MAC）→ 4 位
// base36 散列。只用于撞名去重，无展示语义；碰撞概率 ~1/1.7M，
// 真撞了由 dedupeName 的计数兜底。
func nameToken(key, byID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key + "\x00" + byID))
	s := strconv.FormatUint(uint64(h.Sum32()), 36)
	if len(s) > 4 {
		s = s[len(s)-4:]
	}
	return s
}

// dedupeName: 基名未被占用直接用；被占用则 <base>-<token>；token 也被占用
// （同板换口重枚举成不同身份、或散列碰撞）时退回 -2/-3 计数保底。
func dedupeName(base, key, byID string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	tok := nameToken(key, byID)
	for i := 1; ; i++ {
		n := fmt.Sprintf("%s-%s", base, tok)
		if i > 1 {
			n = fmt.Sprintf("%s-%d", n, i)
		}
		if !used[n] {
			return n
		}
	}
}

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
		// 重名设备去重（同型号板 by-id 无序列号时撞名，如两只乐鑫原生
		// USB-JTAG 都是 303a:1001 → 都叫 esp32s3-jtag）。后缀不按接入顺序
		// 计数——那在换插顺序/守护重启后会换主；改从设备稳定身份派生
		// token，同一块板无论第几个接入、跨守护重启后缀都一致。裸基名
		// 先到先得：双板并存请锚定后缀名，或用配置 names 按 by-id
		// 序列号/MAC 给板子唯一命名。
		name := dedupeName(inf.Name, inf.Key, inf.ByID, d.namesUsed)
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
			d.proxyRemove(key) // 设备拔出收口其代理监听（此前漏调，监听句柄泄漏）
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
