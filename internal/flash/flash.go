// Package flash 编排代理刷固件：构建 esptool 命令、流式执行、
// 解析 ESP-IDF 的 flasher_args.json。
package flash

import (
	"bufio"
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
}

// flasherArgs: ESP-IDF build/flasher_args.json 的相关子集。
type flasherArgs struct {
	ExtraEsptoolArgs map[string]string `json:"extra_esptool_args"`
	FlashFiles       map[string]string `json:"flash_files"`
}

// ResolveEsptool: 显式指定 > PATH 里的 esptool > esptool.py。
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
	return "", fmt.Errorf("找不到 esptool（PATH 无 esptool/esptool.py；请 source ESP-IDF 环境或 --esptool 指定）")
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
		ExtraEsptoolArgs map[string]string `json:"extra_esptool_args"`
	}
	if json.Unmarshal(data, &fa) != nil {
		return "", false
	}
	if c, ok := fa.ExtraEsptoolArgs["--chip"]; ok && c != "" {
		return c, true
	}
	return "", false
}

// Run: 执行 esptool，stdout/stderr 按行流式回调（\r 与 \n 都算行界 ——
// esptool 进度条用 \r 刷新）。返回进程退出码错误。
func Run(esptool, tty string, s Spec, output func(line string)) error {
	args, err := BuildArgs(tty, s)
	if err != nil {
		return err
	}
	if esptool == "" {
		if esptool, err = ResolveEsptool(s.Esptool); err != nil {
			return err
		}
	}
	cmd := exec.Command(esptool, args...)
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
	return cmd.Wait()
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
