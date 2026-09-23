//go:build windows

package cli

import (
	"flag"
	"syscall"
	"time"

	"github.com/mickeyzzc/serialtap/internal/ctl"
	"github.com/mickeyzzc/serialtap/internal/tray"
	"github.com/mickeyzzc/serialtap/internal/web"
)

var freeConsole = syscall.NewLazyDLL("kernel32.dll").NewProc("FreeConsole")

// cmdTray: Windows 托盘常驻 —— 接入状态、按设备暂停/恢复、打开日志。
// 与守护进程只经 ctl socket 通信（未运行时可在菜单里一键启动）。
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
	// 参数校验完毕，从控制台脱离 —— 双击/脚本启动不留黑窗（之后无 stderr 可写）
	freeConsole.Call()
	return tray.RunResident(sockPath, cfg.Root, webURL, time.Duration(*pollMs)*time.Millisecond)
}
