//go:build !windows

package cli

import "errors"

func cmdTray(args []string) error {
	return errors.New("托盘仅支持 Windows")
}

// noArgsRun: 无参数入口在非 Windows 平台维持原语义（打印用法）。
func noArgsRun() (int, bool) { return 0, false }
