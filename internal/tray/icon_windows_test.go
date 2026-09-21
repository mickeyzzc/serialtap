//go:build windows

package tray

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 生成的 ICO 字节必须是 LoadImage 可加载的合法图标
// （曾因 ICONDIR.type 未置 1 被 LoadImageW 静默拒绝）。
func TestIconBytesValid(t *testing.T) {
	loadImage := windows.NewLazySystemDLL("user32.dll").NewProc("LoadImageW")
	destroyIcon := windows.NewLazySystemDLL("user32.dll").NewProc("DestroyIcon")
	for i, ic := range [][]byte{buildIcon(false), buildIcon(true)} {
		p := filepath.Join(t.TempDir(), "icon.ico")
		if err := os.WriteFile(p, ic, 0o644); err != nil {
			t.Fatal(err)
		}
		name, _ := windows.UTF16PtrFromString(p)
		h, _, _ := loadImage.Call(0, uintptr(unsafe.Pointer(name)),
			1 /*IMAGE_ICON*/, 32, 32, 0x10 /*LR_LOADFROMFILE*/)
		if h == 0 {
			t.Fatalf("buildIcon(%d) 生成的 ICO 无法被 LoadImage 加载", i)
		}
		destroyIcon.Call(h)
	}
}
