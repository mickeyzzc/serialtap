package config

// Package config 定义 serialtap 的运行配置与加载。
import (
	"encoding/json"
	"os"
	"path/filepath"
)

// NameRule: by-id 正则 → 设备目录名。配置里的规则优先于内置默认规则。
type NameRule struct {
	Match string `json:"match"`
	Name  string `json:"name"`
}

type Config struct {
	Root          string            `json:"root"`             // 日志根目录
	Baud          int               `json:"baud"`             // 串口波特率（USB-JTAG 板无意义但无害）
	PollMs        int               `json:"poll_interval_ms"` // 热插拔轮询间隔
	SilentReopenS int               `json:"silent_reopen_s"`  // 静默强制重开阈值（秒，0=关。只对保证有周期日志的设备开，见 collector.go）
	ReopenMinS    int               `json:"reopen_min_s"`     // 断线重开退避下限（秒）
	ReopenMaxS    int               `json:"reopen_max_s"`     // 断线重开退避上限（秒）
	RotateMB      int               `json:"rotate_max_mb"`    // 单文件大小轮转阈值
	RetentionDays int               `json:"retention_days"`   // 日志保留天数（<=0 永久）
	Exclude       []string          `json:"exclude"`          // 忽略设备的正则（匹配 tty/by-id/by-path/名字任一）
	Names         []NameRule        `json:"names"`            // 设备命名规则
	ExtraSigs     []string          `json:"signatures_extra"` // 追加事件签名正则
	ElfMap        map[string]string `json:"elf_map"`          // 设备名 → 固件 .elf（decode-backtrace 自动解码用）
	ControlSocket string            `json:"control_socket"`   // 控制 unix socket（空 = 默认路径）
	Esptool       string            `json:"esptool_cmd"`      // 代理刷固件的 esptool 命令（空 = PATH/espressif 环境自动发现）
	FlashBaud     int               `json:"flash_baud"`       // 代理刷波特率（0 = esptool 默认）
	FlashTimeoutS int               `json:"flash_timeout_s"`  // 单台设备刷写超时（秒，超时杀 esptool 并回采；显式写 0 = 不限时）
}

func DefaultConfig() Config {
	return Config{
		Root:          "logs",
		Baud:          115200,
		PollMs:        1000,
		SilentReopenS: 0, // 静默看门狗默认关：见 config 字段注释
		ReopenMinS:    5,
		ReopenMaxS:    60,
		RotateMB:      64,
		RetentionDays: 14,
		FlashTimeoutS: 600, // 10 分钟兜底；注意：LoadConfig 不回填此字段，显式 0 = 不限时
	}
}

// LoadConfig: 先填默认值，再用 JSON 覆盖，零值字段回填默认。
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	def := DefaultConfig()
	if cfg.Baud == 0 {
		cfg.Baud = def.Baud
	}
	if cfg.PollMs == 0 {
		cfg.PollMs = def.PollMs
	}
	if cfg.ReopenMinS == 0 {
		cfg.ReopenMinS = def.ReopenMinS
	}
	if cfg.ReopenMaxS == 0 {
		cfg.ReopenMaxS = def.ReopenMaxS
	}
	if cfg.RotateMB == 0 {
		cfg.RotateMB = def.RotateMB
	}
	if cfg.Root == "" {
		cfg.Root = def.Root
	}
	return cfg, nil
}

// DefaultConfigPath: 默认配置文件位置（存在才加载）。
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "serialtap", "config.json")
}
