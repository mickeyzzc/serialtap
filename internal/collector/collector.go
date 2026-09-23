// Package collector 实现单设备采集器：open-once-and-hold 纪律、
// DTR/RTS 释放、读错误弃 fd、可选静默看门狗、暂停响应，
// 以及透明代理桥（业务程序经 serialtap 读写板子串口，见 proxy.go）。
package collector

import (
	"bytes"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	serial "go.bug.st/serial"

	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/logstore"
	"github.com/mickeyzzc/serialtap/internal/pause"
	"github.com/mickeyzzc/serialtap/internal/signature"
)

// Port: 采集器所需的最小串口面。生产实现包装 go.bug.st/serial，
// 测试注入假端口（假端口驱动采集器全链路离线测试）。
// Read 与 Write 并发安全（全双工桥接的前提：串口句柄两个方向独立）。
type Port interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
	SetDTR(v bool) error
	SetRTS(v bool) error
	SetReadTimeout(d time.Duration) error
}

// OpenPortFunc: 打开一个串口。OpenPort 是包级 seam，测试整体替换。
type OpenPortFunc func(tty string, baud int) (Port, error)

// OpenPort: 包级串口打开器（测试 seam）。
var OpenPort OpenPortFunc = defaultOpenPort

func defaultOpenPort(tty string, baud int) (Port, error) {
	return serial.Open(tty, &serial.Mode{BaudRate: baud}) // 其余零值 = 库默认 8N1
}

// Collector: 单设备采集 goroutine。设计要点（家族陷阱的代码化）：
//
//  1. open 那一拍的复位脉冲不可避免（CH340/CH343 的适配器驱动在 open() 时
//     断言复位线，pyserial 的 rts=False/dtr=False 拦不住；ESP32-S3 原生
//     USB-JTAG 的 USB_SERIAL_JTAG 外设在硅内实现了同款自动复位语义，
//     open 后 3ms 实测 rst:0x15 USB_UART_CHIP_RESET）。
//     因此纪律是"每物理设备恰好 open 一次并持有"，绝不周期性重开。
//     close 也会落一拍（HUPCL/CDC 挂断），pause/停止即复位 —— 对刷写
//     场景无害（esptool 本来就要复位）。
//  2. open 后立即释放 DTR/RTS：CH340 的 RTS 接 EN，不释放则板子被按在
//     复位态 0 字节输入；其他板无副作用。
//  3. 读错误立即放弃 fd（USB 重新枚举抛错时恋战死 fd 会错过设备重启
//     原因 —— 这是最贵的一类诊断数据丢失）。
//  4. 静默看门狗（默认关）：设备 fd 挂死的一种形态是 read 空转不报错，
//     看门狗在静默超阈值时强制重开。只对"保证周期性输出日志"的设备开启
//     （如 30s 心跳），否则合法的安静设备会被复位循环打死。
type Collector struct {
	curBackoff time.Duration // 当前重开退避（测试可观测）
	dev        device.DeviceInfo
	cfg        config.Config
	w          *logstore.DeviceWriter
	sigs       *signature.SignatureEngine
	pause      *pause.PauseState
	stdlog     func(format string, args ...any)

	stopOnce sync.Once
	stop     chan struct{}
	sr       SuspendResume // 程序化让出/收回（release 与代理刷固件）

	portMu    sync.Mutex  // 保护 curPort（Port 是接口，atomic.Value 存异构实现会
	curPort   Port        // "inconsistently typed" panic；多测试假端口混用即触发）
	reopenReq atomic.Bool // 串口层软重连请求（置位后本循环退出即跳过退避重开）

	// 透传桥状态（见 proxy.go）
	proxyMu    sync.Mutex     // 保护 proxyConn
	proxyConn  net.Conn       // 当前代理客户端（单客户端，nil = 无会话）
	portWriter atomic.Value   // func([]byte) error —— 端口写入口
	tapExcl    *regexp.Regexp // 透传期间不落盘的行（proxy_tap_exclude）
}

func NewCollector(dev device.DeviceInfo, cfg config.Config, w *logstore.DeviceWriter,
	sigs *signature.SignatureEngine, p *pause.PauseState, stdlog func(string, ...any)) *Collector {
	if stdlog == nil {
		stdlog = func(string, ...any) {}
	}
	return &Collector{
		dev: dev, cfg: cfg, w: w, sigs: sigs, pause: p,
		stdlog: stdlog, stop: make(chan struct{}),
		tapExcl: compileTapExclude(cfg.ProxyTapExclude, stdlog),
	}
}

// Tty: 该采集器持有的串口路径。
func (c *Collector) Tty() string { return c.dev.Tty }

