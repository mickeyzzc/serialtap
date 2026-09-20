package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/collector"
	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/device"
	"github.com/mickeyzzc/serialtap/internal/pause"
	"github.com/mickeyzzc/serialtap/internal/testutil"
)

func TestLifecycle(t *testing.T) {
	root := t.TempDir()
	// 每次 open 都给全新假端口（两个采集器不能共享同一份待读数据）
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{Chunks: [][]byte{[]byte("hello from dev\n")}}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	cfg := config.DefaultConfig()
	cfg.Root = root
	cfg.PollMs = 10

	var devs = []device.DeviceInfo{
		{Tty: "/dev/fakeA", Key: "keyA", Name: "fakeA", ByID: "idA"},
		{Tty: "/dev/fakeB", Key: "keyB", Name: "fakeB", ByID: "idB"},
	}
	d, err := New(cfg, nil, func() ([]device.DeviceInfo, error) { return devs, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Tick()
	if d.Collectors() != 2 {
		t.Fatalf("应起 2 个采集器: %d", d.Collectors())
	}
	day := time.Now().Format("20060102")
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return strings.Contains(testutil.ReadFile(t, filepath.Join(root, "fakeA", "serial-"+day+".log")), "hello from dev")
	}, "daemon 采集器未落盘")

	// 同 key 再 Tick 不重复起
	d.Tick()
	if d.Collectors() != 2 {
		t.Fatalf("重复起采集器: %d", d.Collectors())
	}

	// 设备移除 → 采集器停止并释放名字
	devs = devs[:1]
	d.Tick()
	if d.Collectors() != 1 {
		t.Fatalf("设备移除后应剩 1 个: %d", d.Collectors())
	}

	// 重名设备 → -2 后缀
	devs = append(devs, device.DeviceInfo{Tty: "/dev/fakeA2", Key: "keyA2", Name: "fakeA"})
	d.Tick()
	if n := d.Name("keyA2"); n != "fakeA-2" {
		t.Fatalf("重名后缀错误: %q", n)
	}

	// 暂停清单热重载
	os.WriteFile(pause.PauseFilePath(root), []byte("fakeA\n"), 0o644)
	d.Tick() // 触发 reloadPause（mtime 变更）
	testutil.WaitFor(t, 3*time.Second, func() bool {
		return d.Paused("fakeA")
	}, "暂停清单未热重载")

	d.Shutdown()
}

func TestEnumErrorDoesNotPanic(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Root = t.TempDir()
	d, err := New(cfg, nil, func() ([]device.DeviceInfo, error) { return nil, os.ErrNotExist }, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.Tick() // 只记日志不崩
	d.Shutdown()
}
