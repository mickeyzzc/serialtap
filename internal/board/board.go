package board

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mickeyzzc/serialtap/internal/flash"
)

// Action: 对主板的读侧操作类型。
const (
	ActionInfo       = "info"       // esptool flash_id：芯片/MAC/flash 容量
	ActionPartitions = "partitions" // 读 0x8000 分区表并解析
	ActionNVS        = "nvs"        // 定位 nvs 分区 → 读 → 解析（凭据默认掩码）
	ActionDump       = "dump"       // 任意区域原始 dump 到文件
)

// Spec: 一次 board 操作。所有操作经 esptool 在让口状态下执行 ——
// **会让目标板复位**（download 模式进出），与 flash 同一物理语义。
type Spec struct {
	Action      string `json:"action"`
	Esptool     string `json:"esptool,omitempty"`
	Chip        string `json:"chip,omitempty"`         // 省略=esptool 自动识别
	Baud        int    `json:"baud,omitempty"`         // 0=默认
	Addr        string `json:"addr,omitempty"`         // dump: 起始偏移（十六进制）
	Size        string `json:"size,omitempty"`         // dump: 长度（十六进制）
	OutPath     string `json:"out_path,omitempty"`     // dump: 输出文件
	ShowSecrets bool   `json:"show_secrets,omitempty"` // nvs: 明文显示凭据键
}

// Plan: 组装 esptool argv（不含本体；dry-run 审计共用）。
func Plan(tty string, s Spec) ([]string, error) {
	args := []string{"--port", tty}
	if s.Chip != "" {
		args = append(args, "--chip", s.Chip)
	}
	if s.Baud > 0 {
		args = append(args, "--baud", strconv.Itoa(s.Baud))
	}
	switch s.Action {
	case ActionInfo:
		return append(args, "flash_id"), nil
	case ActionPartitions:
		return append(args, "read_flash", hexOrDie(PartTableOffset), strconv.Itoa(PartTableSize), "<tmp>"), nil
	case ActionNVS, ActionDump:
		if s.Action == ActionDump && s.OutPath == "" {
			return nil, fmt.Errorf("dump 需要 --out 输出文件")
		}
		if _, err := parseHex(s.Addr); err != nil {
			return nil, fmt.Errorf("addr 非法: %w", err)
		}
		if _, err := parseHex(s.Size); err != nil {
			return nil, fmt.Errorf("size 非法: %w", err)
		}
		return append(args, "read_flash", s.Addr, s.Size, "<tmp>"), nil
	}
	return nil, fmt.Errorf("未知 board 操作: %q", s.Action)
}

// Exec: 让口编排由 daemon 负责（与 Flash 相同）；本函数执行单台设备的
// esptool 读操作并把结果渲染为文本行流。readFlash 回调由调用方注入
// （生产=esptool read_flash 到临时文件；测试=注入内存镜像）。
func Exec(tty string, s Spec, timeout time.Duration, out func(line string),
	readRegion func(addr, size uint32) ([]byte, error)) error {

	switch s.Action {
	case ActionInfo:
		esptool, err := flash.ResolveEsptool(s.Esptool)
		if err != nil {
			return err
		}
		argv, err := Plan(tty, s)
		if err != nil {
			return err
		}
		return flash.RunExec(esptool, argv, timeout, out)

	case ActionPartitions:
		data, err := readRegion(PartTableOffset, PartTableSize)
		if err != nil {
			return err
		}
		parts, err := ParsePartitions(data)
		if err != nil {
			out("⚠ " + err.Error())
		}
		for _, l := range FormatPartitions(parts) {
			out(l)
		}
		return nil

	case ActionNVS:
		table, err := readRegion(PartTableOffset, PartTableSize)
		if err != nil {
			return err
		}
		parts, err := ParsePartitions(table)
		if err != nil {
			return fmt.Errorf("分区表解析失败: %w", err)
		}
		nvs, ok := Find(parts, "nvs")
		if !ok {
			return fmt.Errorf("分区表中无 nvs 分区（共 %d 条）", len(parts))
		}
		data, err := readRegion(nvs.Offset, nvs.Size)
		if err != nil {
			return err
		}
		out(fmt.Sprintf("nvs 分区 @0x%08X %d B，凭据键%s掩码",
			nvs.Offset, nvs.Size, map[bool]string{true: "不", false: "默认"}[s.ShowSecrets]))
		for _, l := range FormatNVS(ParseNVS(data, s.ShowSecrets)) {
			out(l)
		}
		return nil

	case ActionDump:
		addr, _ := parseHex(s.Addr)
		size, _ := parseHex(s.Size)
		data, err := readRegion(uint32(addr), uint32(size))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(mustAbs(s.OutPath)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(s.OutPath, data, 0o600); //nolint:gosec // dump 产物可能含明文凭据，限属主
		err != nil {
			return err
		}
		out(fmt.Sprintf("已导出 0x%X..0x%X（%d B）→ %s（0600；含明文 NVS 数据时勿提交）",
			addr, addr+size, len(data), s.OutPath))
		return nil
	}
	return fmt.Errorf("未知 board 操作: %q", s.Action)
}

// EsptoolReader: 生产环境的 readRegion —— esptool read_flash 到临时文件再读回。
func EsptoolReader(tty string, s Spec, timeout time.Duration, out func(line string)) func(addr, size uint32) ([]byte, error) {
	return func(addr, size uint32) ([]byte, error) {
		esptool, err := flash.ResolveEsptool(s.Esptool)
		if err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp("", "serialtap-read-*.bin")
		if err != nil {
			return nil, err
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()
		defer func() { _ = os.Remove(tmpPath) }()
		argv := []string{"--port", tty}
		if s.Chip != "" {
			argv = append(argv, "--chip", s.Chip)
		}
		if s.Baud > 0 {
			argv = append(argv, "--baud", strconv.Itoa(s.Baud))
		}
		argv = append(argv, "read_flash",
			fmt.Sprintf("0x%X", addr), strconv.FormatUint(uint64(size), 10), tmpPath)
		if err := flash.RunExec(esptool, argv, timeout, out); err != nil {
			return nil, err
		}
		return os.ReadFile(tmpPath)
	}
}

func parseHex(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "0x")))
	if s == "" {
		return 0, fmt.Errorf("空值")
	}
	return strconv.ParseUint(s, 16, 64)
}

func hexOrDie(v uint32) string { return fmt.Sprintf("0x%X", v) }

func mustAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
