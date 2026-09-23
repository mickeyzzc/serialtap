//go:build !windows

package ctl

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultSocketPath: XDG_RUNTIME_DIR/serialtap.sock，回退 /tmp/serialtap-$UID.sock
// （Linux 桌面有 XDG；macOS 无 XDG 但 /tmp 即 /private/tmp，重启自动清理，适合放 socket）。
func DefaultSocketPath() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "serialtap.sock")
	}
	return fmt.Sprintf("/tmp/serialtap-%d.sock", os.Getuid())
}
