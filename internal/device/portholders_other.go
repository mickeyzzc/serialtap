//go:build !linux

package device

// PortHolders: /proc 扫描仅 Linux 可用。
func PortHolders(tty string) []int { return nil }
