//go:build !linux && !darwin

package device

// PortHolders: Windows 无 /proc，也无不开端口即可查占用者的纯 Go 途径 ——
// 试开端口会给 CDC 设备一拍复位脉冲（本项目核心纪律禁止），故恒返回 nil。
// release 的空闲自动回采在 IdleDetectSupported()=false 的平台被拒绝（见 daemon.Release）。
func PortHolders(tty string) []int { return nil }

// IdleDetectSupported: 见上。
func IdleDetectSupported() bool { return false }
