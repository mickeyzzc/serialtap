package daemon

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/mickeyzzc/serialtap/internal/collector"
)

// —— 串口层软重连（reopen）与 USB 层软重枚举（reset）——
//
// 分层语义：
//   reopen  只动串口句柄：立即关口 → 跳过退避重开。所有权与 PAUSED 语义
//           不变（与 release 不同），适合端口疑似卡死时的快速自愈。
//   reset   动 USB 设备节点：让口 → pnputil /restart-device（禁用+启用，
//           等效软件层面的拔插）→ 用自身枚举器确认重枚举 → 回采。作用于
//           设备的串口接口节点（Windows 上 by-id 即实例路径），JTAG 等
//           兄弟接口不受影响。实测（2026-09-22, Win11 zh-CN）：
//           非提权进程调 pnputil 返回"拒绝访问"且退出码仍为 0 —— 成败
//           判定不能信 exit code，只能认输出标记 + 枚举复核；非提权守护
//           进程自动走 UAC 提权重试（Start-Process -Verb RunAs）。

// gateMulti: 动端口的操作（flash/reopen/reset）共用门禁 —— 匹配多台且未
// 显式确认时拒绝并列出设备名（多板同芯片时未锚定正则会误伤在测板）。
// op 为操作名（如 "刷写"），拼进提示语。
func gateMulti(pattern string, all bool, cs []*collector.Collector, op string) error {
	if len(cs) > 1 && !all {
		names := make([]string, len(cs))
		for i, c := range cs {
			names[i] = c.DeviceName()
		}
		return fmt.Errorf("模式 %q 匹配 %d 台设备（%s）—— 批量%s需显式确认（CLI --all / ctl 请求 all:true）；精确操作一台请锚定（如 ^%s$）",
			pattern, len(cs), strings.Join(names, ", "), op, cs[0].DeviceName())
	}
	return nil
}

