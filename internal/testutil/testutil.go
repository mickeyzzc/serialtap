// Package testutil 提供跨包测试助手（假串口、假外部命令、等待与读文件）。
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// FakePort: 假串口，满足 collector.Port。预置数据块按序吐出，
// 读尽后返回 0 字节（模拟读超时）。
type FakePort struct {
	Mu      sync.Mutex
	Chunks  [][]byte
	Written [][]byte // Write 收到的字节块（透传桥测试断言用）
	Closed  bool
	DTR     bool
	RTS     bool
	OnRead  func()
}

func (f *FakePort) Read(p []byte) (int, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Closed {
		return 0, os.ErrClosed
	}
	if len(f.Chunks) > 0 {
		c := f.Chunks[0]
		f.Chunks = f.Chunks[1:]
		n := copy(p, c)
		if f.OnRead != nil {
			f.OnRead()
		}
		return n, nil
	}
	if f.OnRead != nil {
		f.OnRead()
	}
	return 0, nil // 模拟读超时（0 字节无错误）
}

func (f *FakePort) Write(p []byte) (int, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Closed {
		return 0, os.ErrClosed
	}
	b := make([]byte, len(p))
	copy(b, p)
	f.Written = append(f.Written, b)
	return len(p), nil
}

func (f *FakePort) Close() error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Closed = true
	return nil
}

func (f *FakePort) SetDTR(v bool) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.DTR = v
	return nil
}

func (f *FakePort) SetRTS(v bool) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.RTS = v
	return nil
}

func (f *FakePort) SetReadTimeout(d time.Duration) error { return nil }

// WaitFor: 轮询等待条件成立，超时 fatal。
func WaitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// ReadFile: 读文件为字符串（不存在返回空串）。
func ReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// ErrPort: 读即报错的假端口（USB 重枚举场景）。
type ErrPort struct{}

func (e *ErrPort) Read([]byte) (int, error)             { return 0, os.ErrClosed }
func (e *ErrPort) Write(p []byte) (int, error)          { return 0, os.ErrClosed }
func (e *ErrPort) Close() error                         { return nil }
func (e *ErrPort) SetDTR(bool) error                    { return nil }
func (e *ErrPort) SetRTS(bool) error                    { return nil }
func (e *ErrPort) SetReadTimeout(d time.Duration) error { return nil }

// —— 假外部命令（esptool / addr2line 替身）——
// shell 脚本假件在 Windows 不可执行，统一编译成真二进制；进程内只编译一次。
// 行为由环境变量驱动（exec.Command 自动继承）：FAKE_EXIT=退出码；
// FAKE_OUT=要打印的内容（字面输出，可含 \r/\n）；FAKE_SLEEP=先睡的时长（如 2s）。

var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
)

// FakeTool: 返回假外部命令的可执行文件路径。测试里用 t.Setenv 配 FAKE_EXIT/FAKE_OUT。
func FakeTool(tb testing.TB) string {
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "serialtap-fake-")
		if err != nil {
			fakeErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(fakeToolSrc), 0o644); err != nil {
			fakeErr = err
			return
		}
		exe := filepath.Join(dir, "fakecmd")
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		out, err := exec.Command("go", "build", "-o", exe, src).CombinedOutput()
		if err != nil {
			fakeErr = fmt.Errorf("go build 假命令失败: %w: %s", err, out)
			return
		}
		fakeBin = exe
	})
	if fakeErr != nil {
		tb.Fatalf("FakeTool 不可用: %v", fakeErr)
	}
	return fakeBin
}

const fakeToolSrc = "package main\n" +
	"import (\n" +
	"\t\"fmt\"\n" +
	"\t\"os\"\n" +
	"\t\"strings\"\n" +
	"\t\"time\"\n" +
	")\n" +
	"func main() {\n" +
	"\tif v := os.Getenv(\"FAKE_SLEEP\"); v != \"\" {\n" +
	"\t\tif d, err := time.ParseDuration(v); err == nil {\n" +
	"\t\t\ttime.Sleep(d)\n" +
	"\t\t}\n" +
	"\t}\n" +
	"\tcode := 0\n" +
	"\tif v := os.Getenv(\"FAKE_EXIT\"); v != \"\" {\n" +
	"\t\tif _, err := fmt.Sscanf(v, \"%d\", &code); err != nil {\n" +
	"\t\t\tcode = 1\n" +
	"\t\t}\n" +
	"\t}\n" +
	"\tif out := os.Getenv(\"FAKE_OUT\"); out != \"\" {\n" +
	"\t\tfmt.Print(out)\n" +
	"\t\tif !strings.HasSuffix(out, \"\\n\") {\n" +
	"\t\t\tfmt.Println()\n" +
	"\t\t}\n" +
	"\t}\n" +
	"\tos.Exit(code)\n" +
	"}\n"
