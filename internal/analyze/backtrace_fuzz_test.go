package analyze

import (
	"regexp"
	"strings"
	"testing"
)

// FuzzBacktraceAddrs: 任意输入不 panic；返回的每个地址都形如 0x十六进制
// 且确实出现在输入串里；|<-PC 标记不被重复计入。
func FuzzBacktraceAddrs(f *testing.F) {
	f.Add("Backtrace: 0x400D0C5A:0x3FFB7D40 0x4008C71E:0x3FFB7D60")
	f.Add("Backtrace: 0x4008:0x3ffb1c30 |<-0x4008")
	f.Add("Backtrace: 0x:0x |<-0xZZ")
	f.Add("")
	f.Add("0xdeadbeef:0xfeed 0x1:0x2 0xABCDEF:0x")
	f.Fuzz(func(t *testing.T, line string) {
		addrs := backtraceAddrs(line)
		addrRe := regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
		for _, a := range addrs {
			if !addrRe.MatchString(a) {
				t.Fatalf("非法地址格式: %q (input %q)", a, line)
			}
			if !strings.Contains(line, a) {
				t.Fatalf("地址 %q 不在输入中: %q", a, line)
			}
		}
	})
}
