//go:build windows

package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/tray"
	"github.com/mickeyzzc/serialtap/internal/web"
)

var freeConsole = syscall.NewLazyDLL("kernel32.dll").NewProc("FreeConsole")

// trayMutex: 单实例守卫的互斥体句柄——持有至进程退出（Close 即释放，故意
// 不关）。安装包快捷方式/双击会被反复触发，托盘此前无守卫：每次打开都
// 常驻一枚图标堆在托盘区，而守护进程并不随之而起（#83）。
var trayMutex windows.Handle

// trayAlreadyRunning: 命名互斥体探测已有托盘实例。首个调用者持有互斥体，
// 后续实例 CreateMutex 得 ERROR_ALREADY_EXISTS 即退。
func trayAlreadyRunning() bool {
	name, _ := windows.UTF16PtrFromString(`Local\serialtap-tray-singleton`)
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		return errors.Is(err, windows.ERROR_ALREADY_EXISTS)
	}
	trayMutex = h
	return false
}

// noArgsRun: 无参数入口（资源管理器双击 / portable 直开）——Windows 直接
// 进托盘并自动拉起守护。此前只打印用法即退，双击表现为"窗口一闪就没"
// （#83）；用法全文仍可用 -h/--help 查看。
func noArgsRun() (int, bool) {
	fmt.Println("serialtap：进入托盘模式（自动启动守护进程；全部命令见 serialtap --help）")
	if err := cmdTray(nil); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1, true
	}
	return 0, true
}

// cmdTray: Windows 托盘常驻 —— 接入状态、按设备暂停/恢复、打开日志。
// 与守护进程只经 ctl socket 通信；未运行时启动即自动拉起（见
// tray.RunResident），菜单"启动守护进程"保留手动入口。
func cmdTray(args []string) error {
	fs := flag.NewFlagSet("tray", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件 JSON")
	root := fs.String("root", "", "日志根目录")
	sock := fs.String("sock", "", "控制 socket 路径（默认自动）")
	webFlag := fs.String("web", "", "Web 面板地址（默认 127.0.0.1:8801；off = 不显示菜单项）")
	pollMs := fs.Int("poll-ms", 2000, "状态轮询间隔 ms")
	parseFlags(fs, args)
	cfg, err := loadCfgMerged(*cfgPath, *root, 0)
	if err != nil {
		return err
	}
	if *webFlag != "" {
		cfg.WebAddr = *webFlag
	}
	webURL := ""
	if addr := web.PickAddr(cfg.WebAddr); addr != "" {
		webURL = "http://" + addr + "/"
	}
	sockPath := *sock
	if sockPath == "" {
		sockPath = ctl.DefaultSocketPath()
	}
	if trayAlreadyRunning() {
		fmt.Println("serialtap 托盘已在运行（见右下角托盘图标），本次打开退出。")
		return nil
	}
	// 参数校验完毕，从控制台脱离 —— 双击/脚本启动不留黑窗（之后无 stderr 可写）
	freeConsole.Call()
	// 下界 50ms：0/负值会以全速打 ctl socket（panic 边界，issue #62）
	return tray.RunResident(sockPath, cfg.Root, webURL, time.Duration(clampMin(*pollMs, 50))*time.Millisecond)
}
