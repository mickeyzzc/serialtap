//go:build !darwin || !cgo

package tray

// Supported: 当前构建是否有托盘/菜单栏（否 —— Linux/Windows 或 CGO 关闭的 macOS）。
func Supported() bool { return false }

// Run: 无托盘构建。守护循环就是主循环，直接阻塞运行至其返回。
func Run(h Host, daemonLoop func()) {
	daemonLoop()
}
