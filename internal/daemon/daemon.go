// Package daemon 实现热插拔守护循环：轮询枚举、起停采集器、暂停清单热重载。
package daemon

import (
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/facts"
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
	prevInst   map[string]string // 名 → "key\x00tty"：同名牌换实例 = 物理板更换（换板事件）
	misses     map[string]int    // key → 连续未见拍数（移除去抖，见 removeMissTicks）
	enum       func() ([]device.DeviceInfo, error)
	logf       func(format string, args ...any)
	wg         sync.WaitGroup // 采集器 Run 协程追踪（shutdown 等待，防泄漏）
	mu         sync.Mutex     // 保护 releases
	releases   map[string]releaseSpec
	proxyMu    sync.Mutex              // 保护 proxies
	proxies    map[string]net.Listener // 设备 key → 透传监听（见 proxy.go）
	// opMu 串行化 Flash/Release/ResumeAll（TryLock fail-fast，见 flash.go）
	opMu sync.Mutex
	// colMu 保护 collectors/namesUsed/prevInst/misses。Tick（watch 协程，托盘
	// 也会并发触发）写；ctl 每连接一协程、web 面板每请求一协程经
	// Status/Facts/matches 并发读。此前无锁——并发读写普通 map 是
	// runtime fatal error（直接杀进程，丢全部在采日志），面板轮询叠加热插拔
	// 即现实触发路径（#52）。
	colMu sync.RWMutex
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
		prevInst:   map[string]string{},
		misses:     map[string]int{},
		proxies:    map[string]net.Listener{},
		enum:       enum,
		logf:       logf,
	}, nil
}

// Facts: 匹配设备的全量身份事实合并（ctl facts 命令 / 面板详情表）。
func (d *daemon) Facts(pattern string) (map[string]facts.Fact, error) {
	_, cs := d.matches(pattern)
	if len(cs) == 0 {
		return nil, fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	all := map[string]facts.Fact{}
	for _, c := range cs {
		for k, v := range c.FactsSnapshot() {
			all[k] = v
		}
	}
	return all, nil
}

// removeMissTicks: 设备连续未见 N 拍才判移除。Windows PnP 变更窗口期
// （恰是刷另一块板时）注册表枚举可能瞬时失败或部分 USB 条目缺 meta——
// 单拍即拆会把还在的板当拔出处理：关口 → 复位脉冲 → 下一拍重开，
// 违背"每物理设备恰好 open 一次"纪律（#59）。PollMs 默认 2s，3 拍 ≈ 6s。
const removeMissTicks = 3

// tick: 单轮巡检（枚举 diff、起停采集器、暂停清单热重载）。
// 枚举失败跳过本轮 diff（只保留 pause 热重载与 release 到期处理）——
// 把"枚举报错"当"设备全拔了"处理会拆光采集器（#59）。
func (d *daemon) Tick() {
	infos, err := d.enum()
	if err != nil {
		d.logf("[watch] 枚举失败（本轮跳过 diff）: %v", err)
		d.reloadPause()
		d.tickReleases()
		return
	}
	seen := map[string]bool{}
	for _, inf := range infos {
		seen[inf.Key] = true
	}
	d.colMu.Lock()
	for _, inf := range infos {
		delete(d.misses, inf.Key)
		if _, ok := d.collectors[inf.Key]; ok {
			continue
		}
		// 快换收口：同名前任已不在本轮枚举（正处移除去抖窗口）、继任者已
		// 到位——立即拆前任，让继任者继承裸基名并触发换板事件。不拆的话
		// 去抖期内双活，继任者被迫挂 token 后缀、换板检测失灵（seen 先
		// 全量建好再进本循环，双同名板并存时不会误拆）
		for k2, c2 := range d.collectors {
			if c2.DeviceName() == inf.Name && !seen[k2] {
				d.logf("[watch] 快换收口 %s (key=%s)", c2.Tty(), k2)
				d.removeCollectorLocked(k2, c2)
			}
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
			// 占坑回滚：不回滚的话 root 目录持续不可写（盘满/权限）时，
			// 每拍 dedupeName 都会派生 base-tok-2/-3/… 新名占坑，namesUsed
			// 无界膨胀且每拍刷错误日志（#72）
			delete(d.namesUsed, name)
			d.logf("[watch] 建目录失败 %s: %v", name, err)
			continue
		}
		c := collector.NewCollector(inf, d.cfg, w, d.eng, d.pause, d.logf)
		d.collectors[inf.Key] = c
		// 换板检测：同名牌下的实例 key 变了 = 物理板被换过（同型号板共享内置
		// 名，如两只乐鑫原生 USB-JTAG 都叫 esp32s3-jtag——不写这条事件的
		// 话，换板在面板上完全不可见：名字/日志目录/曲线全部无缝继承）
		if prev, ok := d.prevInst[name]; ok {
			if pk, pt, _ := strings.Cut(prev, "\x00"); pk != inf.Key {
				c.LogEvent("[board-swap] 物理板已更换：%s → %s（同名牌继承，日志沿用本目录）", pt, inf.Tty)
				d.logf("[watch] 换板 %s: 实例 %s → %s", name, pk, inf.Key)
			}
		}
		d.prevInst[name] = inf.Key + "\x00" + inf.Tty
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
			d.misses[key]++
			if d.misses[key] < removeMissTicks {
				continue // 连续未见去抖：单拍缺失不拆（枚举抖动/注册表瞬时读败）
			}
			d.logf("[watch] 设备移除 %s (key=%s)", c.Tty(), key)
			d.removeCollectorLocked(key, c)
		}
	}
	d.colMu.Unlock()
	d.reloadPause()
	d.tickReleases()
}

// removeCollectorLocked: 拆除单个采集器——停采集、收口代理监听与会话
// （只关 listener 不摘会话的话 pump 协程挂在 conn.Read 上直到客户端自己
// 断开，每次"带会话拔板"泄漏一个 goroutine + 一个 fd，#58）、释放名字。
// 需持 colMu 写锁。
func (d *daemon) removeCollectorLocked(key string, c *collector.Collector) {
	c.Stop()
	d.proxyRemove(key)
	c.DetachProxy()
	delete(d.collectors, key)
	delete(d.namesUsed, c.DeviceName())
	delete(d.misses, key)
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

// Shutdown: 停止全部采集器并等待退出。等待有上限（30s）——2026-09-27 出现过
// 一次关闭序列僵死（采集器已停、进程 0 CPU 楔住 30+ 分钟），兜底：超时放弃
// 等待、继续退出路径，宁可带病退出也不永久楔死。
func (d *daemon) Shutdown() {
	d.proxyCloseAll()
	d.colMu.RLock()
	for _, c := range d.collectors {
		c.Stop()
	}
	d.colMu.RUnlock()
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		d.logf("[watch] Shutdown 兜底：采集器 30s 未全部退出，放弃等待继续退出")
	}
}

// Collectors: 当前活跃采集器数量（巡检/测试用）。
func (d *daemon) Collectors() int {
	d.colMu.RLock()
	defer d.colMu.RUnlock()
	return len(d.collectors)
}

// Name: 指定 key 的采集器设备名（测试用）。
func (d *daemon) Name(key string) string {
	d.colMu.RLock()
	defer d.colMu.RUnlock()
	if c, ok := d.collectors[key]; ok {
		return c.DeviceName()
	}
	return ""
}

// Paused: 指定设备名当前是否被暂停（巡检/测试用）。
func (d *daemon) Paused(name string) bool {
	return d.pause.Matches(device.DeviceInfo{Name: name})
}
