package collector

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzLineAssembler: 与朴素参考模型比对 —— 任意字节流下拼装器的输出
// 必须与"逐字节遇 \n 切行"完全一致（\r 于行尾剥除），且缓冲有界。
func FuzzLineAssembler(f *testing.F) {
	f.Add([]byte("hello\nworld"))
	f.Add([]byte("a\r\nb\n"))
	f.Add([]byte{0x00, 0xff, '\n', 0x80})
	f.Add([]byte(strings.Repeat("x", maxTail*2)))
	f.Add([]byte("\n\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var asm lineAssembler
		var got []string
		// 分块喂入（随机切成两半，验证跨块拼装）
		mid := len(data) / 2
		got = append(got, asm.feed(data[:mid])...)
		got = append(got, asm.feed(data[mid:])...)
		got = append(got, asm.feed(nil)...)
		tail := asm.flush()

		// 参考模型：逐字节切行
		var want []string
		var cur []byte
		for _, b := range data {
			if b == '\n' {
				want = append(want, strings.TrimSuffix(string(cur), "\r"))
				cur = cur[:0]
			} else {
				cur = append(cur, b)
			}
		}

		// 泥石流保护生效时（无换行超 maxTail）参考模型失效，只验有界性
		if len(data) > 0 && !bytes.ContainsRune(data, '\n') && len(data) > maxTail {
			if len(tail) > maxTail {
				t.Fatalf("缓冲超界: %d > %d", len(tail), maxTail)
			}
			return
		}

		if len(got) != len(want) {
			t.Fatalf("行数不符: got %d want %d\ngot=%q\nwant=%q", len(got), len(want), got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("第 %d 行不符: got %q want %q", i, got[i], want[i])
			}
		}
		if tail != string(cur) {
			t.Fatalf("残余不符: got %q want %q", tail, string(cur))
		}
	})
}
