// Package collector 实现单设备采集器：open-once-and-hold 纪律、
// DTR/RTS 释放、读错误弃 fd、可选静默看门狗、暂停响应。
package collector

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
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
type Port interface {
	Read(p []byte) (int, error)
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
	dev    device.DeviceInfo
	cfg    config.Config
	w      *logstore.DeviceWriter
	sigs   *signature.SignatureEngine
	pause  *pause.PauseState
	stdlog func(format string, args ...any)

	stopOnce sync.Once
	stop     chan struct{}
}

func NewCollector(dev device.DeviceInfo, cfg config.Config, w *logstore.DeviceWriter,
	sigs *signature.SignatureEngine, p *pause.PauseState, stdlog func(string, ...any)) *Collector {
	if stdlog == nil {
		stdlog = func(string, ...any) {}
	}
	return &Collector{
		dev: dev, cfg: cfg, w: w, sigs: sigs, pause: p,
		stdlog: stdlog, stop: make(chan struct{}),
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

		if c.pause.Matches(c.dev) {
			c.event("paused by PAUSED file — port closed, waiting")
			if !c.waitUnpause() {
				return
			}
			continue
		}

		reason, openErr := c.collectOnce()

		select {
		case <-c.stop:
			c.event("collector stopped")
			return
		default:
		}
		switch reason {
		case reasonPaused:
			// 外层循环处理等待
		case reasonOpenFailed:
			// 设备可能已拔走（watcher 会 Stop 我们）；也可能被别的进程占用
			// （EBUSY）——错误详情必须进事件流，否则排查全靠猜。指数退避防刷屏。
			c.event("open failed: %v — retry in %s", openErr, backoff)
			if !c.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
		default:
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
	// 见文件头注释第 2 条：open 后立即释放 DTR/RTS
	_ = port.SetDTR(false)
	_ = port.SetRTS(false)
	// 1s 读超时：喂看门狗检查、响应 stop/pause（注意 v1.8 API 是 Duration，
	// 传裸数字会成纳秒级忙轮询）
	_ = port.SetReadTimeout(time.Second)
	defer func() { _ = port.Close() }()
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
		if c.pause.Matches(c.dev) {
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
		for _, line := range asm.feed(buf[:n]) {
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

func (c *Collector) waitUnpause() bool {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.stop:
			return false
		case <-tick.C:
			if !c.pause.Matches(c.dev) {
				c.event("unpaused — resuming")
				return true
			}
		}
	}
}

// lineAssembler: 跨读取块的行拼装（残余半行保留在内部）。
type lineAssembler struct {
	tail []byte
}

const maxTail = 1 << 16 // 无换行的二进制泥石流保护：超限整块吐出

func (a *lineAssembler) feed(data []byte) []string {
	a.tail = append(a.tail, data...)
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
	// Split first, then flood-protect the unterminated remainder. A single
	// read that is both huge and contains newlines must still emit real
	// lines; dumping the whole buffer would smuggle CR/LF into a "line".
	if len(a.tail) > maxTail {
		lines = append(lines, string(a.tail))
		a.tail = a.tail[:0]
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