// DeviceName: 设备名（即日志目录名）。
func (c *Collector) DeviceName() string { return c.dev.Name }

// pauseState: 暴露暂停状态给同包测试（热替换用）。
func (c *Collector) pauseState() *pause.PauseState { return c.pause }

func (c *Collector) Stop() {
	c.stopOnce.Do(func() { close(c.stop) })
}

// Reopen: 串口层软断开重连 —— 关闭当前端口句柄，采集循环读错误退出后
// 跳过退避立即重开。不改变所有权与暂停语义（与 Suspend 不同），用于
// 端口疑似驱动/对端卡死时的快速自愈。会打断进行中的透传会话（客户端
// 按既有语义重连）。端口未开时仅置请求位，下次打开即按新句柄工作。
func (c *Collector) Reopen() {
	c.reopenReq.Store(true)
	// 读立即报错 → collectOnce 退出 → 立即重开。残留旧值无害（Close 幂等）。
	c.portMu.Lock()
	p := c.curPort
	c.portMu.Unlock()
	if p != nil {
		_ = p.Close()
	}
}

// event: 生命周期事件进 events 文件 + 守护进程 stdout。
func (c *Collector) event(format string, args ...any) {
	msg := fmt.Sprintf("[collector %s] %s", c.dev.Name, fmt.Sprintf(format, args...))
	_ = c.w.WriteEvent(msg)
	c.stdlog("%s", msg)
}

