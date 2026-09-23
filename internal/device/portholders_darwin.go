//go:build darwin

package device

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// PortHolders: 用 lsof 找出当前打开了指定 tty 的其他进程 PID
// （macOS 无 /proc）。只查询、绝不碰端口本身（探测动作不能给设备复位脉冲）。
// lsof 无占用时退出码非 0，与"lsof 不可用"同样返回 nil。
func PortHolders(tty string) []int {
	out, err := exec.Command("lsof", "-t", "--", tty).Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(f); err == nil && p != self {
			pids = append(pids, p)
		}
	}
	return pids
}

// IdleDetectSupported: lsof 随系统自带（见上）。
func IdleDetectSupported() bool { return true }
