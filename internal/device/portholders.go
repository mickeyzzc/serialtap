//go:build linux

package device

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PortHolders: 扫描 /proc/*/fd 找出当前打开了指定 tty 的其他进程 PID。
// 只读符号链接、绝不碰端口本身（探测动作不能给设备复位脉冲）。
// procRoot 注入（测试用假 /proc 树）。
func PortHoldersAt(procRoot, tty string) []int {
	tty = strings.TrimPrefix(tty, "/dev/") // fd 链接目标是 /dev/<tty>
	procs, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, p := range procs {
		if !p.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid == self {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, p.Name(), "fd"))
		if err != nil {
			continue // 权限或竞态，跳过
		}
		for _, fd := range fds {
			tgt, err := os.Readlink(filepath.Join(procRoot, p.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			if tgt == "/dev/"+tty || strings.HasSuffix(tgt, "/"+tty) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

func PortHolders(tty string) []int {
	return PortHoldersAt("/proc", tty)
}

// IdleDetectSupported: 本平台能否检测"端口被谁占用"（release 的空闲自动回采依赖）。
// Linux 走 /proc，macOS 走 lsof，均只读探测。
func IdleDetectSupported() bool { return true }
