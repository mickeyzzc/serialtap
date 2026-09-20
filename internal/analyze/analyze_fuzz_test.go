package analyze

import (
	"strings"
	"testing"
)

func FuzzBacktraceAddrs(f *testing.F) {
	f.Add("Backtrace: 0x40081234:0x3ffb1234 0x4008abcd:0x3ffb1250 |<-CORRUPTED")
	f.Add("Backtrace: |<-PC 0xDECAFBAD")
	f.Add("Backtrace:")
	f.Add("0x:0x 0x")
	f.Add("Backtrace: 0x400d1234:0x3ffb0030 |<-0x400d5678")

	f.Fuzz(func(t *testing.T, line string) {
		for _, a := range backtraceAddrs(line) {
			if !strings.HasPrefix(a, "0x") || len(a) < 3 {
				t.Fatalf("malformed address returned: %q", a)
			}
		}
	})
}