func (c *Collector) Run() {
	defer c.w.Close()
	c.event("collector started: tty=%s key=%s by-id=%s", c.dev.Tty, c.dev.Key, c.dev.ByID)

	backoff := time.Duration(c.cfg.ReopenMinS) * time.Second
	maxBackoff := time.Duration(c.cfg.ReopenMaxS) * time.Second
	for {
		select {
		case <-c.stop:
			c.event("collector stopped")
			return
		default:
		}

		if c.Held() || c.pause.Matches(c.dev) {
			c.event("port released — waiting (PAUSED file or explicit suspend)")
			if !c.waitHeldCleared() {
				return
			}
			c.event("hold cleared — resuming")
			continue
		}

		reason, openErr := c.collectOnce()

		// 成功 open 过的会话（任何非 openFailed 退出）把退避复位 ——
		// 否则长期运行中偶发断连会把 backoff 棘轮到上限，之后每次
		// 瞬断都白等 reopen_max_s（issue #4）
		if reason != reasonOpenFailed {
			backoff = time.Duration(c.cfg.ReopenMinS) * time.Second
		}

		select {
		case <-c.stop:
			c.event("collector stopped")
			return
		default:
		}
		manualReopen := c.reopenReq.CompareAndSwap(true, false)
		switch reason {
		case reasonPaused:
			// 外层循环处理等待
		case reasonOpenFailed:
			// 设备可能已拔走（watcher 会 Stop 我们）；也可能被别的进程占用
			// （EBUSY）——错误详情必须进事件流，否则排查全靠猜。指数退避防刷屏。
			c.curBackoff = backoff
			c.event("open failed: %v — retry in %s", openErr, backoff)
			if !c.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
		default:
			if manualReopen {
				// 串口层软重连（Reopen() 触发）：立即重开，不退避
				c.event("port cycle (manual reopen)")
				continue
			}
			c.event("port lost (%s) — reopening in %s", reason, backoff)
			if !c.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

type collectExit string

const (
	reasonStopped    collectExit = "stopped"
	reasonPaused     collectExit = "paused"
	reasonOpenFailed collectExit = "open-failed"
	reasonReadError  collectExit = "read-error"
	reasonSilent     collectExit = "silent-watchdog"
)

func (c *Collector) collectOnce() (collectExit, error) {
	port, err := OpenPort(c.dev.Tty, c.cfg.Baud)
	if err != nil {
		return reasonOpenFailed, err
	}
	c.portMu.Lock()
	c.curPort = port
	c.portMu.Unlock()
	c.sr.portOpen.Store(true)
	defer c.sr.portOpen.Store(false)
	// 关口 defer 先注册（LIFO 后执行）：必须先摘写入口再关端口。
	// 此前顺序相反 —— Close 与 proxy 泵的并发写竞态，Windows 重叠 IO
	// 未及取消时 Close 静默失败（错误被 _ = 吞掉），句柄泄漏在守护进程
	// 里，此后任何人（esptool/采集器重开）都打不开该口，只能重启守护
	// （2026-09-22 s3zero 实测：proxy 会话 + pause 后端口永久 busy）。
	defer func() {
		// 写入口已在上一个 defer 摘除；给在途写 50ms 收尾再关
		time.Sleep(50 * time.Millisecond)
		if err := port.Close(); err != nil {
			// 重叠 IO 取消可能瞬时失败：稍候重试一次，仍败则必须留痕
			time.Sleep(150 * time.Millisecond)
			if err2 := port.Close(); err2 != nil {
				c.event("port close error: %v / retry %v（句柄可能泄漏，重启守护可解）", err, err2)
			}
		}
	}()
	// 透传桥的写入口：端口存续期间登记，关闭即撤销（见 proxy.go）
	c.setPortWriter(port)
	defer c.setPortWriter(nil)
	// 见文件头注释第 2 条：open 后立即释放 DTR/RTS
	_ = port.SetDTR(false)
	_ = port.SetRTS(false)
	// 1s 读超时：喂看门狗检查、响应 stop/pause（注意 v1.8 API 是 Duration，
	// 传裸数字会成纳秒级忙轮询）
	_ = port.SetReadTimeout(time.Second)
	c.event("serial opened (%d baud)", c.cfg.Baud)

	var asm lineAssembler
	buf := make([]byte, 4096)
	lastRX := time.Now()
	for {
		n, err := port.Read(buf)
		if err != nil {
			// USB 重新枚举（拔出）在此抛错 —— 不能恋战死 fd
			c.event("read error: %v", err)
			c.flushTail(&asm)
			return reasonReadError, nil
		}
		select {
		case <-c.stop:
			c.flushTail(&asm)
			return reasonStopped, nil
		default:
		}
		if c.Held() || c.pause.Matches(c.dev) {
			c.event("paused — closing port")
			c.flushTail(&asm)
			return reasonPaused, nil
		}
		if n == 0 {
			if c.cfg.SilentReopenS > 0 &&
				time.Since(lastRX) > time.Duration(c.cfg.SilentReopenS)*time.Second {
				c.event("silent >%ds — forcing reopen", c.cfg.SilentReopenS)
				c.flushTail(&asm)
				return reasonSilent, nil
			}
			continue
		}
		lastRX = time.Now()
		c.proxyOut(buf[:n]) // 透传：原始字节镜像给代理客户端（无客户端时零开销）
		for _, line := range asm.feed(buf[:n]) {
			if c.proxyTapDrop(line) {
				// 透传期间的指定行不落全量日志（如高频遥测），签名照常
				if sig, ok := c.matchSig(line); ok {
					_ = c.w.WriteEvent(fmt.Sprintf("[%s] %s", sig, truncate(line, 200)))
				}
				continue
			}
			_ = c.w.WriteLine(line)
			if sig, ok := c.matchSig(line); ok {
				_ = c.w.WriteEvent(fmt.Sprintf("[%s] %s", sig, truncate(line, 200)))
			}
		}
	}
}

// flushTail: 端口关闭时把残余半行落盘（零丢失承诺 —— 半行以 …partial 标记）。
// matchSig: nil 引擎视为不匹配（防御；生产路径恒有引擎）。
func (c *Collector) matchSig(line string) (string, bool) {
	if c.sigs == nil {
		return "", false
	}
	return c.sigs.Match(line)
}

func (c *Collector) flushTail(asm *lineAssembler) {
	if t := asm.flush(); t != "" {
		_ = c.w.WriteLine("…partial " + t)
	}
}

func (c *Collector) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.stop:
		return false
	case <-t.C:
		return true
	}
}

// lineAssembler: 跨读取块的行拼装（残余半行保留在内部）。
type lineAssembler struct {
	tail []byte
}

const maxTail = 1 << 16 // 无换行的二进制泥石流保护：超限整块吐出

func (a *lineAssembler) feed(data []byte) []string {
	a.tail = append(a.tail, data...)
	if len(a.tail) > maxTail {
		out := string(a.tail)
		a.tail = a.tail[:0]
		return []string{out}
	}
	var lines []string
	for {
		i := bytes.IndexByte(a.tail, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSuffix(string(a.tail[:i]), "\r")
		a.tail = append(a.tail[:0], a.tail[i+1:]...) // copy=memmove，重叠安全
		lines = append(lines, line)
	}
	return lines
}

func (a *lineAssembler) flush() string {
	s := string(a.tail)
	a.tail = a.tail[:0]
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Key: 设备稳定身份（by-path）。
func (c *Collector) Key() string { return c.dev.Key }

// ByID: 设备的 by-id 字符串（可能为空）。
func (c *Collector) ByID() string { return c.dev.ByID }

// LogEvent: 外部（如代理刷固件）向事件流追加记录。
func (c *Collector) LogEvent(format string, args ...any) { c.event(format, args...) }

// Tty: 该采集器持有的串口路径。
