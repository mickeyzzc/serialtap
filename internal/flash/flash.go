// Package flash 编排代理刷固件：构建 esptool 命令、流式执行、
// 解析 ESP-IDF 的 flasher_args.json。
package flash

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BinSpec: 一个待刷镜像（路径 + 偏移，偏移为十六进制字符串如 "0x10000"）。
type BinSpec struct {
	Path   string `json:"path"`
	Offset string `json:"offset"`
}

// Spec: 一次刷写。Bins 与 ArgsFile 二选一（同时给以 ArgsFile 为准）。
type Spec struct {
	Baud     int       `json:"baud,omitempty"`      // 0 = 用 esptool/args 默认
	Esptool  string    `json:"esptool,omitempty"`   // 显式 esptool 命令
	Chip     string    `json:"chip,omitempty"`      // 如 esp32s3（省略=esptool 自动识别）
	Bins     []BinSpec `json:"bins,omitempty"`      // 手工指定 bin@offset
	ArgsFile string    `json:"args_file,omitempty"` // IDF build/flasher_args.json
	DryRun   bool      `json:"dry_run,omitempty"`   // 只预演：解析并回显 esptool 命令，不动端口
}

// flasherArgs: ESP-IDF build/flasher_args.json 的相关子集。
type flasherArgs struct {
	// extra_esptool_args 是异构值表（"--chip":"esp32s3"、"stub":true、"trace":0），
	// 只取 string 值（chip 等），布尔/数字忽略（stub/trace 走默认即可）
	ExtraEsptoolArgs map[string]any    `json:"extra_esptool_args"`
	FlashFiles       map[string]string `json:"flash_files"`
}

// ResolveEsptool: 显式指定 > PATH 里的 esptool / esptool.py >
// ESP-IDF 工具环境（~/.espressif/python_env，未 source 环境时 PATH 里没有）>
// 平台特有目录（Windows 的 pip --user）。多个命中取排序后最后一个
// （与 analyze.findAddr2line 的策略一致）。
func ResolveEsptool(explicit string) (string, error) {
	if explicit != "" {
		if p, err := exec.LookPath(explicit); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("esptool 命令不可用: %s", explicit)
	}
	for _, c := range []string{"esptool", "esptool.py"} {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	root := os.Getenv("IDF_TOOLS_PATH") // ESP-IDF 允许重定位工具根
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".espressif")
		}
	}
	roots := []string{root}
	roots = append(roots, eimRoots()...) // eim 安装管理器可把工具根搬到别处（实测 C:\Espressif）
	var allGlobs []string
	for _, r := range roots {
		if r != "" {
			allGlobs = append(allGlobs, esptoolEnvGlobs(r)...)
		}
	}
	if p, ok := globLast(allGlobs); ok {
		return p, nil
	}
	if p, ok := globLast(esptoolUserGlobs()); ok {
		return p, nil
	}
	return "", fmt.Errorf("找不到 esptool（PATH、espressif python_env、pip --user 目录均无；" +
		"请 source ESP-IDF 环境或 --esptool 指定）")
}

// eimRoots: ESP-IDF 安装管理器（eim）的 eim_config.toml 会把工具根搬离
// ~/.espressif（如 tool_install_folder_name = 'C:\Espressif\tools'，python_env
// 在 tools 同级）。轻量扫描该单行（单引号 TOML 字符串），失败即跳过 —— 不引 TOML 依赖。
// espressifDir 为 .espressif 目录（生产取 ~/.espressif，测试可注入）。
func eimRootsAt(espressifDir string) []string {
	data, err := os.ReadFile(filepath.Join(espressifDir, "eim_config.toml"))
	if err != nil {
		return nil
	}
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "tool_install_folder_name") {
			continue
		}
		q1 := strings.IndexByte(ln, '\'')
		q2 := strings.LastIndexByte(ln, '\'')
		if q1 < 0 || q2 <= q1 {
			return nil
		}
		if dir := strings.TrimSpace(ln[q1+1 : q2]); dir != "" {
			return []string{filepath.Dir(dir), dir}
		}
		return nil
	}
	return nil
}

func eimRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return eimRootsAt(filepath.Join(home, ".espressif"))
}

// globLast: 执行一组 glob，全部命中排序后取最后一个（无命中 false）。
func globLast(pats []string) (string, bool) {
	var hits []string
	for _, p := range pats {
		if m, _ := filepath.Glob(p); len(m) > 0 {
			hits = append(hits, m...)
		}
	}
	if len(hits) == 0 {
		return "", false
	}
	sort.Strings(hits)
	return hits[len(hits)-1], true
}

// Plan: 解析 Spec → 完整 esptool argv（不执行）。dry-run 预演与事件审计共用，
// 路径与 esptool 发现都在调用方一侧解析。
func Plan(tty string, s Spec) ([]string, error) {
	args, err := BuildArgs(tty, s)
	if err != nil {
		return nil, err
	}
	esptool, err := ResolveEsptool(s.Esptool)
	if err != nil {
		return nil, err
	}
	return append([]string{filepath.Base(esptool)}, args...), nil
}

