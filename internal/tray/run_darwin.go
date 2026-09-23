//go:build darwin && cgo

package tray

import (
	_ "embed"
	"os"
	"os/exec"
	"strconv"
	"time"

	"fyne.io/systray"
)

// sshWarn: SSH 会话下 NSStatusBar 拿不到 systemStatusBar，图标静默不显示。
func sshWarn(getenv func(string) string) string {
	if getenv("SSH_TTY") != "" || getenv("SSH_CONNECTION") != "" {
		return "[tray] 警告: 检测到 SSH 会话 —— 菜单栏图标需要本机 GUI 登录会话，SSH 下不会显示（可 --no-tray）"
	}
	return ""
}

// logf: nil 安全的日志快捷方式（logf 只被本文件使用，放在 darwin 文件里
// 避免在 Linux lint 视角下成为死代码）。
func (h Host) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

//go:embed icon.png
var iconBytes []byte // 菜单栏 template 图标（黑+透明，随深/浅色菜单栏自适应）

const maxDevRows = 8 // 状态区设备行槽位（超出折叠为 "…等 N 台"）

var (
	mTitle   *systray.MenuItem
	mNoDev   *systray.MenuItem
	mDevRows []*systray.MenuItem
	mPause   *systray.MenuItem
	mResume  *systray.MenuItem
	mOpen    *systray.MenuItem
	mWeb     *systray.MenuItem // PanelURL 空 = 未创建（nil channel = 分支禁用）
	mQuit    *systray.MenuItem
)

// Supported: 当前构建是否有菜单栏托盘。
func Supported() bool { return true }

// Run: 进驻 macOS 菜单栏并阻塞到退出。守护循环 daemonLoop 搬到 goroutine
// （macOS UI 必须占主线程）；任一侧先退出（菜单"退出" / 终端 Ctrl-C）都会
// 收掉另一侧，Run 返回后调用方做最终清理。
func Run(h Host, daemonLoop func()) {
	loopDone := make(chan struct{})
	go func() {
		daemonLoop()
		close(loopDone)
	}()
	go func() {
		<-loopDone // 守护先退（SIGINT/SIGTERM）→ 同步收掉菜单栏
		systray.Quit()
	}()
	systray.Run(func() { onReady(h) }, h.Quit)
	<-loopDone
}

func onReady(h Host) {
	systray.SetTemplateIcon(iconBytes, iconBytes)
	systray.SetTooltip("serialtap v" + h.Version)
	systray.SetTitle("")
	if w := sshWarn(os.Getenv); w != "" {
		h.logf("%s", w)
	}
	// NSStatusBar 在无 GUI 会话下静默返回 nil（图标不显示也不报错）——
	// 就绪日志是"图标应该出现了"的唯一线索，请对照菜单栏确认。
	h.logf("[tray] 菜单栏图标已创建（仅 run 运行期间显示）")

	mTitle = systray.AddMenuItem("serialtap v"+h.Version, "")
	mTitle.Disable()
	systray.AddSeparator()

	mNoDev = systray.AddMenuItem("（无 USB 串口设备）", "")
	mNoDev.Disable()
	for i := 0; i < maxDevRows; i++ {
		row := systray.AddMenuItem("", "")
		row.Disable()
		row.Hide()
		mDevRows = append(mDevRows, row)
	}
	refresh(h)

	systray.AddSeparator()
	mPause = systray.AddMenuItem("⏸ 暂停全部采集", "写入 PAUSED 清单并立即生效")
	mResume = systray.AddMenuItem("▶ 恢复全部采集", "清空 PAUSED 清单并立即生效")
	mOpen = systray.AddMenuItem("📂 打开日志目录", "在 Finder 中打开")
	if h.PanelURL != "" {
		mWeb = systray.AddMenuItem("🌐 打开 Web 面板", h.PanelURL)
	}
	systray.AddSeparator()
	mQuit = systray.AddMenuItem("⏏ 退出 serialtap", "")

	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for range tick.C {
			refresh(h)
		}
	}()
	go clickLoop(h)
}

func clickLoop(h Host) {
	// mWeb 未创建时用 nil channel 永久阻塞该分支（PanelURL 空 = 面板关闭）
	var webCh chan struct{}
	if mWeb != nil {
		webCh = mWeb.ClickedCh
	}
	for {
		select {
		case <-mPause.ClickedCh:
			if err := h.PauseAll(); err != nil {
				mPause.SetTitle("⏸ 暂停失败: " + err.Error())
			}
			refresh(h)
		case <-mResume.ClickedCh:
			if err := h.ResumeAll(); err != nil {
				mResume.SetTitle("▶ 恢复失败: " + err.Error())
			}
			refresh(h)
		case <-mOpen.ClickedCh:
			_ = h.OpenLogs()
		case <-webCh:
			_ = exec.Command("open", h.PanelURL).Start()
		case <-mQuit.ClickedCh:
			systray.Quit() // onExit → h.Quit 停守护循环
			return
		}
	}
}

// refresh: 拉取状态并更新菜单（只改既有项的标题与显隐，不重建菜单 ——
// 避开 Reset 期间用户点击的竞态）。SetTitle 跨线程由 systray 内部派发。
func refresh(h Host) {
	if h.Status == nil {
		return
	}
	lines := h.Status()
	for i, row := range mDevRows {
		if i < len(lines) && lines[i] != "" {
			row.SetTitle(lines[i])
			row.Show()
		} else {
			row.SetTitle("")
			row.Hide()
		}
	}
	if len(lines) > maxDevRows {
		mDevRows[maxDevRows-1].SetTitle("…等 " + strconv.Itoa(len(lines)) + " 台设备")
		mDevRows[maxDevRows-1].Show()
	}
	if len(lines) == 0 {
		mNoDev.Show()
	} else {
		mNoDev.Hide()
	}
}
