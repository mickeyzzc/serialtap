// selftest 子命令：聚合设备日志里的 SELFTEST 自检行（固件自检行规范见
// 工作区 AGENTS.md——固件开机打一行/两行 "SELFTEST: key=value ..." 体检
// 证据，本命令按设备归档日志反向检索最近一次开机的那组行，并给出
// 简单异常提示：传感器没探到、空口全聋、配置网信号弱）。
//
// 用法:
//
//	serialtap selftest [RE] [--config F] [--root DIR] [--files N]
//	RE 匹配设备名（正则，省略=全部设备）；只读本机日志，不动串口。
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	selftestMarker   = "SELFTEST:"
	selftestMaxFiles = 14  // 每设备最多回溯的日志文件数（日期×轮转后缀）
	selftestCluster  = 200 // 同一开机组内相邻自检行的最大行距
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")
var rssiRe = regexp.MustCompile(`(-\d+)dBm`)
var serialFileRe = regexp.MustCompile(`^serial-(\d{8})\.log(\.\d{3})?$`)

// SelftestEntry: 单设备的检索结果。Lines 为同一次开机的那组原始行
// （含时间戳前缀）；Missing=回溯范围内未见自检行（固件未实现规范或
// 日志轮转已冲掉）。
type SelftestEntry struct {
	Device  string
	File    string
	Lines   []string
	Missing bool
}

// stripANSI: 日志行可能带终端色彩码（串口直通落档），匹配/解析前去掉。
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// scanDeviceSelftest: 在设备目录的 serial-* 日志里反向找最近一组自检行。
func scanDeviceSelftest(dir string, maxFiles int) SelftestEntry {
	entry := SelftestEntry{Device: filepath.Base(dir), Missing: true}

	ents, err := os.ReadDir(dir)
	if err != nil {
		return entry
	}
	var files []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		m := serialFileRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		// 排序键：日期字符串 + 轮转后缀数字（.001 比 .000 即基础文件新）
		suffix := 0
		if m[2] != "" {
			suffix, _ = strconv.Atoi(strings.TrimPrefix(m[2], "."))
		}
		files = append(files, fmt.Sprintf("%s|%03d|%s", m[1], suffix, name))
	}
	// 新→旧：日期降序，同日轮转后缀降序（后缀大的是更晚的轮转）
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]
		ad, bd := a[:8], b[:8]
		if ad != bd {
			return ad > bd
		}
		return a[9:12] > b[9:12]
	})
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}

	for _, f := range files {
		name := f[13:]
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		// 反向收集同一开机组的自检行（组内相邻行距 ≤ cluster）
		var cluster []string
		lastIdx := -1
		for i := len(all) - 1; i >= 0; i-- {
			if !strings.Contains(stripANSI(all[i]), selftestMarker) {
				continue
			}
			if lastIdx == -1 || lastIdx-i <= selftestCluster {
				cluster = append([]string{all[i]}, cluster...)
				lastIdx = i
			} else {
				break // 更早的行属于上一次开机
			}
		}
		if len(cluster) > 0 {
			entry.File = path
			entry.Lines = cluster
			entry.Missing = false
			return entry
		}
	}
	return entry
}

// selftestWarnings: 对一组自检行做常识性异常提示（不追求完备，只报
// 无争议的坏消息）。返回空 = 未发现异常。
func selftestWarnings(lines []string) []string {
	var warns []string
	for _, raw := range lines {
		line := stripANSI(raw)
		body := line
		if i := strings.Index(body, selftestMarker); i >= 0 {
			body = body[i+len(selftestMarker):]
		}
		if strings.Contains(body, "sensor=none") {
			warns = append(warns, "传感器未检出（sensor=none）——排查排线/XCLK/模块供电")
		}
		if strings.Contains(body, "psram=0MB") {
			warns = append(warns, "PSRAM 未起来（psram=0MB）——检查 sdkconfig/上电时序")
		}
		if strings.HasPrefix(strings.TrimSpace(body), "wifi") {
			if strings.Contains(body, "aps=0") {
				warns = append(warns, "空口全聋（aps=0）——扫描一个 AP 都没见着，射频/天线嫌疑")
			}
			if i := strings.Index(body, `ours="`); i >= 0 {
				ours := body[i+6:]
				if j := strings.Index(ours, `"`); j >= 0 {
					ours = ours[:j]
				}
				for _, m := range rssiRe.FindAllStringSubmatch(ours, -1) {
					if v, err := strconv.Atoi(m[1]); err == nil && v <= -75 {
						warns = append(warns, fmt.Sprintf("配置网信号弱（%sdBm ≤ -75），预期不稳定", m[1]))
						break
					}
				}
			}
		}
	}
	return warns
}

func runSelftest(args []string) int {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件 JSON")
	rootFlag := fs.String("root", "", "日志根目录（默认取配置）")
	filesN := fs.Int("files", selftestMaxFiles, "每设备最多回溯的日志文件数")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: serialtap selftest [RE] [--config F] [--root DIR] [--files N]\n"+
			"  按设备归档日志反向检索最近一次开机的 SELFTEST 自检行并做异常提示。\n"+
			"  RE 匹配设备名（正则，省略=全部设备）。只读日志，不动串口。\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	rest := fs.Args()

	cfg, err := loadCfgMerged(*configPath, *rootFlag, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取配置失败:", err)
		return 2
	}
	root := cfg.Root

	pattern := ""
	if len(rest) > 0 {
		pattern = rest[0]
	}
	var re *regexp.Regexp
	if pattern != "" {
		re, err = regexp.Compile(pattern)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无效正则 %q: %v\n", pattern, err)
			return 2
		}
	}

	ents, err := os.ReadDir(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读日志根目录 %s 失败: %v\n", root, err)
		return 2
	}

	found, missing := 0, 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if re != nil && !re.MatchString(name) {
			continue
		}
		entry := scanDeviceSelftest(filepath.Join(root, name), *filesN)
		fmt.Printf("== %s\n", name)
		if entry.Missing {
			missing++
			fmt.Printf("   （近 %d 个日志未见自检行——固件未实现规范或已轮转）\n", *filesN)
			continue
		}
		found++
		rel, _ := filepath.Rel(root, entry.File)
		fmt.Printf("   来源: %s\n", rel)
		for _, l := range entry.Lines {
			fmt.Printf("   %s\n", stripANSI(l))
		}
		for _, w := range selftestWarnings(entry.Lines) {
			fmt.Printf("   ⚠ %s\n", w)
		}
	}
	fmt.Printf("-- %d 台有自检行, %d 台未见\n", found, missing)
	return 0
}
