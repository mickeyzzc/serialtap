// Package analyze 提供离线分析：签名汇总与 Backtrace addr2line 解码。
package analyze

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mickeyzzc/serialtap/internal/config"
	"github.com/mickeyzzc/serialtap/internal/signature"
)

// analyzeLogs: 离线扫描日志，按签名汇总（计数 / 首末时间 / 样本行）。
// 首末时间跨文件按时间序取 min/max（时间戳定宽 ISO 格式，字典序=时间序），
// 与文件传入顺序无关。
func Logs(w io.Writer, paths []string, showLines bool) error {
	eng := signature.New(nil)
	tsRe := regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3})\]`)

	type stat struct {
		count  int
		first  string
		last   string
		sample string
	}
	tally := map[string]*stat{}
	total := 0

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			name, ok := eng.Match(line)
			if !ok {
				continue
			}
			total++
			st := tally[name]
			if st == nil {
				st = &stat{sample: truncate(line, 160)}
				tally[name] = st
			}
			st.count++
			if ts := tsRe.FindStringSubmatch(line); ts != nil {
				if st.first == "" || ts[1] < st.first {
					st.first = ts[1]
				}
				if ts[1] > st.last {
					st.last = ts[1]
				}
			}
			if showLines {
				fmt.Fprintln(w, line)
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return err
		}
	}

	if len(tally) == 0 {
		fmt.Fprintln(w, "未命中任何签名。")
		return nil
	}
	fmt.Fprintf(w, "扫描 %d 个文件，命中 %d 行：\n\n", len(paths), total)
	// 按计数降序
	keys := make([]string, 0, len(tally))
	for k := range tally {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return tally[keys[i]].count > tally[keys[j]].count })
	fmt.Fprintf(w, "%-18s %7s  %-23s %-23s  %s\n", "SIGNATURE", "COUNT", "FIRST", "LAST", "SAMPLE")
	for _, k := range keys {
		st := tally[k]
		if st.first == "" {
			st.first, st.last = "-", "-"
		}
		fmt.Fprintf(w, "%-18s %7d  %-23s %-23s  %s\n", k, st.count, st.first, st.last, st.sample)
	}
	return nil
}

var (
	addrPairRe = regexp.MustCompile(`(0x[0-9a-fA-F]+):0x[0-9a-fA-F]+`) // Backtrace 帧的 addr:sp 对
	pcMarkRe   = regexp.MustCompile(`\|<-(0x[0-9a-fA-F]+)`)            // |<-PC 标记
)

// backtraceAddrs: 从一行 Backtrace 文本提取地址帧（addr:sp 对取 addr + |<-PC）。
func backtraceAddrs(line string) []string {
	var addrs []string
	for _, m := range addrPairRe.FindAllStringSubmatch(line, -1) {
		addrs = append(addrs, m[1])
	}
	if m := pcMarkRe.FindStringSubmatch(line); m != nil {
		addrs = append(addrs, m[1])
	}
	return addrs
}

// cmdDecodeBacktrace: 从日志提取 Backtrace 地址帧，用 addr2line 翻译成 源文件:行号。
// elf 省略时按日志路径 <root>/<name>/… 从配置 elf_map[name] 取。
func DecodeBacktrace(logPath, elf, addr2lineBin string, cfg config.Config) error {
	if elf == "" {
		elf = elfFromLogPath(logPath, cfg)
	}
	if elf == "" {
		return fmt.Errorf("未指定 --elf，且配置 elf_map 无该设备映射；无法解码")
	}
	if _, err := os.Stat(elf); err != nil {
		return fmt.Errorf("elf 文件不存在: %s", elf)
	}
	bin, err := findAddr2line(addr2lineBin)
	if err != nil {
		return err
	}
	fmt.Printf("# addr2line: %s\n# elf:       %s\n\n", bin, elf)

	f, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	found := 0
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "Backtrace") {
			continue
		}
		addrs := backtraceAddrs(line)
		if len(addrs) == 0 {
			continue
		}
		found++
		args := append([]string{"-pfiaC", "-e", elf}, addrs...)
		out, err := exec.Command(bin, args...).Output()
		fmt.Println(line)
		if err != nil {
			fmt.Printf("  addr2line 失败: %v\n\n", err)
			continue
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Println("  " + l)
		}
		fmt.Println()
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if found == 0 {
		fmt.Println("日志中未发现 Backtrace 行。")
	}
	return nil
}

func elfFromLogPath(logPath string, cfg config.Config) string {
	parts := strings.Split(filepath.ToSlash(logPath), "/")
	if len(parts) < 2 {
		return ""
	}
	name := parts[len(parts)-2]
	return cfg.ElfMap[name]
}

// findAddr2line: --addr2line > $ESP_ADDR2LINE > PATH 常见名 > ~/.espressif/tools glob。
func findAddr2line(explicit string) (string, error) {
	if explicit != "" {
		if p, err := exec.LookPath(explicit); err == nil {
			return p, nil
		}
		return explicit, fmt.Errorf("--addr2line 指定的 %s 不可执行", explicit)
	}
	if env := os.Getenv("ESP_ADDR2LINE"); env != "" {
		return env, nil
	}
	names := []string{
		"xtensa-esp32-elf-addr2line",
		"xtensa-esp32s3-elf-addr2line",
		"riscv32-esp-elf-addr2line",
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("找不到 addr2line（请 source ESP-IDF 环境或 --addr2line 指定）")
	}
	pats := []string{
		filepath.Join(home, ".espressif", "tools", "*", "*", "*", "bin", "*addr2line*"),
		filepath.Join(home, ".espressif", "tools", "*", "*", "bin", "*addr2line*"),
	}
	var hits []string
	for _, p := range pats {
		if m, _ := filepath.Glob(p); len(m) > 0 {
			hits = append(hits, m...)
		}
	}
	sort.Strings(hits)
	if len(hits) == 0 {
		return "", fmt.Errorf("找不到 addr2line（请 source ESP-IDF 环境或 --addr2line 指定）")
	}
	return hits[len(hits)-1], nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
