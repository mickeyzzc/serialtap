// Package facts: 设备身份事实引擎——从常开日志流（被动）、固件 #DEV 自述行、
// esptool 探测输出（主动）三类来源提取结构化事实（芯片/复位原因/PSRAM/WiFi/
// IP/型号……），每条带证据（来源行 + 时间 + 来源类别），按设备持久化到
// <root>/<设备>/info.json。定位与签名引擎同构：serialtap 只做模式提取，
// 不做业务判断；规则表可随 IDF 版本演进（每条事实保留原始行可审计）。
package facts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 事实来源（决定覆盖优先级：固件自述 > 主动探测 > 被动日志）。
const (
	OriginDev   = "dev"   // 固件 #DEV 自述行——最权威
	OriginProbe = "probe" // esptool info 探测（会复位板子，按需触发）
	OriginLog   = "log"   // 被动日志提取——零打扰、插上即积累
)

var originRank = map[string]int{OriginDev: 3, OriginProbe: 2, OriginLog: 1}

// Fact: 一条事实。Source 是证据原始行（截断），审计与排错用。
type Fact struct {
	Value  string `json:"value"`
	Source string `json:"source"`
	At     int64  `json:"at"` // UnixMilli
	Origin string `json:"origin"`
}

// Store: 单设备事实集。并发安全（采集协程写、状态读取协程读）。
type Store struct {
	mu    sync.Mutex
	dir   string
	facts map[string]Fact
}

// 被动日志规则表。行首 [时间戳] 前缀已剥离；全部用真实日志行校准。
var logRules = []struct {
	key  string
	re   *regexp.Regexp
	pick func(m []string) string // 从捕获组拼值
}{
	// 固件自述（L2 契约行）：#DEV model=... fw=... —— 单独在 feed 里展开
	// 芯片族 + ROM 版本：ESP-ROM:esp32s3-api1-20210207
	{"chip", regexp.MustCompile(`ESP-ROM:(esp32c3|esp32s3|esp32s2|esp32c6|esp32c5|esp32h2|esp32|esp32c2)-`),
		func(m []string) string { return chipName(m[1]) }},
	{"rom", regexp.MustCompile(`ESP-ROM:\w+-(\S+)`),
		func(m []string) string { return m[1] }},
	// 复位原因：rst:0x15 (USB_UART_CHIP_RESET),boot:0x2b
	{"reset", regexp.MustCompile(`rst:0x[0-9A-Fa-f]+ \(([A-Z_]+)\)`),
		func(m []string) string { return m[1] }},
	// PSRAM：esp_psram: Found 8MB / esp32: Found 8MB PSRAM（IDF4 旧格式）
	{"psram", regexp.MustCompile(`esp(?:32)?_?psram:.*?(\d+)\s*MB`),
		func(m []string) string { return m[1] + "MB" }},
	{"psram", regexp.MustCompile(`esp32:.*?(\d+)MB PSRAM`),
		func(m []string) string { return m[1] + "MB" }},
	// WiFi 信道：wifi:new:<11,2>, old:...（主信道 = 第一个数）
	{"wifi_ch", regexp.MustCompile(`wifi:new:<(\d+),`),
		func(m []string) string { return m[1] }},
	// IP：got ip:192.168.63.9 / sta ip: ... / IP address: ... / ip:...
	{"ip", regexp.MustCompile(`(?:got ip|sta ip|ip addr(?:ess)?|IP)[: ]+(\d{1,3}(?:\.\d{1,3}){3})`),
		func(m []string) string { return m[1] }},
	// SSID：connect to ssid [home]（IDF wifi 事件）/ SSID: home
	{"ssid", regexp.MustCompile(`connect to ssid \[([^\]]*)\]`),
		func(m []string) string { return m[1] }},
	{"ssid", regexp.MustCompile(`\bSSID:?\s*([^\s,\]]+)`),
		func(m []string) string { return m[1] }},
}

// esptool 探测输出规则（info = flash_id）。
var probeRules = []struct {
	key  string
	re   *regexp.Regexp
	pick func(m []string) string
}{
	{"chip", regexp.MustCompile(`Chip (?:is |type:\s+)(ESP32-\w+)`),
		func(m []string) string { return m[1] }}, // v4 "Chip is X" / v5 "Chip type: X"
	{"chip_rev", regexp.MustCompile(`Chip (?:is|type:).*\((revision [^)]*)\)`),
		func(m []string) string { return m[1] }},
	{"mac", regexp.MustCompile(`MAC:\s+([0-9a-fA-F:]{17})`),
		func(m []string) string { return strings.ToLower(m[1]) }},
	{"flash", regexp.MustCompile(`(?:Computed|Detected) flash size[:=]\s*(\d+)\s*([MG]B)`),
		func(m []string) string { return m[1] + m[2] }},
	{"flash_id", regexp.MustCompile(`Manufacturer: (\S+) Device: (\S+)`),
		func(m []string) string { return m[1] + "/" + m[2] }},
}

// #DEV 自述行：#DEV model=n16r8-cam fw=1.2.3 flash=16M psram=8M mac=.. ip=.. ssid=..
var devLineRe = regexp.MustCompile(`^#DEV\s+(.*)`)
var devKVRe = regexp.MustCompile(`([A-Za-z_][\w-]*)=("[^"]*"|\S+)`)

var tsPrefixRe = regexp.MustCompile(`^\[[^\]]*\]\s?`)

