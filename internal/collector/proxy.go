// proxy.go — 透明 USB 代理桥：业务程序（如感知引擎 aa）经 serialtap 的
// 本地 TCP 端点读写板子串口，对程序来说等同直接持有串口。
//
// 定位约束：serialtap 是基础设施，代理只做字节搬运，不理解也不处理
// 任何业务协议。桥接期间端口不重开（保持 open-once 纪律 —— 不产生
// 复位脉冲），采集照常（tap 模式：双向流量仍按行落盘，可用
// proxy_tap_exclude 正则剔除高频遥测行防刷盘）。
//
// 方向：
//
//	设备 → 客户端：collectOnce 读循环拿到字节后 proxyOut 镜像写出；
//	客户端 → 设备：AttachProxy 起的 pump 协程读 TCP，经登记的端口写入口写入。
package collector

import (
	"fmt"
	"net"
	"regexp"
	"time"
)

// proxy 写出阻塞保护：客户端不收（死了/慢了）时不能拖住采集读循环。
const proxyWriteTimeout = 200 * time.Millisecond

// setPortWriter: 登记/撤销端口写入口（collectOnce 开关口时调用）。
func (c *Collector) setPortWriter(p Port) {
	if p == nil {
		var nilFn func([]byte) error // atomic.Value 不收 nil 接口，存值为 nil 的函数
		c.portWriter.Store(nilFn)
		return
	}
	c.portWriter.Store(func(b []byte) error {
		_, err := p.Write(b)
		return err
	})
}

// AttachProxy: 挂接一个代理客户端（单客户端语义：已挂接时返回错误）。
// 调用方负责 accept 循环；本方法起 客户端→设备 泵协程。
func (c *Collector) AttachProxy(conn net.Conn) error {
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if c.proxyConn != nil {
		return fmt.Errorf("设备 %s 已有代理客户端", c.dev.Name)
	}
	c.proxyConn = conn
	c.event("proxy: 客户端接入 (%s)", conn.RemoteAddr())
	go c.proxyPump(conn)
	return nil
}

// DetachProxy: 摘除当前代理客户端（幂等）。端口让出/刷写/设备拔出时
// 由生命周期路径调用；泵协程读失败时自调。
func (c *Collector) DetachProxy() {
	c.proxyMu.Lock()
	conn := c.proxyConn
	c.proxyConn = nil
	c.proxyMu.Unlock()
	if conn != nil {
		_ = conn.Close()
		c.event("proxy: 客户端断开")
	}
}

// ProxyAddr: 当前代理客户端的远端地址（status 展示用；空 = 无会话）。
func (c *Collector) ProxyAddr() string {
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	if c.proxyConn == nil {
		return ""
	}
	return c.proxyConn.RemoteAddr().String()
}

// proxyPump: 客户端 → 设备 方向泵。读到 EOF/错误即摘除会话
// （端口暂时不在（让口/重开间隙）时写入失败同样摘除，客户端重连即可）。
// tap：该方向字节同样按行落盘（"> " 前缀标记方向，与板子回显区分），
// 使透传会话双向均可观测；剔除正则与落盘口径与设备方向一致。
func (c *Collector) proxyPump(conn net.Conn) {
	buf := make([]byte, 4096)
	var asm lineAssembler
	defer func() { // 摘除时残余半行照常落盘（零丢失口径）
		if t := asm.flush(); t != "" {
			_ = c.w.WriteLine("> …partial " + t)
		}
	}()
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			for _, line := range asm.feed(buf[:n]) {
				if !c.proxyTapDrop(line) {
					_ = c.w.WriteLine("> " + line)
				}
			}
			if werr := c.writeToPort(buf[:n]); werr != nil {
				c.event("proxy: 端口写入失败: %v", werr)
				c.DetachProxy()
				return
			}
		}
		if err != nil {
			c.DetachProxy()
			return
		}
	}
}

// writeToPort: 经登记的写入口写设备（端口未开时返回错误）。
func (c *Collector) writeToPort(b []byte) error {
	v := c.portWriter.Load()
	if v == nil {
		return fmt.Errorf("端口未打开")
	}
	w, ok := v.(func([]byte) error)
	if !ok || w == nil {
		return fmt.Errorf("端口未打开")
	}
	return w(b)
}

// proxyOut: 设备 → 客户端 镜像。写超时/失败即摘除（慢客户端不值得
// 牺牲采集循环），不阻塞无会话路径。
func (c *Collector) proxyOut(b []byte) {
	c.proxyMu.Lock()
	conn := c.proxyConn
	c.proxyMu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(proxyWriteTimeout))
	if _, err := conn.Write(b); err != nil {
		c.DetachProxy()
	}
}

// proxyTapDrop: 透传会话激活期间，命中 proxy_tap_exclude 的行不落
// 全量日志（防高频遥测刷盘）。无会话或未配置时恒 false。
func (c *Collector) proxyTapDrop(line string) bool {
	c.proxyMu.Lock()
	active := c.proxyConn != nil
	c.proxyMu.Unlock()
	if !active || c.tapExcl == nil {
		return false
	}
	return c.tapExcl.MatchString(line)
}

// compileTapExclude: 编译配置的透传剔除正则（坏正则忽略并告警）。
func compileTapExclude(pattern string, logf func(string, ...any)) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		if logf != nil {
			logf("[watch] 忽略坏正则 proxy_tap_exclude: %s", pattern)
		}
		return nil
	}
	return re
}
