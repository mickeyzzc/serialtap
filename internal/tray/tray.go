// Package tray 提供守护进程的托盘/菜单栏管理入口。
//
// macOS（darwin + cgo）：run 命令进驻菜单栏，图标 + 状态区 + 暂停/恢复/打开
// 日志/退出；Linux/Windows 或 CGO 关闭时 Run 退化为直接阻塞跑守护循环
// （不引入任何 GUI 依赖，保持纯静态二进制承诺）。
package tray

// Host: 托盘菜单与宿主（daemon + 配置）的接线。全部由 cli 注入。
type Host struct {
	Version   string          // 展示用版本号
	Status    func() []string // 状态区行（如 "ch340 · 采集中"）；无设备返回含"无设备"的行
	PauseAll  func() error    // 暂停全部采集（写 PAUSED 清单并立即生效）
	ResumeAll func() error    // 恢复全部采集
	OpenLogs  func() error    // 在 Finder 中打开日志根目录
	Quit      func()          // 停止守护循环（菜单"退出"触发；须幂等）
}
