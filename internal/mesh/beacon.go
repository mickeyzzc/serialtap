package mesh

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Beacon: UDP 发现广播（JSON，单包）。fp = 主钥对节点 id 的 HMAC 指纹——
// 同 mesh 互认，跨 mesh 互不可见，且不泄露口令。
type Beacon struct {
	V    int    `json:"v"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port"` // mesh TCP 端口
	FP   string `json:"fp"`
}

const beaconMagic = "serialtap-mesh"

func encodeBeacon(b Beacon) []byte {
	b.V = protoVersion
	line, _ := json.Marshal(b)
	return append([]byte(beaconMagic+" "), line...)
}

func parseBeacon(data []byte) (Beacon, error) {
	var b Beacon
	prefix := []byte(beaconMagic + " ")
	if len(data) <= len(prefix) || string(data[:len(prefix)]) != string(prefix) {
		return b, fmt.Errorf("非 serialtap beacon 包")
	}
	if err := json.Unmarshal(data[len(prefix):], &b); err != nil {
		return b, err
	}
	if b.V != protoVersion {
		return b, fmt.Errorf("beacon 协议版本不兼容: %d", b.V)
	}
	if b.ID == "" || b.Name == "" || b.Port <= 0 || b.Port > 65535 {
		return b, fmt.Errorf("beacon 字段不完整")
	}
	return b, nil
}

// broadcastAddrs: 各非回环 IPv4 网口的定向广播地址（拿不到掩码时兜底
// 全局广播）。纯 stdlib net —— 三平台同一实现。
func broadcastAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{"255.255.255.255"}
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagBroadcast == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() {
				continue
			}
			out = append(out, directedBroadcast(ipnet))
		}
	}
	if len(out) == 0 {
		return []string{"255.255.255.255"}
	}
	return out
}

// directedBroadcast: 网段广播地址（主机位全 1）。
func directedBroadcast(n *net.IPNet) string {
	ip := n.IP.To4()
	mask := n.Mask
	if len(mask) != 4 {
		mask = net.CIDRMask(32, 32)
	}
	bc := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		bc[i] = ip[i] | ^mask[i]
	}
	return bc.String()
}

// Announcer: 周期广播。targets 是目标地址提供者（生产 = broadcastAddrs
// + mesh 端口；测试注入回环单播地址），每次广播全部目标各一包。
type Announcer struct {
	self    Beacon
	targets func() []string // 返回 "host:port" 列表
	logf    func(string, ...any)
	stop    chan struct{}
	done    chan struct{}
}

// NewAnnouncer: interval<=0 用默认 5s。
func NewAnnouncer(self Beacon, port int, interval time.Duration, targets func() []string, logf func(string, ...any)) *Announcer {
	self.Port = port
	if interval <= 0 {
		interval = 5 * time.Second
	}
	a := &Announcer{
		self:    self,
		targets: targets,
		logf:    logf,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(a.done)
		a.announceOnce(nil) // 启动即广播（不等第一个 tick）
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-t.C:
				a.announceOnce(nil)
			}
		}
	}()
	return a
}

// announceOnce: 经给定 UDP 连接（nil = 临时拨号）向全部目标发包。
func (a *Announcer) announceOnce(conn net.PacketConn) {
	if conn == nil {
		c, err := net.ListenPacket("udp", ":0")
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		conn = c
	}
	packet := encodeBeacon(a.self)
	for _, addr := range a.targets() {
		if _, err := conn.WriteTo(packet, mustUDPAddr(addr)); err != nil && a.logf != nil {
			a.logf("[mesh] beacon 发往 %s 失败: %v", addr, err)
		}
	}
}

func mustUDPAddr(s string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		return nil
	}
	return a
}

// Close: 停止广播并等待 goroutine 退出。
func (a *Announcer) Close() {
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
	<-a.done
}

// ListenBeacons: 在 UDP 端口上收 beacon，逐个回调（指纹校验在回调里做——
// 不同 mesh 的包静默忽略）。返回前阻塞调用者前需先 go 起来。
func ListenBeacons(port int, selfID string, sec *Secrets, onBeacon func(b Beacon, from net.Addr), logf func(string, ...any)) (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, fmt.Errorf("mesh beacon 监听失败（UDP %d）: %w", port, err)
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, rerr := conn.ReadFromUDP(buf)
			if rerr != nil {
				return // 连接关闭
			}
			b, perr := parseBeacon(buf[:n])
			if perr != nil {
				continue
			}
			if b.ID == selfID {
				continue // 自己的广播环回
			}
			if !sec.ValidFingerprint(b.ID, b.FP) {
				if logf != nil {
					logf("[mesh] 忽略不同密钥的节点 %s(%s) 的 beacon", b.Name, b.ID)
				}
				continue
			}
			onBeacon(b, from)
		}
	}()
	return conn, nil
}
