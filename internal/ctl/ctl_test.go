package ctl

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// sockPath: 测试 socket 路径。macOS 的 sun_path 上限 104 字节，而 t.TempDir()
// 在 darwin 上位于很长的 /var/folders/… 下 —— 长测试名直接把 bind 顶爆
// （报 invalid argument）。darwin 统一改用 /tmp 短路径。
func sockPath(t *testing.T, name string) string {
	if runtime.GOOS == "darwin" {
		p := filepath.Join("/tmp", fmt.Sprintf("serialtap-test-%d-%s.sock", os.Getpid(), name))
		t.Cleanup(func() { os.Remove(p) })
		return p
	}
	return filepath.Join(t.TempDir(), name+".sock")
}

func TestServerClientRoundTrip(t *testing.T) {
	sock := sockPath(t, "roundtrip")
	srv, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(func(req Request, respond func(Response)) {
		switch req.Cmd {
		case "status":
			respond(Response{OK: true, Devices: []DevState{{Name: "a", Tty: "/dev/x", Key: "k", State: "collecting"}}})
		case "flash":
			respond(Response{OK: true, Event: "flash-log", Line: "flashing 50%"})
			respond(Response{OK: true, Event: "flash-done", Code: 0})
		default:
			respond(Response{OK: false, Error: "unknown cmd"})
		}
	})
	defer srv.Close()

	// 单响应
	var got DevState
	err = Send(sock, Request{Cmd: "status"}, func(r Response) bool {
		if r.OK && len(r.Devices) == 1 {
			got = r.Devices[0]
			return true
		}
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" || got.State != "collecting" {
		t.Fatalf("status 数据错误: %+v", got)
	}

	// flash 流式：两行后 flash-done 结束
	var lines []string
	err = Send(sock, Request{Cmd: "flash"}, func(r Response) bool {
		if r.Event == "flash-log" {
			lines = append(lines, r.Line)
		}
		return r.Event == "flash-done"
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "flashing 50%" {
		t.Fatalf("flash 流错误: %v", lines)
	}

	// 未知命令
	if err := Send(sock, Request{Cmd: "zzz"}, func(r Response) bool { return true }); err != nil {
		t.Fatal(err)
	}

	// 守护不在 → 连接错误
	if err := Send(sockPath(t, "nope"), Request{Cmd: "status"}, nil); err == nil {
		t.Fatal("缺 socket 应报错")
	}
}

func TestSocketFileCleanedUp(t *testing.T) {
	sock := sockPath(t, "cleaned")
	srv, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(func(req Request, respond func(Response)) { respond(Response{OK: true}) })
	time.Sleep(50 * time.Millisecond)
	srv.Close()
	if _, err := osStat(sock); !osIsNotExist(err) {
		t.Fatal("Close 后 socket 文件应删除")
	}
	// 残留 socket 可被 Listen 清理
	srv2, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv2.Close()
}

// 小包装避免直接引 os（测试文件顶部已够干净）
func osStat(p string) (any, error) {
	return os.Stat(p)
}

func osIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

// —— 双实例抢 socket：活着的主人拒绝后来者；死 socket（残留文件）可接管 ——
func TestListenRefusesWhenSocketOwned(t *testing.T) {
	sock := sockPath(t, "owned")
	first, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go first.Serve(func(req Request, respond func(Response)) { respond(Response{OK: true}) })

	if _, err := Listen(sock); err == nil {
		t.Fatal("第二实例抢活 socket 应被拒绝")
	}

	// 主人退出 → 残留 socket 文件 → 后来者可接管
	first.Close()
	second, err := Listen(sock)
	if err != nil {
		t.Fatalf("接管残留 socket 失败: %v", err)
	}
	second.Close()
}