func chipName(id string) string {
	switch id {
	case "esp32s3":
		return "ESP32-S3"
	case "esp32s2":
		return "ESP32-S2"
	case "esp32c3":
		return "ESP32-C3"
	case "esp32c6":
		return "ESP32-C6"
	case "esp32c5":
		return "ESP32-C5"
	case "esp32h2":
		return "ESP32-H2"
	case "esp32c2":
		return "ESP32-C2"
	}
	return strings.ToUpper(id)
}

// Open: 打开（或创建）设备事实存储，加载已有的 info.json（换名继承/守护重启不丢）。
func Open(deviceDir string) *Store {
	s := &Store{dir: deviceDir, facts: map[string]Fact{}}
	if b, err := os.ReadFile(s.path()); err == nil {
		_ = json.Unmarshal(b, &s.facts) // 损坏则从零开始，不致命
	}
	return s
}

func (s *Store) path() string { return filepath.Join(s.dir, "info.json") }

// Feed: 喂一行日志（可带 [时间戳] 前缀），命中规则即更新事实。
// 返回是否有变化（事实变化稀疏，变化即落盘）。
func (s *Store) Feed(line string) bool {
	body := stripAnsi(tsPrefixRe.ReplaceAllString(line, ""))
	now := time.Now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()

	// #DEV 自述行：k=v 全量入表（origin=dev，最高优先级）
	if m := devLineRe.FindStringSubmatch(body); m != nil {
		changed := false
		for _, kv := range devKVRe.FindAllStringSubmatch(m[1], -1) {
			v := strings.Trim(kv[2], `"`)
			if s.set(kv[1], v, body, now, OriginDev) {
				changed = true
			}
		}
		if changed {
			s.save()
		}
		return changed
	}

	changed := false
	for _, r := range logRules {
		if m := r.re.FindStringSubmatch(body); m != nil {
			if s.set(r.key, r.pick(m), body, now, OriginLog) {
				changed = true
			}
		}
	}
	if changed {
		s.save()
	}
	return changed
}

// FeedProbe: 喂 esptool 探测输出行（origin=probe，高于被动日志）。
func (s *Store) FeedProbe(line string) bool {
	now := time.Now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, r := range probeRules {
		if m := r.re.FindStringSubmatch(line); m != nil {
			if s.set(r.key, r.pick(m), line, now, OriginProbe) {
				changed = true
			}
		}
	}
	if changed {
		s.save()
	}
	return changed
}

// set: 写一条事实。覆盖规则：更高来源优先；同来源新值覆盖旧值（IP/信道会漂移）。
// 返回是否有变化。
func (s *Store) set(key, value, source string, at int64, origin string) bool {
	if value == "" {
		return false
	}
	old, ok := s.facts[key]
	if ok && originRank[origin] < originRank[old.Origin] {
		return false // 低优先级来源不覆盖高优先级事实
	}
	if ok && old.Value == value && old.Origin == origin {
		return false // 完全相同则不重写
	}
	s.facts[key] = Fact{Value: value, Source: truncate(source, 160), At: at, Origin: origin}
	return true
}

// Snapshot: 全量事实（面板详情表用）。
func (s *Store) Snapshot() map[string]Fact {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Fact, len(s.facts))
	for k, v := range s.facts {
		out[k] = v
	}
	return out
}

// summaryKeys: DevState 摘要字段（随 mesh 聚合广播，保持短小）。
var summaryKeys = []string{"model", "fw", "chip", "mac", "flash", "psram", "ip", "ssid", "wifi_ch", "reset"}

// Summary: 摘要视图（卡片一行身份用）。
func (s *Store) Summary() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, k := range summaryKeys {
		if f, ok := s.facts[k]; ok {
			out[k] = f.Value
		}
	}
	return out
}

func (s *Store) save() {
	b, err := json.MarshalIndent(s.facts, "", " ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path(), b, 0o644)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// stripAnsi: 剥终端颜色转义（与面板同规则）。
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func stripAnsi(s string) string { return ansiRe.ReplaceAllString(s, "") }

// SeedFromLog: 从设备目录最新 serial-*.log 的末尾回灌历史行——守护重启/
// 换名继承后几秒内恢复既有事实（芯片/复位原因/IP/SSID 多在近期日志里），
// 不必等下一次 boot banner 或 WiFi 事件。
func (s *Store) SeedFromLog(maxBytes int64) {
	if maxBytes <= 0 {
		maxBytes = 256 << 10
	}
	name, ok := latestSerial(s.dir)
	if !ok {
		return
	}
	b, err := readTail(filepath.Join(s.dir, name), maxBytes)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if line != "" {
			s.Feed(line)
		}
	}
}

// latestSerial: 目录下最新的 serial-YYYYMMDD[.NNN].log（与面板/尾随同语义：
// 同日序号大者新——轮转后序号文件才是活跃文件）。
func latestSerial(dir string) (string, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	best, bestDay, bestSfx := "", "", -1
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "serial-") || !strings.HasSuffix(n, ".log") {
			continue
		}
		core := strings.TrimSuffix(strings.TrimPrefix(n, "serial-"), ".log")
		day, sfx := core, 0
		if i := strings.IndexByte(core, '.'); i >= 0 {
			if v, err := strconv.Atoi(core[i+1:]); err == nil {
				day, sfx = core[:i], v
			}
		}
		if day > bestDay || (day == bestDay && sfx > bestSfx) {
			best, bestDay, bestSfx = n, day, sfx
		}
	}
	return best, best != ""
}

func readTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := fi.Size() - max
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, fi.Size()-start)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	buf = buf[:n]
	if start > 0 { // 丢弃文件中部切出的半行
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return buf, nil
}
