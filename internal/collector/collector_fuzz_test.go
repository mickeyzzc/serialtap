package collector

import (
	"strings"
	"testing"
)

func FuzzLineAssembler(f *testing.F) {
	f.Add([]byte("rst:0x1 (POWERON_RESET)\r\n"))
	f.Add([]byte("no-newline-tail"))
	f.Add([]byte("\r\n\r\n\r\n"))
	f.Add([]byte("a\r\nb\nc"))
	f.Add([]byte{0x00, 0xff, 0xfe, 0x0a, 0x0d})
	f.Add([]byte(strings.Repeat("x", maxTail+100)))
	f.Add(append([]byte("ok\n"), bytesRepeat('x', maxTail+8)...))

	f.Fuzz(func(t *testing.T, data []byte) {
		var a lineAssembler
		var emitted []string
		for i := 0; i < len(data); i += 7 {
			end := i + 7
			if end > len(data) {
				end = len(data)
			}
			emitted = append(emitted, a.feed(data[i:end])...)
		}
		rest := a.flush()

		for _, l := range emitted {
			if strings.Contains(l, "\n") {
				t.Fatalf("emitted line contains LF: %q", l)
			}
		}
		if strings.Contains(rest, "\n") {
			t.Fatalf("flush remainder contains LF: %q", rest)
		}
		if len(a.tail) != 0 {
			t.Fatalf("tail not drained after flush: %d bytes", len(a.tail))
		}
	})
}

func BenchmarkLineAssemblerFeed(b *testing.B) {
	chunk := []byte(strings.Repeat("I (100) boot: hello world this is a serial line\r\n", 20))
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var a lineAssembler
		_ = a.feed(chunk)
		_ = a.flush()
	}
}

func bytesRepeat(c byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = c
	}
	return out
}
