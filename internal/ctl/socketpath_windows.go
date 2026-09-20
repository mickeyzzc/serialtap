//go:build windows

package ctl

import (
	"os"
	"path/filepath"
)

// DefaultSocketPath: %LOCALAPPDATA%\serialtap\serialtap.sock，回退 %TEMP%。
// Go 的 unix socket 需要 Windows 10 1803+（AF_UNIX）。
func DefaultSocketPath() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "serialtap", "serialtap.sock")
	}
	return filepath.Join(os.TempDir(), "serialtap.sock")
}
