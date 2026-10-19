//go:build !windows

package cli

// setupConsole: 非 Windows 无 OEM 代码页概念，空实现（与 console_windows.go
// 成对，Run 入口无差别调用）。
func setupConsole() {}