// BuildArgs: 组装 esptool 命令行（不含 argv[0]）。
func BuildArgs(tty string, s Spec) ([]string, error) {
	args := []string{"--port", tty}

	var bins []BinSpec
	if s.ArgsFile != "" {
		parsed, err := parseFlasherArgs(s.ArgsFile)
		if err != nil {
			return nil, err
		}
		bins = parsed
		if s.Chip == "" {
			if chip, ok := parsedChip(s.ArgsFile); ok {
				args = append(args, "--chip", chip)
			}
		}
	} else {
		bins = s.Bins
	}
	if len(bins) == 0 {
		return nil, fmt.Errorf("无可刷镜像（bins 为空且未提供 args_file）")
	}
	if s.Chip != "" {
		args = append(args, "--chip", s.Chip)
	}
	if s.Baud > 0 {
		args = append(args, "--baud", strconv.Itoa(s.Baud))
	}
	args = append(args, "write_flash")
	for _, b := range bins {
		if _, err := strconv.ParseUint(strings.TrimPrefix(b.Offset, "0x"), 16, 64); err != nil {
			return nil, fmt.Errorf("偏移不是合法十六进制: %q", b.Offset)
		}
		if _, err := os.Stat(b.Path); err != nil {
			return nil, fmt.Errorf("镜像不可读: %s", b.Path)
		}
		args = append(args, b.Offset, b.Path)
	}
	return args, nil
}

// parseFlasherArgs: 解析 flasher_args.json → 按 offset 排序的 BinSpec 列表
// （镜像路径相对于 args 文件所在目录）。
func parseFlasherArgs(path string) ([]BinSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 args_file 失败: %w", err)
	}
	var fa flasherArgs
	if err := json.Unmarshal(data, &fa); err != nil {
		return nil, fmt.Errorf("解析 flasher_args.json 失败: %w", err)
	}
	if len(fa.FlashFiles) == 0 {
		return nil, fmt.Errorf("flasher_args.json 无 flash_files")
	}
	dir := filepath.Dir(path)
	var offs []string
	for off := range fa.FlashFiles {
		offs = append(offs, off)
	}
	sort.Slice(offs, func(i, j int) bool {
		a, _ := strconv.ParseUint(strings.TrimPrefix(offs[i], "0x"), 16, 64)
		b, _ := strconv.ParseUint(strings.TrimPrefix(offs[j], "0x"), 16, 64)
		return a < b
	})
	bins := make([]BinSpec, 0, len(offs))
	for _, off := range offs {
		p := fa.FlashFiles[off]
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		bins = append(bins, BinSpec{Path: p, Offset: off})
	}
	return bins, nil
}

func parsedChip(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var fa struct {
		ExtraEsptoolArgs map[string]any `json:"extra_esptool_args"`
	}
	if json.Unmarshal(data, &fa) != nil {
		return "", false
	}
	if c, ok := fa.ExtraEsptoolArgs["--chip"].(string); ok && c != "" {
		return c, true
	}
	return "", false
}

// Run: 执行 esptool，stdout/stderr 按行流式回调（\r 与 \n 都算行界 ——
// esptool 进度条用 \r 刷新）。timeout > 0 时超时杀进程（挂死的 esptool 会
// 永远持有串口）。返回进程退出码错误。
func Run(esptool, tty string, s Spec, timeout time.Duration, output func(line string)) error {
	args, err := BuildArgs(tty, s)
	if err != nil {
		return err
	}
	if esptool == "" {
		if esptool, err = ResolveEsptool(s.Esptool); err != nil {
			return err
		}
	}
	ctx := context.Background()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, esptool, args...)
	stdout, err1 := cmd.StdoutPipe()
	stderr, err2 := cmd.StderrPipe()
	if err1 != nil || err2 != nil {
		return fmt.Errorf("创建管道失败")
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 esptool 失败: %w", err)
	}
	done := make(chan struct{}, 2)
	var outMu sync.Mutex // 两路扫描协程共享 output 回调，串行化防交错
	scan := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Split(splitCRLF)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				outMu.Lock()
				output(line)
				outMu.Unlock()
			}
		}
		done <- struct{}{}
	}
	go scan(stdout)
	go scan(stderr)
	<-done
	<-done
	err = cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("esptool 超时（%s）被强制终止 —— 请检查设备连接或调大 flash_timeout_s", timeout)
	}
	return err
}

// splitCRLF: 以 \r 或 \n 任一为行界。
func splitCRLF(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytesIndexAny(data); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func bytesIndexAny(b []byte) int {
	for i, c := range b {
		if c == '\n' || c == '\r' {
			return i
		}
	}
	return -1
}
