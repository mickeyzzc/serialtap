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
	"github.com/mickeyzzc/serialtap/internal/flash"
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
	t.Cleanup(d.Shutdown) // 失败路径也要收尾（Windows 上打开的文件不可删除）
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

	// 重名设备 → 身份 token 后缀（同 key/by-id 的板无论何时接入后缀一致）
	devs = append(devs, device.DeviceInfo{Tty: "/dev/fakeA2", Key: "keyA2", Name: "fakeA"})
	d.Tick()
	if want := "fakeA-" + nameToken("keyA2", ""); d.Name("keyA2") != want {
		t.Fatalf("重名后缀错误: %q（期望 %q）", d.Name("keyA2"), want)
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

// —— release / flash 编排（fake esptool + 假端口，无硬件）——
func newTestDaemon(t *testing.T, root string, devs ...device.DeviceInfo) (*daemon, error) {
	cfg := config.DefaultConfig()
	cfg.Root = root
	cfg.PollMs = 10
	return New(cfg, nil, func() ([]device.DeviceInfo, error) { return devs, nil }, nil)
}

func TestReleaseUntilIdleAutoResumes(t *testing.T) {
	if !device.IdleDetectSupported() {
		t.Skip("此平台无端口占用检测（Windows：无 /proc/lsof），until_idle 由 Release 显式拒绝，见下")
	}
	root := t.TempDir()
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{Chunks: [][]byte{[]byte("x\n")}}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root, device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	testutil.WaitFor(t, 3*time.Second, func() bool { return d.Collectors() == 1 }, "采集器未起")

	// 让口：until_idle（无人持有 → idleQuietS 后自动回采）
	n, err := d.Release("fakeA", 0, true)
	if err != nil || n != 1 {
		t.Fatalf("Release 失败: n=%d err=%v", n, err)
	}
	if st := d.Status()[0].State; st != "suspended" {
		t.Fatalf("Release 后状态应为 suspended: %s", st)
	}
	// 手动驱动 tick 直到自动回采（3s 空闲确认）
	deadline := time.Now().Add(10 * time.Second)
	resumed := false
	for time.Now().Before(deadline) {
		d.Tick()
		if len(d.releases) == 0 {
			resumed = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !resumed {
		t.Fatal("until_idle 未自动回采")
	}
}

// Windows 等无占用检测的平台：until_idle 的 release 必须显式报错（而非静默误判
// "无人占用"导致 3s 后抢回口）
func TestReleaseUntilIdleRejectedOnUnsupportedPlatform(t *testing.T) {
	if device.IdleDetectSupported() {
		t.Skip("此平台支持空闲检测，跳过拒绝用例")
	}
	root := t.TempDir()
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root, device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	testutil.WaitFor(t, 3*time.Second, func() bool { return d.Collectors() == 1 }, "采集器未起")

	if _, err := d.Release("fakeA", 0, true); err == nil {
		t.Fatal("不支持空闲检测的平台 until_idle 应报错")
	}
	// 限时回采不受影响
	if _, err := d.Release("fakeA", time.Second, false); err != nil {
		t.Fatalf("限时 release 不应受影响: %v", err)
	}
}

func TestReleaseTimedAutoResumes(t *testing.T) {
	root := t.TempDir()
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root, device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"})
	if err != nil {
		t.Fatal(err)
	}
	d.Tick()
	if _, err := d.Release("fakeA", 100*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.Tick()
		if len(d.releases) == 0 {
			d.Shutdown()
			return // 到期自动回采 ✓
		}
		time.Sleep(100 * time.Millisecond)
	}
	d.Shutdown()
	t.Fatal("限时 release 未自动回采")
}

func TestFlashOrchestrationWithFakeEsptool(t *testing.T) {
	root := t.TempDir()
	a2l := testutil.FakeTool(t)
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "fake flashing\n")

	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root, device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"})
	if err != nil {
		t.Fatal(err)
	}
	// Windows 上文件被打开时无法删除：失败路径也要收掉采集协程，TempDir 清理才不炸
	t.Cleanup(d.Shutdown)
	d.Tick()

	var lines []string
	bin := filepath.Join(root, "app.bin")
	os.WriteFile(bin, []byte("x"), 0o644)
	err = d.Flash("fakeA", false, flash.Spec{
		Esptool: a2l, Bins: []flash.BinSpec{{Path: bin, Offset: "0x0"}},
	}, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatalf("代理刷失败: %v", err)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "fake flashing") {
		t.Fatalf("esptool 输出未流式回传: %v", lines)
	}
	// 刷完自动回采
	if c := d.collectors["kA"]; c == nil || c.State() != "collecting" {
		t.Fatalf("刷完未回采: %+v", d.Status())
	}
	// 无匹配设备
	if err := d.Flash("nope", false, flash.Spec{}, func(string) {}); err == nil {
		t.Fatal("无匹配设备应报错")
	}
}

// —— resume 无参必须清空整个 PAUSED（与无参 pause 对称）——
func TestResumeAllClearsEntirePausedFile(t *testing.T) {
	root := t.TempDir()
	pause.PauseCLI(root, true, []string{"a", "b"}) // PAUSED 含两条
	d, err := newTestDaemon(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := d.ResumeAll(""); err != nil || n == 0 {
		t.Fatalf("ResumeAll 失败: n=%d err=%v", n, err)
	}
	st, _, _ := pause.LoadPauseFile(root)
	if st.Len() != 0 {
		t.Fatalf("PAUSED 未清空: %d 条", st.Len())
	}
	// 带参 resume 只删单条
	pause.PauseCLI(root, true, []string{"a", "b"})
	if n, _ := d.ResumeAll("a"); n == 0 {
		t.Fatal("单条 resume 失败")
	}
	st, _, _ = pause.LoadPauseFile(root)
	if st.Len() != 1 || !st.Matches(device.DeviceInfo{Name: "b"}) {
		t.Fatalf("单条 resume 语义错误: len=%d", st.Len())
	}
	d.Shutdown()
}

// —— G2/G3：并发互斥、撤销 pending release、dry-run 预演 ——

// flash 进行中：并发的 flash/release/resume 必须 fail-fast 报错
// （否则两个 esptool 抢同一口 / resume 在 esptool 工作中途抢回口）
func TestFlashRejectsConcurrentOps(t *testing.T) {
	root := t.TempDir()
	tool := testutil.FakeTool(t)
	t.Setenv("FAKE_SLEEP", "800ms")
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "slow flashing\n")

	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root, device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	bin := filepath.Join(root, "app.bin")
	os.WriteFile(bin, []byte("x"), 0o644)

	done := make(chan error, 1)
	go func() {
		done <- d.Flash("fakeA", false, flash.Spec{Esptool: tool,
			Bins: []flash.BinSpec{{Path: bin, Offset: "0x0"}}}, func(string) {})
	}()
	// 确定性等到第一个 flash 持锁进入 esptool 阶段（盲睡在忙机器上会 flaky）
	testutil.WaitFor(t, 5*time.Second, func() bool {
		return d.Status()[0].State == "flashing"
	}, "首个 flash 未进入 flashing 状态")

	if _, err := d.Release("fakeA", time.Hour, false); err == nil {
		t.Fatal("flash 进行中 release 应报错")
	}
	if _, err := d.ResumeAll(""); err == nil {
		t.Fatal("flash 进行中 resume 应报错")
	}
	if err := <-done; err != nil {
		t.Fatalf("首个 flash 应成功: %v", err)
	}
	// 完成后恢复可用
	if _, err := d.ResumeAll(""); err != nil {
		t.Fatalf("结束后 resume 不应报错: %v", err)
	}
}

// 刷写开始时撤销 pending 的限时 release —— 否则到期会在 esptool 工作中途抢回口
func TestFlashRevokesPendingTimedRelease(t *testing.T) {
	root := t.TempDir()
	tool := testutil.FakeTool(t)
	t.Setenv("FAKE_SLEEP", "")
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "flashing\n")

	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root, device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	bin := filepath.Join(root, "app.bin")
	os.WriteFile(bin, []byte("x"), 0o644)

	// 限时 1 小时的 release 挂着，然后刷写
	if _, err := d.Release("fakeA", time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if err := d.Flash("fakeA", false, flash.Spec{Esptool: tool,
		Bins: []flash.BinSpec{{Path: bin, Offset: "0x0"}}}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	pending := len(d.releases)
	d.mu.Unlock()
	if pending != 0 {
		t.Fatalf("刷写后 pending release 应被撤销: %d", pending)
	}
	if st := d.Status()[0].State; st != "collecting" {
		t.Fatalf("刷完应回采: %s", st)
	}
}

// dry-run：daemon 侧解析 esptool 命令并回显，不动端口、不切状态、不执行
func TestFlashDryRun(t *testing.T) {
	root := t.TempDir()
	tool := testutil.FakeTool(t)
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "SHOULD-NOT-APPEAR\n")

	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root, device.DeviceInfo{Tty: "/dev/ttyFAKE", Key: "kA", Name: "fakeA"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	testutil.WaitFor(t, 3*time.Second, func() bool { return d.Collectors() == 1 }, "采集器未起")
	bin := filepath.Join(root, "app.bin")
	os.WriteFile(bin, []byte("x"), 0o644)

	var lines []string
	err = d.Flash("fakeA", false, flash.Spec{Esptool: tool, DryRun: true,
		Bins: []flash.BinSpec{{Path: bin, Offset: "0x0"}}}, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "write_flash") || !strings.Contains(joined, "fakeA") {
		t.Fatalf("dry-run 应回显 esptool 命令: %v", lines)
	}
	if strings.Contains(joined, "SHOULD-NOT-APPEAR") {
		t.Fatal("dry-run 不应执行 esptool")
	}
	if st := d.Status()[0].State; st != "collecting" {
		t.Fatalf("dry-run 不应改变状态: %s", st)
	}
}

// —— 多板同芯片（两只 303a:1001 同名）场景的确定性 ——
// 背景：homepulse 经 status 取"第一台" + ProxyStart 返回"第一个"端点，
// map 迭代随机时两层各掷骰子，业务程序会随机连到错误的板子上。

// 撞名 token 后缀跨守护重启稳定：同一块板（同 key/by-id）无论何时撞名，
// 拿到的后缀一致 —— 不随接入顺序/重启漂移。
func TestDuplicateNameTokenStableAcrossRestart(t *testing.T) {
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	devs := []device.DeviceInfo{
		{Tty: "/dev/a", Key: "keyA", Name: "dup", ByID: "idA"},
		{Tty: "/dev/b", Key: "keyB", Name: "dup", ByID: "idB"},
	}
	want := "dup-" + nameToken("keyB", "idB")

	d1, err := newTestDaemon(t, t.TempDir(), devs...)
	if err != nil {
		t.Fatal(err)
	}
	d1.Tick()
	if n := d1.Name("keyB"); n != want {
		t.Fatalf("首次后缀错误: %q（期望 %q）", n, want)
	}
	d1.Shutdown()

	// 全新守护（模拟重启）：同一对板再撞名，后缀不变
	d2, err := newTestDaemon(t, t.TempDir(), devs...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d2.Shutdown)
	d2.Tick()
	if n := d2.Name("keyB"); n != want {
		t.Fatalf("重启后后缀漂移: %q（期望 %q）", n, want)
	}
}

// status 与 matches 按稳定 key 序返回：逆序注入也要正序出来，
// "取第一台/第一个端点"的消费方才不会每次调用换目标。
func TestStatusAndMatchesSortedByKey(t *testing.T) {
	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	// 注入顺序与 key 序相反（假 enum 不经 buildDevices 排序）
	d, err := newTestDaemon(t, t.TempDir(),
		device.DeviceInfo{Tty: "/dev/z", Key: "kz", Name: "dz"},
		device.DeviceInfo{Tty: "/dev/a", Key: "ka", Name: "da"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()

	st := d.Status()
	if len(st) != 2 || st[0].Key != "ka" || st[1].Key != "kz" {
		t.Fatalf("status 应按 key 排序: %+v", st)
	}
	keys, cs := d.matches("d")
	if len(cs) != 2 || keys[0] != "ka" || keys[1] != "kz" {
		t.Fatalf("matches 应按 key 排序: %v", keys)
	}
}

// flash 多设备匹配默认拒绝（列出设备名并提示锚定/--all），all=true 才逐台刷
func TestFlashMultiDeviceGate(t *testing.T) {
	root := t.TempDir()
	tool := testutil.FakeTool(t)
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "flashing\n")

	old := collector.OpenPort
	collector.OpenPort = func(tty string, baud int) (collector.Port, error) {
		return &testutil.FakePort{}, nil
	}
	t.Cleanup(func() { collector.OpenPort = old })

	d, err := newTestDaemon(t, root,
		device.DeviceInfo{Tty: "/dev/a", Key: "ka", Name: "sense"},
		device.DeviceInfo{Tty: "/dev/b", Key: "kb", Name: "sense-dev"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Shutdown)
	d.Tick()
	bin := filepath.Join(root, "app.bin")
	os.WriteFile(bin, []byte("x"), 0o644)
	spec := flash.Spec{Esptool: tool, Bins: []flash.BinSpec{{Path: bin, Offset: "0x0"}}}

	// 未锚定 + 未确认 → 拒绝并列出两台
	err = d.Flash("sense", false, spec, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "2 台") ||
		!strings.Contains(err.Error(), "sense-dev") || !strings.Contains(err.Error(), "--all") {
		t.Fatalf("多台未确认应拒绝并列出设备: %v", err)
	}
	// 锚定单台 → 正常刷
	if err := d.Flash("^sense$", false, spec, func(string) {}); err != nil {
		t.Fatalf("锚定单台应可刷: %v", err)
	}
	// 显式 all → 逐台刷
	if err := d.Flash("sense", true, spec, func(string) {}); err != nil {
		t.Fatalf("all=true 应逐台刷: %v", err)
	}
}
