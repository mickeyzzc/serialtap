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

// Run: 进入托盘主循环（阻塞至"退出"）。
func Run(sockPath, root string, poll time.Duration) error {
	t := &trayUI{sockPath: sockPath, root: root, poll: poll, states: map[string]string{}}
	systray.Run(t.onReady, func() {})
	return nil
}

type trayUI struct {
	sockPath, root string
	poll           time.Duration

	mu      sync.Mutex // 保护 states/lastHash 与菜单重建
	states  map[string]string
	lastHash string

	header      *systray.MenuItem
	startDaemon *systray.MenuItem
	devItems    []*systray.MenuItem
}

func (t *trayUI) onReady() {
	systray.SetIcon(buildIcon(false))
	systray.SetTitle("")

	t.header = systray.AddMenuItem("serialtap", "serialtap")
	t.header.Disable()
	systray.AddSeparator()
	for _, s := range []struct{ title, tip string; ch func() }{
		{"全部暂停", "暂停所有设备的采集", func() { _ = SendPause(t.sockPath, "") }},
		{"全部恢复", "恢复所有设备的采集", func() { _ = SendResume(t.sockPath, "") }},
		{"刷新状态", "立即刷新设备状态", nil},
	} {
		it := systray.AddMenuItem(s.title, s.tip)
		item, act := it, s.ch
		go func() {
			for range item.ClickedCh {
				if act != nil {
					act()
				}
				t.refreshSoon()
			}
		}()
	}
	t.startDaemon = systray.AddMenuItem("启动守护进程", "后台运行 serialtap run")
	go func() {
		for range t.startDaemon.ClickedCh {
			_ = StartDaemon(t.sockPath, t.root)
			t.refreshSoon()
		}
	}()
	systray.AddSeparator()
	quit := systray.AddMenuItem("退出", "退出托盘（不影响守护进程采集）")
	go func() {
		for range quit.ClickedCh {
			systray.Quit()
		}
	}()
	systray.AddSeparator()

	go t.loop()
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

// refreshOnce: 轮询 status，状态有变才重建动态设备区。
// 设备项是 checkbox：勾选 = 采集中，点击即暂停/恢复（"接入开关"）。
func (t *trayUI) refreshOnce() {
	devs, ok := QueryStatus(t.sockPath)
	h := SnapshotHash(devs, ok)
	t.mu.Lock()
	defer t.mu.Unlock()
	if h == t.lastHash {
		return
	}
	t.lastHash = h
	for _, it := range t.devItems {
		it.Hide()
	}
	t.devItems = nil
	t.states = map[string]string{}

	if !ok {
		systray.SetTooltip("serialtap — 守护进程未运行")
		systray.SetIcon(buildIcon(true))
		t.header.SetTitle("serialtap — 守护进程未运行")
		t.startDaemon.Enable()
		return
	}
	t.startDaemon.Disable()
	systray.SetIcon(buildIcon(false))

	collecting := 0
	for _, d := range devs {
		t.states[d.Name] = d.State
		if d.State == "collecting" {
			collecting++
		}
		item := systray.AddMenuItemCheckbox(DeviceTitle(d), d.Key, Collecting(d.State))
		name := d.Name
		logItem := item.AddSubMenuItem("查看串口日志", "打开最新全量日志")
		dirItem := item.AddSubMenuItem("打开日志目录", "在资源管理器中打开")
		go func() {
			for range logItem.ClickedCh {
				OpenPath(LatestSerialLog(t.root, name))
			}
		}()
		go func() {
			for range dirItem.ClickedCh {
				OpenPath(filepath.Join(t.root, name))
			}
		}()
		go t.watchToggle(item, name)
		t.devItems = append(t.devItems, item)
	}
	tip := fmt.Sprintf("serialtap — %d 台设备，%d 台采集中", len(devs), collecting)
	systray.SetTooltip(tip)
	t.header.SetTitle(tip)
}

// watchToggle: checkbox 点击 = 切换该设备接入。以本地缓存的 daemon 状态决定方向，
// 不依赖 UI 勾选态（systray 的勾选回读与 UI 可见态可能短暂不一致）。
func (t *trayUI) watchToggle(item *systray.MenuItem, name string) {
	for range item.ClickedCh {
		t.mu.Lock()
		collecting := t.states[name] == "collecting"
		t.mu.Unlock()
		if collecting {
			_ = SendPause(t.sockPath, PatternFor(name))
		} else {
			_ = SendResume(t.sockPath, PatternFor(name))
		}
		t.refreshSoon()
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
