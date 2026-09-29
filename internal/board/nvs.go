package board

import (
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
)

// NVS（Non-Volatile Storage）分区解析。布局（esp-idf components/nvs_flash）：
//   - 4096 字节页：32B 页头 + 32B 条目位图 + 126 × 32B 条目
//   - 条目：ns(1) type(1) span(1) chunk(1) crc32(4) key[15] data[8]
//   - 变长类型（0x21 字符串等）：data 前 2 字节为长度，内容紧跟条目头的
//     后续槽位（span 覆盖头+数据）
//
// 2026-09-29 实机验证（esp-idf v6.0.1，esp32s3）：IDF 自带 nvs_parser CLI
// 在该版本不可用，本解析器按 nvs_types.hpp Item 布局实现并已对真机
// dump 校验（见 PIT-059 关联事件）。

const (
	nvsPageSize   = 4096
	nvsEntryStart = 64 // 页头 32 + 位图 32
	nvsEntrySize  = 32
	nvsMaxEntries = 126
)

// NVS 条目类型（nvs_types.hpp ItemType，实测值）。
const (
	typeU8        uint8 = 0x01
	typeI8        uint8 = 0x11
	typeU16       uint8 = 0x02
	typeI16       uint8 = 0x12
	typeU32       uint8 = 0x03
	typeI32       uint8 = 0x13
	typeU64       uint8 = 0x05
	typeI64       uint8 = 0x15
	typeString    uint8 = 0x21
	typeBlobIndex uint8 = 0x42
	typeBlobData  uint8 = 0x48
)

// Entry: 一条解析后的 NVS 条目。
type Entry struct {
	Namespace string // 命名空间名（未知索引则报 "ns<N>"）
	Key       string
	Type      string // u8/i32/str/blob…
	Value     string // 可读值；blob 报字节数
}

var secretKeyRe = regexp.MustCompile(`(?i)pass|pswd|pwd|secret|token|seed|key`)

// ParseNVS: 解析 NVS 分区镜像。凭据形键（pass/token/secret/seed/key）
// 默认掩码为 "****"；showSecrets=true 时原样输出。
func ParseNVS(data []byte, showSecrets bool) []Entry {
	nsNames := map[byte]string{} // nsIndex -> 命名空间名
	type rawEntry struct {
		ns   byte
		typ  uint8
		span uint8
		key  string
		val  string
	}
	var raws []rawEntry

	for page := 0; page+nvsPageSize <= len(data); page += nvsPageSize {
		pg := data[page : page+nvsPageSize]
		if binary.LittleEndian.Uint32(pg[0:4]) == 0xFFFFFFFF {
			continue // 擦除页
		}
		for i := 0; i < nvsMaxEntries; i++ {
			off := nvsEntryStart + i*nvsEntrySize
			e := pg[off : off+nvsEntrySize]
			if isAllFF(e) {
				continue
			}
			// span>1 的条目：后续 span-1 个槽位是它的变长数据，不是条目。
			// 不跳过会把字符串内容误读成幽灵条目（真机 dump 实证）。
			skip := 0
			if e[2] > 1 && e[2] < 0x7F {
				skip = int(e[2]) - 1
			}
			r := rawEntry{ns: e[0], typ: e[1], span: e[2], key: trimNUL(e[8:23])}
			if r.key == "" {
				continue
			}
			// 命名空间定义：ns=0 且 type=u8，key=命名空间名、data[0]=分配的
			// 索引（esp-idf v6 实机 dump 验证；如 mibee_cfg→4）。不作为数据输出。
			if r.ns == 0 && r.typ == typeU8 {
				nsNames[e[24]] = r.key
				continue
			}
			switch r.typ {
			case typeString:
				r.val = readVarLen(pg, off, e)
			case typeU8:
				r.val = fmt.Sprintf("%d", e[24])
			case typeI8:
				r.val = fmt.Sprintf("%d", int8(e[24]))
			case typeU16:
				r.val = fmt.Sprintf("%d", binary.LittleEndian.Uint16(e[24:26]))
			case typeI16:
				r.val = fmt.Sprintf("%d", int16(binary.LittleEndian.Uint16(e[24:26])))
			case typeU32:
				r.val = fmt.Sprintf("%d", binary.LittleEndian.Uint32(e[24:28]))
			case typeI32:
				r.val = fmt.Sprintf("%d", int32(binary.LittleEndian.Uint32(e[24:28])))
			case typeU64, typeI64:
				v := binary.LittleEndian.Uint64(e[24:32])
				if r.typ == typeI64 {
					r.val = fmt.Sprintf("%d", int64(v))
				} else {
					r.val = fmt.Sprintf("%d", v)
				}
			case typeBlobIndex:
				size := binary.LittleEndian.Uint32(e[24:28])
				r.val = fmt.Sprintf("<blob %d B>", size)
			case typeBlobData:
				r.val = fmt.Sprintf("<blob chunk %d B>", len(e[24:32]))
			default:
				r.val = fmt.Sprintf("<type 0x%02X>", r.typ)
			}
			raws = append(raws, r)
			i += skip
		}
	}

	out := make([]Entry, 0, len(raws))
	for _, r := range raws {
		nsName, ok := nsNames[r.ns]
		if !ok {
			nsName = fmt.Sprintf("ns%d", r.ns)
		}
		v := r.val
		if !showSecrets && secretKeyRe.MatchString(r.key) {
			v = "****"
		}
		out = append(out, Entry{Namespace: nsName, Key: r.key, Value: v, Type: typeName(r.typ)})
	}
	return out
}

// readVarLen: 变长条目取值——长度在 data[0:2]，内容在条目头后一槽位起。
func readVarLen(pg []byte, entryOff int, e []byte) string {
	size := int(binary.LittleEndian.Uint16(e[24:26]))
	if size <= 0 {
		return ""
	}
	start := entryOff + nvsEntrySize
	end := start + size
	if end > len(pg) {
		end = len(pg)
	}
	return trimNUL(pg[start:end])
}

func typeName(t uint8) string {
	switch t {
	case typeU8:
		return "u8"
	case typeI8:
		return "i8"
	case typeU16:
		return "u16"
	case typeI16:
		return "i16"
	case typeU32:
		return "u32"
	case typeI32:
		return "i32"
	case typeU64:
		return "u64"
	case typeI64:
		return "i64"
	case typeString:
		return "str"
	case typeBlobIndex, typeBlobData:
		return "blob"
	}
	return fmt.Sprintf("0x%02X", t)
}

// FormatNVS: NVS 条目的文本渲染（命名空间/键/类型/值）。
func FormatNVS(entries []Entry) []string {
	out := []string{fmt.Sprintf("%-14s %-20s %-5s %s", "NAMESPACE", "KEY", "TYPE", "VALUE")}
	for _, e := range entries {
		val := e.Value
		if idx := strings.IndexByte(val, '\n'); idx >= 0 {
			val = val[:idx] + "…"
		}
		if len(val) > 72 {
			val = val[:72] + "…"
		}
		out = append(out, fmt.Sprintf("%-14s %-20s %-5s %s", e.Namespace, e.Key, e.Type, val))
	}
	return out
}

func isAllFF(b []byte) bool {
	for _, c := range b {
		if c != 0xFF {
			return false
		}
	}
	return true
}