// Reopen: 串口层软断开重连 —— 匹配设备的采集器立即关口并跳过退避重开。
// 不释放所有权、不动 PAUSED（与 release 不同）。会打断进行中的透传会话
// （客户端按既有语义重连）。与 Flash/Release/Reset 互斥。
func (d *daemon) Reopen(pattern string, all bool) (int, error) {
	_, cs := d.matches(pattern)
	if len(cs) == 0 {
		return 0, fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	if err := gateMulti(pattern, all, cs, "重开"); err != nil {
		return 0, err
	}
	if !d.opMu.TryLock() {
		return 0, fmt.Errorf("另一个 flash/release 操作进行中，请稍后再试")
	}
	defer d.opMu.Unlock()
	n := 0
	for _, c := range cs {
		c.LogEvent("port cycle requested (manual reopen)")
		c.Reopen()
		n++
	}
	return n, nil
}

// resetReenumWait: pnputil 重启后等待设备节点回来的窗口。
const resetReenumWait = 10 * time.Second

// Reset: USB 层软拔插 —— 让口 → pnputil 重启设备节点 → 枚举确认 → 回采。
// 设备节点整段消失重建时 watcher 会重建采集器；原地重启时 Resume 让原
// 采集器直接重开 —— 两条路径都收敛到"设备在、采集恢复"。pnputil 需要
// 管理员权限：非提权守护进程自动弹 UAC 提权重试（用户可取消）。
func (d *daemon) Reset(pattern string, all bool) error {
	keys, cs := d.matches(pattern)
	if len(cs) == 0 {
		return fmt.Errorf("没有匹配 %q 的采集设备", pattern)
	}
	if err := gateMulti(pattern, all, cs, "重置"); err != nil {
		return err
	}
	if !d.opMu.TryLock() {
		return fmt.Errorf("另一个 flash/release 操作进行中，请稍后再试")
	}
	defer d.opMu.Unlock()

	for i, c := range cs {
		key := keys[i]
		if c.ByID() == "" {
			return fmt.Errorf("%s: 无 USB 实例路径（by-id 为空，非 USB 枚举设备？），无法 USB reset", c.DeviceName())
		}
		// 撤销 pending release，防止限时到期在重启中途抢口（与 Flash 同口径）
		d.mu.Lock()
		delete(d.releases, key)
		d.mu.Unlock()
		c.LogEvent("usb reset start (instance=%s)", c.ByID())
		if !c.Suspend(10 * time.Second) {
			c.LogEvent("usb reset abort: port did not release")
			return fmt.Errorf("%s: 端口让出超时", c.DeviceName())
		}
		out, err := usbRestartDevice(c.ByID())
		if out != "" {
			c.LogEvent("usb reset pnputil: %s", oneLine(out))
		}
		if err == nil {
			if !d.waitKeyBack(key, resetReenumWait) {
				c.Resume()
				c.LogEvent("usb reset: device did not re-enumerate within %s", resetReenumWait)
				return fmt.Errorf("%s: 重枚举后设备未在 %s 内回来（检查 USB 连接/供电，或人工重插）",
					c.DeviceName(), resetReenumWait)
			}
			// 设备若曾整段消失，watcher 下一拍重建采集器并自动开采；
			// 本采集器若仍存活（原地重启），Resume 让它立即重开。幂等。
		}
		c.Resume()
		c.LogEvent("usb reset finished (err=%v)", err)
		if err != nil {
			return fmt.Errorf("%s: %w", c.DeviceName(), err)
		}
	}
	return nil
}

// waitKeyBack: 轮询枚举器等设备 key 回来（pnputil 重启后节点可能整段
// 消失重建；原地重启则恒在，立即返回真）。
func (d *daemon) waitKeyBack(key string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		infos, err := d.enum()
		if err == nil {
			for _, inf := range infos {
				if inf.Key == key {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// oneLine: 输出压成单行（事件流可读性）。
func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// —— pnputil 执行体（usbRestartDevice 为测试 seam）——

var usbRestartDevice = defaultUSBRestartDevice

const (
	pnputilTimeout = 60 * time.Second  // 直连调用超时
	pnputilUACTime = 120 * time.Second // 提权路径超时（含用户响应 UAC 的时间）
)

// defaultUSBRestartDevice: 重启 USB 设备节点。先直连调 pnputil（守护进程
// 已提权时直接成功）；输出含"拒绝访问"则自动 UAC 提权重试。
func defaultUSBRestartDevice(instanceID string) (string, error) {
	if runtime.GOOS != "windows" {
		return "", fmt.Errorf("USB reset 仅实现 Windows（pnputil）；%s 平台未实现", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pnputilTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "pnputil", "/restart-device", instanceID).CombinedOutput()
	s := string(out)
	if accessDenied(s) {
		return runPnputilElevated(instanceID)
	}
	if pnputilFailed(s) {
		return s, fmt.Errorf("pnputil 重启设备失败: %s", oneLine(s))
	}
	return s, nil
}

// runPnputilElevated: UAC 提权执行 pnputil。内层 PowerShell 以 UTF-8 把
// 输出写临时文件（提权进程无法直接管道回非提权父进程），经 -EncodedCommand
// 传递脚本避免三层引号转义；外层 Start-Process -Verb RunAs -Wait 等待完成。
// 用户在 UAC 对话框点"否"时 Start-Process 报错，原样上抛。
func runPnputilElevated(instanceID string) (string, error) {
	tmp, err := os.CreateTemp("", "serialtap-reset-*.txt")
	if err != nil {
		return "", fmt.Errorf("USB reset 提权失败（临时文件）: %w", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpName) }()

	inner := fmt.Sprintf("pnputil /restart-device '%s' 2>&1 | Out-File -Encoding utf8 '%s'",
		instanceID, tmpName)
	outer := fmt.Sprintf("$p = Start-Process powershell -Verb RunAs -WindowStyle Hidden -PassThru -Wait "+
		"-ArgumentList @('-NoProfile','-EncodedCommand','%s'); exit $p.ExitCode", psEncode(inner))

	ctx, cancel := context.WithTimeout(context.Background(), pnputilUACTime)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", outer)
	psOut, err := cmd.CombinedOutput()
	pnputilOut, _ := os.ReadFile(tmpName)
	if err != nil {
		return string(pnputilOut), fmt.Errorf("USB reset 提权执行失败（UAC 被取消或超时？）: %v: %s",
			err, oneLine(string(psOut)))
	}
	s := string(pnputilOut)
	if pnputilFailed(s) {
		return s, fmt.Errorf("pnputil（提权）重启设备失败: %s", oneLine(s))
	}
	return s, nil
}

// psEncode: PowerShell -EncodedCommand 的 Base64(UTF-16LE)。
func psEncode(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// accessDenied: pnputil 输出是否为权限拒绝（pnputil 失败时退出码不可靠，
// 只能认输出标记；zh/en 两种）。
func accessDenied(out string) bool {
	return strings.Contains(out, "拒绝访问") || strings.Contains(out, "Access is denied")
}

// pnputilFailed: pnputil 输出是否报告失败。成功文案本地化不可枚举，
// 采用失败标记清单 + 默认成功；reset 层另有枚举复核兜底。
func pnputilFailed(out string) bool {
	for _, kw := range []string{
		"无法重启", "无法重新启动", "拒绝访问", "找不到", "发生错误",
		"failed to restart", "Access is denied", "not found", "error",
	} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
