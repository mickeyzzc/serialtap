// Package testutil 提供跨包测试助手（假串口、等待与读文件）。
package testutil

import (
	"os"
	"sync"
	"testing"
	"time"
)

// FakePort: 假串口，满足 collector.Port。预置数据块按序吐出，
// 读尽后返回 0 字节（模拟读超时）。
type FakePort struct {
	Mu     sync.Mutex
	Chunks [][]byte
	Closed bool
	DTR    bool
	RTS    bool
	OnRead func()
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
func (e *ErrPort) Close() error                         { return nil }
func (e *ErrPort) SetDTR(bool) error                    { return nil }
func (e *ErrPort) SetRTS(bool) error                    { return nil }
func (e *ErrPort) SetReadTimeout(d time.Duration) error { return nil }
