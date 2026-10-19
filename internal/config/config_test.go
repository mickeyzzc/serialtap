package config

import (
	"encoding/json"
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
	t.Setenv("USERPROFILE", home) // Windows 的 os.UserHomeDir 走 USERPROFILE
	if p := DefaultConfigPath(); !strings.HasPrefix(p, home) || !strings.HasSuffix(p, "config.json") {
		t.Fatalf("默认路径异常: %q", p)
	}
}

// mesh_access：节点接入权限模式（"" / "rw" = 可写默认，"ro" = 只读）。
func TestMeshAccessParse(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MeshAccess != "" {
		t.Fatalf("默认应为空（=rw）: %q", cfg.MeshAccess)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{"mesh_enabled":true,"mesh_access":"ro"}`), 0o644)
	cfg, err := LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MeshAccess != "ro" {
		t.Fatalf("ro 解析失败: %q", cfg.MeshAccess)
	}
}

// —— issue #68：示例配置必须是可直接复制使用的严格 JSON ——
// 曾带 // 注释行，json.Unmarshal 一读就炸，用户抄一份即坏配置。
func TestExampleConfigParses(t *testing.T) {
	example := filepath.Join("..", "..", "config.example.json")
	data, err := os.ReadFile(example)
	if err != nil {
		t.Fatalf("读示例配置失败: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("示例配置不是合法 JSON（注释/尾逗号？）: %v", err)
	}
	cfg, err := LoadConfig(example)
	if err != nil {
		t.Fatalf("示例配置加载失败: %v", err)
	}
	// 补齐的 6 个字段（#68）取值抽查
	if cfg.FlashBaud != 921600 || cfg.FlashTimeoutS != 600 || cfg.WebAddr != "127.0.0.1:8801" {
		t.Fatalf("示例补齐字段取值异常: %+v", cfg)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("示例配置不应有未知键告警: %v", cfg.Warnings)
	}
}

// —— issue #61：未知顶层键必须告警（不阻断加载）——
func TestUnknownKeyWarns(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "typo.json")
	os.WriteFile(f, []byte(`{"root":"/x","mesh_keyy":"typo-here"}`), 0o644)
	cfg, err := LoadConfig(f)
	if err != nil {
		t.Fatalf("未知键不应导致加载失败（前向兼容）: %v", err)
	}
	if cfg.Root != "/x" {
		t.Fatalf("合法键仍应生效: %+v", cfg)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "mesh_keyy") {
		t.Fatalf("告警应点名未知键 mesh_keyy: %v", cfg.Warnings)
	}
	if strings.Contains(joined, `"root"`) {
		t.Fatalf("合法键不应被告警: %v", cfg.Warnings)
	}
}

// 全量合法键（含 json:"-" 的 Warnings 不算）零告警；反射清单天然与结构同步。
func TestAllKnownKeysNoWarnings(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "all.json")
	os.WriteFile(f, []byte(`{
		"root":"/x","baud":1,"poll_interval_ms":1,"silent_reopen_s":1,"reopen_min_s":1,
		"reopen_max_s":1,"rotate_max_mb":1,"retention_days":1,"exclude":[],"names":[],
		"signatures_extra":[],"elf_map":{},"control_socket":"","esptool_cmd":"",
		"flash_baud":1,"flash_timeout_s":1,"proxy_tap_exclude":"","dtr_hold":[],
		"web_addr":"127.0.0.1:1","mesh_enabled":false,"mesh_name":"n","mesh_port":1,
		"mesh_key":"k","mesh_access":"rw","mesh_announce_s":1,"mesh_peers":[],"mesh_auto_approve":false
	}`), 0o644)
	cfg, err := LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("全量合法键不应告警: %v", cfg.Warnings)
	}
	if known := knownKeys(); !known["root"] || !known["mesh_auto_approve"] {
		t.Fatalf("knownKeys 反射缺字段: %v", known)
	}
}
