//go:build !windows

package cli

import "errors"

func cmdTray(args []string) error {
	return errors.New("托盘仅支持 Windows")
}
