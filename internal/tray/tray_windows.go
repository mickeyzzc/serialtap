//go:build windows

package tray

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
)

var shellExecuteW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteW")

// OpenPath: 用系统默认方式打开文件/目录（日志 → 默认编辑器；目录 → 资源管理器）。
func OpenPath(path string) {
	if path == "" {
		return
	}
	verb, _ := windows.UTF16PtrFromString("open")
	p, _ := windows.UTF16PtrFromString(path)
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(path))
	_, _, _ = shellExecuteW.Call(0,
		uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(p)),
		0, uintptr(unsafe.Pointer(dir)), 1) // SW_SHOWNORMAL
}

// StartDaemon: 启动一个后台守护进程（serialtap run，无窗口）。
func StartDaemon(sockPath, root string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "run", "--sock", sockPath, "--root", root)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
	return cmd.Start()
}

// debugf: 设 SERIALTAP_TRAY_DEBUG=1 时把点击/动作事件追加到 <root>/tray-debug.log
// （托盘无控制台，排查"点了没反应"类问题全靠它）。
var debugMu sync.Mutex

func (t *trayUI) debugf(format string, args ...any) {
	if os.Getenv("SERIALTAP_TRAY_DEBUG") == "" {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	f, err := os.OpenFile(filepath.Join(t.root, "tray-debug.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// Run: 进入托盘主循环（阻塞至"退出"）。
func Run(sockPath, root string, poll time.Duration) error {
	t := &trayUI{
		sockPath: sockPath, root: root, poll: poll,
		states: map[string]string{}, devUIs: map[string]*devUI{},
	}
	systray.Run(t.onReady, func() {})
	return nil
}

type trayUI struct {
	sockPath, root string
	poll           time.Duration

	mu       sync.Mutex // 保护 states/lastHash/devUIs 与菜单更新
	states   map[string]string
	lastHash string

	header      *systray.MenuItem
	startDaemon *systray.MenuItem
	devUIs      map[string]*devUI
}

// devUI: 一台设备的菜单项簇（按设备名长期复用，状态变化只改标题/勾选，
// 不重建 —— 重建会换掉 ClickedCh 对应的菜单项，goroutine 也会泄漏）。
type devUI struct {
	item    *systray.MenuItem
	hidden  bool
	logPath string // 缓存最新日志路径，子菜单点击时直接用
}

func (t *trayUI) onReady() {
	systray.SetIcon(buildIcon(false))

	t.header = systray.AddMenuItem("serialtap", "serialtap")
	t.header.Disable()
	systray.AddSeparator()
	for _, s := range []struct {
		title, tip string
		act        func()
	}{
		// 全部暂停逐台发锚定 pattern（而非空 pattern 的 ".*" 通配）——
		// 否则设备勾选框的 ^name$ 恢复无法移除 .* 条目，恢复会失效
		{"全部暂停", "暂停所有设备的采集", t.pauseEach},
		{"全部恢复", "恢复所有设备的采集", func() { t.send("resume", "") }}, // 空 = 清空 PAUSED
		{"刷新状态", "立即刷新设备状态", nil},
	} {
		it := systray.AddMenuItem(s.title, s.tip)
		item, act := it, s.act
		// 处理体不允许慢：ClickedCh 无缓冲且分发端 select/default，
		// 接收方必须立刻回到 channel 等下一击（刷新走异步）
		go func() {
			for range item.ClickedCh {
				t.debugf("click: %s", s.title)
				if act != nil {
					act()
				}
				go t.refreshSoon()
			}
		}()
	}
	t.startDaemon = systray.AddMenuItem("启动守护进程", "后台运行 serialtap run")
	go func() {
		for range t.startDaemon.ClickedCh {
			t.debugf("click: 启动守护进程")
			if err := StartDaemon(t.sockPath, t.root); err != nil {
				t.debugf("start daemon: %v", err)
			}
			go t.refreshSoon()
		}
	}()
	systray.AddSeparator()
	quit := systray.AddMenuItem("退出", "退出托盘（不影响守护进程采集）")
	go func() {
		for range quit.ClickedCh {
			t.debugf("click: 退出")
			systray.Quit()
		}
	}()
	systray.AddSeparator()

	go t.loop()
}

// send: 发 pause/resume 并记日志（pattern 空 = 全部）。
func (t *trayUI) send(cmd, pattern string) {
	t.debugf("send %s pattern=%q …", cmd, pattern)
	var err error
	if cmd == "pause" {
		err = SendPause(t.sockPath, pattern)
	} else {
		err = SendResume(t.sockPath, pattern)
	}
	if err != nil {
		t.debugf("send %s: %v", cmd, err)
	}
}

// pauseEach: 逐台发锚定暂停（见 onReady 注释）。
func (t *trayUI) pauseEach() {
	t.mu.Lock()
	names := make([]string, 0, len(t.states))
	for n := range t.states {
		names = append(names, n)
	}
	t.mu.Unlock()
	for _, n := range names {
		t.send("pause", PatternFor(n))
	}
}

func (t *trayUI) loop() {
	t.refreshOnce()
	tk := time.NewTicker(t.poll)
	defer tk.Stop()
	for range tk.C {
		t.refreshOnce()
	}
}

// refreshSoon: 命令发出后稍等生效再拉取（daemon 侧 pause 有 ≤1s 检查点延迟）。
func (t *trayUI) refreshSoon() {
	time.Sleep(700 * time.Millisecond)
	t.refreshOnce()
}

// refreshOnce: 轮询 status；状态有变才更新菜单。设备项按名字复用：
// 新设备建项、消失设备隐藏、既有设备只改标题/勾选 —— 菜单对象保持稳定，
// 点击通道（ClickedCh）始终对应可见的那一项。
func (t *trayUI) refreshOnce() {
	devs, ok := QueryStatus(t.sockPath)
	devs = SortedDevices(devs)
	h := SnapshotHash(devs, ok)
	t.mu.Lock()
	defer t.mu.Unlock()
	if h == t.lastHash {
		return
	}
	t.lastHash = h
	t.states = map[string]string{}

	if !ok {
		t.debugf("daemon 不可达")
		systray.SetTooltip("serialtap — 守护进程未运行")
		systray.SetIcon(buildIcon(true))
		t.header.SetTitle("serialtap — 守护进程未运行")
		t.startDaemon.Enable()
		for _, ui := range t.devUIs {
			if !ui.hidden {
				ui.item.Hide()
				ui.hidden = true
			}
		}
		return
	}
	t.startDaemon.Disable()
	systray.SetIcon(buildIcon(false))

	collecting := 0
	seen := map[string]bool{}
	for _, d := range devs {
		seen[d.Name] = true
		t.states[d.Name] = d.State
		if d.State == "collecting" {
			collecting++
		}
		ui := t.devUIs[d.Name]
		if ui == nil {
			ui = &devUI{item: systray.AddMenuItemCheckbox(DeviceTitle(d), d.Key, Collecting(d.State))}
			logItem := ui.item.AddSubMenuItem("查看串口日志", "打开最新全量日志")
			dirItem := ui.item.AddSubMenuItem("打开日志目录", "在资源管理器中打开")
			name := d.Name
			go func() {
				for range logItem.ClickedCh {
					t.debugf("click: 查看串口日志 %s → %s", name, ui.logPath)
					OpenPath(ui.logPath)
				}
			}()
			go func() {
				for range dirItem.ClickedCh {
					t.debugf("click: 打开日志目录 %s", name)
					OpenPath(filepath.Join(t.root, name))
				}
			}()
			go t.watchToggle(ui, name)
			t.devUIs[d.Name] = ui
		} else {
			// 复用：只更新标题与勾选，不换菜单对象
			ui.item.SetTitle(DeviceTitle(d))
			if Collecting(d.State) {
				ui.item.Check()
			} else {
				ui.item.Uncheck()
			}
			if ui.hidden {
				ui.item.Show()
				ui.hidden = false
			}
		}
		ui.logPath = LatestSerialLog(t.root, d.Name)
	}
	for name, ui := range t.devUIs {
		if !seen[name] && !ui.hidden {
			ui.item.Hide()
			ui.hidden = true
		}
	}
	tip := fmt.Sprintf("serialtap — %d 台设备，%d 台采集中", len(devs), collecting)
	systray.SetTooltip(tip)
	t.header.SetTitle(tip)
}

// watchToggle: 设备勾选框点击 = 切换该设备接入。以本地缓存的 daemon 状态决定
// 方向（不读 UI 勾选态 —— 它与真实状态可能短暂不一致）。
func (t *trayUI) watchToggle(ui *devUI, name string) {
	for range ui.item.ClickedCh {
		t.mu.Lock()
		collecting := t.states[name] == "collecting"
		t.mu.Unlock()
		t.debugf("click: 切换 %s（当前 %s）", name, map[bool]string{true: "采集中→暂停", false: "已停→恢复"}[collecting])
		if collecting {
			t.send("pause", PatternFor(name))
		} else {
			t.send("resume", PatternFor(name))
		}
		go t.refreshSoon()
	}
}

// —— 程序化生成托盘图标（32x32 32bpp ICO，深蓝底白色 S；断连时灰阶）——

func buildIcon(dim bool) []byte {
	const size = 32
	glyph := [7]string{
		".###.",
		"#....",
		"#....",
		".###.",
		"....#",
		"....#",
		"###..",
	}
	br, bg, bb := 30, 58, 95 // #1E3A5F
	if dim {
		br, bg, bb = 128, 128, 128
	}
	var px []byte // BMP 像素自底向上、BGRA
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			r, g, b := br, bg, bb
			gx, gy := (x-9)/3, (y-6)/3 // 字形 3 倍缩放居中
			if gx >= 0 && gx < 5 && gy >= 0 && gy < 7 && glyph[gy][gx] == '#' {
				r, g, b = 255, 255, 255
			}
			px = append(px, byte(b), byte(g), byte(r), 255)
		}
	}
	mask := make([]byte, size*size/8) // AND 掩码全 0 = 不透明
	bmp := make([]byte, 40)
	binary.LittleEndian.PutUint32(bmp[0:], 40)
	binary.LittleEndian.PutUint32(bmp[4:], size)
	binary.LittleEndian.PutUint32(bmp[8:], size*2) // XOR+AND 两倍高
	binary.LittleEndian.PutUint16(bmp[12:], 1)
	binary.LittleEndian.PutUint16(bmp[14:], 32)
	data := append(append(bmp, px...), mask...)

	ico := make([]byte, 6, 6+16+len(data))
	binary.LittleEndian.PutUint16(ico[2:], 1) // type: 1 = RT_ICON
	binary.LittleEndian.PutUint16(ico[4:], 1) // 图标数
	ico = append(ico, make([]byte, 16)...)
	ico[6], ico[7] = size, size
	binary.LittleEndian.PutUint16(ico[10:], 1)  // planes
	binary.LittleEndian.PutUint16(ico[12:], 32) // bpp
	binary.LittleEndian.PutUint32(ico[14:], uint32(len(data)))
	binary.LittleEndian.PutUint32(ico[18:], 22) // 数据起始偏移（6+16）
	return append(ico, data...)
}
