package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Baud != 115200 || cfg.Root != "logs" || cfg.PollMs != 1000 {
		t.Fatalf("默认值错误: %+v", cfg)
	}
	if cfg.SilentReopenS != 0 {
		t.Fatal("静默看门狗默认必须为 0（关）——安静设备会被复位循环打死")
	}
	if cfg.RotateMB != 64 || cfg.RetentionDays != 14 {
		t.Fatalf("轮转/保留默认值错误: %+v", cfg)
	}
}

func TestLoadJSONOverridesAndZeroBackfill(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{"root":"/x","baud":9600,"rotate_max_mb":8,"silent_reopen_s":120}`), 0o644)
	cfg, err := LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != "/x" || cfg.Baud != 9600 || cfg.RotateMB != 8 || cfg.SilentReopenS != 120 {
		t.Fatalf("JSON 覆盖失败: %+v", cfg)
	}
	// 零值字段回填默认
	if cfg.PollMs != 1000 || cfg.ReopenMinS != 5 || cfg.RetentionDays != 14 {
		t.Fatalf("零值回填失败: %+v", cfg)
	}
}

func TestLoadMissingPathUsesDefaults(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil || cfg.Root != "logs" {
		t.Fatalf("空路径应纯默认: %+v err=%v", cfg, err)
	}
	if _, err := LoadConfig("/nonexistent/c.json"); err == nil {
		t.Fatal("缺文件应报错")
	}
}

func TestDefaultConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if p := DefaultConfigPath(); !strings.HasPrefix(p, home) || !strings.HasSuffix(p, "config.json") {
		t.Fatalf("默认路径异常: %q", p)
	}
}
