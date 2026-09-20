package main

import (
	"fmt"
	"regexp"
	"sync"
)

// 默认事件签名表 —— 全量日志照收，命中签名的行另记入 events-*.log。
// 覆盖 ESP-IDF 设备最常见的故障行：
// lwIP "accept (n)"（lwIP 插座耗尽 EMFILE 家族）、"E ("（ESP_LOG 错误级）、
// "rst:0x"（bootloader 复位 banner，重启检测的根依据）。
type sigDef struct {
	pattern string
	name    string
	insens  bool // 大小写不敏感
}

var defaultSigs = []sigDef{
	{`rst:0x`, "reset-banner", false},
	{`boot:0x`, "boot-mode", false},
	{`esp_restart`, "restart-call", false},
	{`E \(`, "esp-log-error", false},
	{`Guru Meditation`, "guru-meditation", false},
	{`Backtrace:`, "backtrace", false},
	{`panic`, "panic", true},
	{`WDT|watchdog`, "watchdog", true},
	{`abort`, "abort", true},
	{`assert`, "assert", true},
	{`accept \(-?\d+\)`, "lwip-accept-err", false},
	{`probe failed`, "probe-failed", true},
	{`pausing`, "pausing", true},
	{`reboot`, "reboot", true},
}

type compiledSig struct {
	re   *regexp.Regexp
	name string
}

type SignatureEngine struct {
	mu   sync.RWMutex
	sigs []compiledSig
}

// NewSignatureEngine: 默认签名 + 配置追加（追加项名字 extra-N）。
// 单条正则编译失败只跳过该条（不让守护进程死在一条坏正则上）。
func NewSignatureEngine(extra []string) *SignatureEngine {
	e := &SignatureEngine{}
	for _, d := range defaultSigs {
		if re, err := compileSig(d.pattern, d.insens); err == nil {
			e.sigs = append(e.sigs, compiledSig{re: re, name: d.name})
		}
	}
	for i, p := range extra {
		if p == "" {
			continue
		}
		if re, err := compileSig(p, false); err == nil {
			e.sigs = append(e.sigs, compiledSig{re: re, name: fmt.Sprintf("extra-%d", i)})
		}
	}
	return e
}

func compileSig(pattern string, insens bool) (*regexp.Regexp, error) {
	if insens {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

// Match: 返回命中的第一个签名名。行内容为空不算事件。
func (e *SignatureEngine) Match(line string) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(line) == 0 {
		return "", false
	}
	for _, s := range e.sigs {
		if s.re.MatchString(line) {
			return s.name, true
		}
	}
	return "", false
}

// Names: 签名清单（analyze 汇总用）。
func (e *SignatureEngine) Names() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.sigs))
	for _, s := range e.sigs {
		out = append(out, s.name)
	}
	return out
}
