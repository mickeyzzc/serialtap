//go:build windows

package cli

import "golang.org/x/sys/windows"

// setupConsole: Windows 控制台默认代码页（中文系统 936/GBK）渲染不了
// UTF-8 输出，中文帮助文本/告警符号会花屏（issue #62）。CLI 入口一次性
// 把输出代码页切到 UTF-8（65001）；失败无害——输出重定向/无控制台时
// 本就不经代码页。
func setupConsole() {
	_ = windows.SetConsoleOutputCP(65001)
}
