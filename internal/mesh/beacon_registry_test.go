package mesh

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestBeaconEncodeParse(t *testing.T) {
	sec := testSecrets(t, "k")
	b := Beacon{ID: "node-a1", Name: "bench", Port: 8802, FP: sec.Fingerprint("node-a1")}
	data := encodeBeacon(b)
	got, err := parseBeacon(data)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.ID != "node-a1" || got.Name != "bench" || got.Port != 8802 {
		t.Fatalf("字段错误: %+v", got)
	}
	if !sec.ValidFingerprint(got.ID, got.FP) {
		t.Fatal("指纹校验失败")
	}
	// 非本协议包 / 坏 JSON / 缺字段
	if _, err := parseBeacon([]byte("hello")); err == nil {
		t.Fatal("垃圾包必须被拒绝")
	}
	if _, err := parseBeacon([]byte("serialtap-mesh {")); err == nil {
		t.Fatal("坏 JSON 必须被拒绝")
	}
	bad := append([]byte(beaconMagic+" "), []byte(`{"v":99,"id":"x","name":"y","port":1,"fp":"z"}`)...)
	if _, err := parseBeacon(bad); err == nil {
		t.Fatal("版本不兼容必须被拒绝")
	}
}

func TestBeaconRoundtripUDP(t *testing.T) {
	// 真实 UDP 回环：announcer → 监听 socket → beacon 进注册表
	sec := testSecrets(t, "k")
	listen, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = listen.Close() }()
	target := listen.LocalAddr().String()

	self := Beacon{ID: "node-b2", Name: "srv", FP: sec.Fingerprint("node-b2")}
	a := NewAnnouncer(self, 8802, 10*time.Second, func() []string { return []string{target} }, nil)
	defer a.Close()

	buf := make([]byte, 2048)
	var got Beacon
	for {
		_ = listen.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, from, rerr := listen.ReadFromUDP(buf)
		if rerr != nil {
			t.Fatalf("3s 内未收到 beacon: %v", rerr)
		}
		b, perr := parseBeacon(buf[:n])
		if perr != nil {
			continue // 其它流量
		}
		if !sec.ValidFingerprint(b.ID, b.FP) {
			t.Fatal("回环包指纹必须通过")
		}
		if from.IP.String() != "127.0.0.1" {
			t.Fatalf("源地址异常: %v", from)
		}
		got = b
		break
	}
	r := NewRegistry(time.Minute)
	r.UpsertBeacon(got, net.IPv4(127, 0, 0, 1))
	if p, merr := r.Match("srv"); merr != nil || p.ID != "node-b2" {
		t.Fatalf("beacon → 注册表链路失败: %+v err=%v", p, merr)
	}
}

func TestRegistryLifecycle(t *testing.T) {
	r := NewRegistry(150 * time.Millisecond)

	// beacon 学习
	r.UpsertBeacon(Beacon{ID: "id1", Name: "bench1", Port: 8802}, net.IPv4(192, 168, 63, 10))
	p, err := r.Match("bench1")
	if err != nil || p.ID != "id1" || p.Addr != "192.168.63.10:8802" {
		t.Fatalf("beacon 学习失败: %+v err=%v", p, err)
	}
	// 唯一前缀
	if p, err = r.Match("ben"); err != nil || p.ID != "id1" {
		t.Fatalf("前缀匹配失败: %+v err=%v", p, err)
	}

	// 过期（beacon 学到的、无 Static）
	time.Sleep(160 * time.Millisecond)
	if got := r.Expire(); len(got) != 1 || got[0] != "id1" {
		t.Fatalf("过期摘除失败: %v", got)
	}
	if _, err = r.Match("bench1"); err == nil {
		t.Fatal("过期后不应再匹配")
	}

	// 静态种子：占位 → 学习身份 → 撤销
	r.SetStatic([]string{"192.168.63.20:8802"})
	p, err = r.Match("192.168.63.20:8802")
	if err != nil || !p.Static {
		t.Fatalf("静态占位匹配失败: %+v err=%v", p, err)
	}
	time.Sleep(160 * time.Millisecond)
	if got := r.Expire(); len(got) != 0 {
		t.Fatal("静态种子不应过期")
	}
	r.LearnedIdent("192.168.63.20:8802", Ident{ID: "id2", Name: "bench2"})
	if p, err = r.Match("bench2"); err != nil || p.ID != "id2" || !p.Static {
		t.Fatalf("静态身份回填失败: %+v err=%v", p, err)
	}
	r.SetStatic(nil)                   // 撤销配置
	time.Sleep(160 * time.Millisecond) // LearnedIdent 刚刷新过 LastSeen，等一个 TTL
	r.Expire()                         // Match 是名字解析；存活与否拨号时才判。这里先扫过期
	if _, err = r.Match("bench2"); err == nil {
		t.Fatal("撤销静态后（beacon 也已过期）不应再匹配")
	}

	// 歧义前缀
	r.UpsertBeacon(Beacon{ID: "id3", Name: "cam-a"}, net.IPv4(1, 2, 3, 4))
	r.UpsertBeacon(Beacon{ID: "id4", Name: "cam-b"}, net.IPv4(1, 2, 3, 5))
	_, err = r.Match("cam-")
	if err == nil || !strings.Contains(err.Error(), "歧义") {
		t.Fatalf("歧义前缀必须报错并列出候选: %v", err)
	}
	// host:port 直连兜底（不在注册表里）
	p, err = r.Match("10.0.0.9:8802")
	if err != nil || p.Addr != "10.0.0.9:8802" {
		t.Fatalf("地址直连兜底失败: %+v err=%v", p, err)
	}
}

func TestBroadcastAddrs(t *testing.T) {
	addrs := broadcastAddrs()
	if len(addrs) == 0 {
		t.Fatal("至少返回一个广播地址")
	}
	for _, a := range addrs {
		if net.ParseIP(a) == nil {
			t.Fatalf("非法广播地址: %s", a)
		}
	}
}

func TestDirectedBroadcast(t *testing.T) {
	_, ipnet, err := net.ParseCIDR("192.168.63.7/24")
	if err != nil {
		t.Fatal(err)
	}
	if got := directedBroadcast(ipnet); got != "192.168.63.255" {
		t.Fatalf("定向广播算错: %s", got)
	}
	_, ipnet, _ = net.ParseCIDR("10.1.2.3/8")
	if got := directedBroadcast(ipnet); got != "10.255.255.255" {
		t.Fatalf("定向广播算错: %s", got)
	}
}
