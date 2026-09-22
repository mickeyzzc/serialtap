package daemon

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

// proxyHarness: 单设备 + 可注入数据的假端口 + 已 Tick 的守护。
func proxyHarness(t *testing.T, tapExclude string) (*daemon, *testutil.FakePort, string) {
	t.Helper()
	root := t.TempDir()
	var fp *testutil.FakePort
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		fp = &testutil.FakePort{}
		return fp, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	cfg := config.DefaultConfig()
	cfg.Root = root
	cfg.PollMs = 50
	cfg.ProxyTapExclude = tapExclude
	devs := []device.DeviceInfo{{Tty: "/dev/fakeP", Key: "keyP", Name: "proxdev", ByID: "idP"}}
	d, err := New(cfg, nil, func() ([]device.DeviceInfo, error) { return devs, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	testutil.WaitFor(t, 3*time.Second, func() bool { return fp != nil }, "假端口未打开")
	return d, fp, root
}

func injectDev(t *testing.T, fp *testutil.FakePort, data string) {
	t.Helper()
	fp.Mu.Lock()
	fp.Chunks = append(fp.Chunks, []byte(data))
	fp.Mu.Unlock()
}

func TestProxyEndToEnd(t *testing.T) {
	d, fp, root := proxyHarness(t, "")

	ep, devName, devKey, err := d.ProxyStart("proxdev")
	if err != nil {
		t.Fatalf("ProxyStart: %v", err)
	}
	if devName != "proxdev" || devKey != "keyP" {
		t.Fatalf("ProxyStart 应回报端点所属设备: name=%q key=%q", devName, devKey)
	}
	conn, err := net.Dial("tcp", ep)
	if err != nil {
		t.Fatalf("dial %s: %v", ep, err)
	}
	defer conn.Close()
	waitProxyAttached(t, d) // Dial 早于 accept/挂接返回：等会话建立再注入

	// 设备 → 客户端：注入串口数据应镜像到 TCP
	injectDev(t, fp, "#S1 1 100 -50 ab\n")
	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("读镜像失败: %v", err)
	}
	if strings.TrimSpace(line) != "#S1 1 100 -50 ab" {
		t.Fatalf("镜像内容错误: %q", line)
	}

	// 客户端 → 设备：TCP 写入应落到串口
	if _, err := conn.Write([]byte("{\"cmd\":\"hello\"}\n")); err != nil {
		t.Fatalf("写透传: %v", err)
	}
	testutil.WaitFor(t, 3*time.Second, func() bool {
		fp.Mu.Lock()
		defer fp.Mu.Unlock()
		for _, w := range fp.Written {
			if strings.Contains(string(w), "\"cmd\":\"hello\"") {
				return true
			}
		}
		return false
	}, "客户端字节未写往串口")

	// 单客户端语义：第二个连接立即被关闭
	conn2, err := net.Dial("tcp", ep)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	defer conn2.Close()
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn2.Read(make([]byte, 16)); err == nil {
		t.Fatal("第二客户端应被拒绝（连接关闭）")
	}

	// tap：透传期间串口行照常落盘
	day := time.Now().Format("20060102")
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(testutil.ReadFile(t, filepath.Join(root, "proxdev", "serial-"+day+".log")), "#S1 1 100 -50 ab")
	}, "tap 日志未落盘")

	// status 应反映代理会话
	found := false
	for _, s := range d.Status() {
		if s.Name == "proxdev" && s.Proxy != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("status 未显示代理会话")
	}

	// stop：客户端连接应被关闭
	if _, err := d.ProxyStop("proxdev"); err != nil {
		t.Fatalf("ProxyStop: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 16)); err == nil {
		t.Fatal("stop 后客户端连接应关闭")
	}
}

func TestProxyTapExclude(t *testing.T) {
	d, fp, root := proxyHarness(t, `^#S1 `)

	if _, _, _, err := d.ProxyStart("proxdev"); err != nil {
		t.Fatalf("ProxyStart: %v", err)
	}
	conn, err := net.DialTimeout("tcp", mustFirstEndpoint(t, d), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	waitProxyAttached(t, d)

	injectDev(t, fp, "#S1 2 200 -51 cd\n")
	injectDev(t, fp, "I (100) main: ordinary log\n")
	day := time.Now().Format("20060102")
	logPath := filepath.Join(root, "proxdev", "serial-"+day+".log")
	testutil.WaitFor(t, 3*time.Second, func() bool {
		s := testutil.ReadFile(t, logPath)
		return strings.Contains(s, "ordinary log")
	}, "普通行未落盘")
	if strings.Contains(testutil.ReadFile(t, logPath), "#S1 2 200") {
		t.Fatal("命中 tap_exclude 的行不应落全量日志")
	}

	// 幂等：重复 ProxyStart 返回同一端点
	if ep2, n2, _, err := d.ProxyStart("proxdev"); err != nil || ep2 == "" || n2 != "proxdev" {
		t.Fatalf("重复 ProxyStart 应成功: %v", err)
	}
}

// waitProxyAttached: 轮询 status 直到代理会话出现（消除 Dial/accept 竞态）。
func waitProxyAttached(t *testing.T, d *daemon) {
	t.Helper()
	testutil.WaitFor(t, 3*time.Second, func() bool {
		for _, s := range d.Status() {
			if s.Proxy != "" {
				return true
			}
		}
		return false
	}, "代理会话未挂接")
}

func mustFirstEndpoint(t *testing.T, d *daemon) string {
	t.Helper()
	ep, _, _, err := d.ProxyStart("proxdev")
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestProxyStartNoMatch(t *testing.T) {
	d, _, _ := proxyHarness(t, "")
	if _, _, _, err := d.ProxyStart("nosuchdev"); err == nil {
		t.Fatal("无匹配设备应报错")
	}
	_ = fmt.Sprint(d.Collectors())
}
