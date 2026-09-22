// proxy.go — 透明代理端点的生命周期：ctl proxy start/stop 为设备开/关
// 127.0.0.1 TCP 监听，客户端接入即挂到对应采集器的透传桥（单客户端）。
// 纯基础设施：只搬字节，不理解协议；设备移除/守护退出时联动收口。
package daemon

import (
	"fmt"
	"net"

	"github.com/mickeyzzc/serialtap/internal/collector"
)

// ProxyStart: 为匹配设备各开一个本地 TCP 监听（已开的复用，幂等），
// 返回第一个端点（key 序第一台）及其所属设备 name/key。多设备匹配时
// 全部开通，端点以 status 为准；回报设备身份供客户端校验"拨的就是
// 选中的那台"——多板同名场景下这是防连错板的关键一环。
func (d *daemon) ProxyStart(pattern string) (string, string, string, error) {
	keys, cs := d.matches(pattern)
	if len(cs) == 0 {
		return "", "", "", fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	d.proxyMu.Lock()
	defer d.proxyMu.Unlock()
	first, devName, devKey := "", "", ""
	for i := range keys { // 已有监听的匹配设备：复用端点
		if ln, ok := d.proxies[keys[i]]; ok && first == "" {
			first = ln.Addr().String()
			devName, devKey = cs[i].DeviceName(), keys[i]
		}
	}
	for i := range keys {
		key := keys[i]
		if _, ok := d.proxies[key]; ok {
			continue
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			d.logf("[proxy] 监听创建失败 %s: %v", cs[i].DeviceName(), err)
			continue
		}
		d.proxies[key] = ln
		if first == "" {
			first = ln.Addr().String()
			devName, devKey = cs[i].DeviceName(), key
		}
		go d.acceptLoop(ln, cs[i])
		cs[i].LogEvent("proxy endpoint open: %s", ln.Addr().String())
		d.logf("[proxy] %s 透传端点 %s", cs[i].DeviceName(), ln.Addr().String())
	}
	if first == "" {
		return "", "", "", fmt.Errorf("代理监听创建失败（详见守护日志）")
	}
	return first, devName, devKey, nil
}

// ProxyStop: 关闭匹配设备的监听并摘除活跃代理会话。返回关停数。
func (d *daemon) ProxyStop(pattern string) (int, error) {
	keys, cs := d.matches(pattern)
	if len(cs) == 0 {
		return 0, fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	d.proxyMu.Lock()
	defer d.proxyMu.Unlock()
	n := 0
	for i, key := range keys {
		if ln, ok := d.proxies[key]; ok {
			_ = ln.Close()
			delete(d.proxies, key)
			cs[i].DetachProxy()
			cs[i].LogEvent("proxy endpoint closed: %s", ln.Addr().String())
			n++
		}
	}
	return n, nil
}

// proxyEndpointOf: 设备当前的透传监听端点（未开 = 空串）。status 暴露给
// 客户端做"端点已存在则附加、不开新的"判定（所有权语义见 wifipulse link）。
func (d *daemon) proxyEndpointOf(key string) string {
	d.proxyMu.Lock()
	defer d.proxyMu.Unlock()
	if ln, ok := d.proxies[key]; ok {
		return ln.Addr().String()
	}
	return ""
}

// acceptLoop: 单客户端语义 —— 已有会话时拒绝新连接（关闭之）。
// 拒绝也留事件痕迹：面板上能看到"有人抢线被拒"，与接入/断开构成完整记录。
func (d *daemon) acceptLoop(ln net.Listener, c *collector.Collector) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // 监听已关（stop / 设备移除 / 退出）
		}
		if err := c.AttachProxy(conn); err != nil {
			c.LogEvent("proxy: 接入被拒 %s（%v）", conn.RemoteAddr(), err)
			_ = conn.Close()
			continue
		}
	}
}

// proxyRemove: 设备移除时收口其监听与会话（Tick 调用）。
func (d *daemon) proxyRemove(key string) {
	d.proxyMu.Lock()
	ln, ok := d.proxies[key]
	if ok {
		delete(d.proxies, key)
	}
	d.proxyMu.Unlock()
	if ok {
		_ = ln.Close()
	}
}

// proxyCloseAll: 守护退出时全量收口（Shutdown 调用）。
func (d *daemon) proxyCloseAll() {
	d.proxyMu.Lock()
	defer d.proxyMu.Unlock()
	for key, ln := range d.proxies {
		_ = ln.Close()
		delete(d.proxies, key)
	}
}
