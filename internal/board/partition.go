// Package board 提供对主板的读侧操作：分区表解析、NVS 提取解析、
// 任意 flash 区域 dump 与芯片信息 —— 全部经 esptool 在让口状态下执行
// （与 flash 同一编排纪律），解析器为纯函数便于测试。
package board

import (
	"encoding/binary"
	"fmt"
)

// 分区表：0x8000 起的 32 字节定长条目（legacy binary 格式）。
// magic AA50；0xEBEB 为 MD5 校验尾条目，全 FF 为未写区。
const (
	PartTableOffset = 0x8000
	PartTableSize   = 0x1000
	entrySize       = 32
	magicPart       = 0x50AA // 小端字节序 AA 50
	magicMD5        = 0xEBEB
	magicErased     = 0xFFFF
)

// Partition: 一条分区表条目。
type Partition struct {
	Type    byte // 0=app 1=data
	SubType byte // 如 nvs=0x02, ota=0x00, spiffs=0x82
	Offset  uint32
	Size    uint32
	Label   string
	Flags   uint32
}

// TypeName: 分区类型的人类可读名。
func (p Partition) TypeName() string {
	if p.Type == 0 {
		return "app"
	}
	return "data"
}

// Find: 按标签查分区。
func Find(parts []Partition, label string) (Partition, bool) {
	for _, p := range parts {
		if p.Label == label {
			return p, true
		}
	}
	return Partition{}, false
}

// ParsePartitions: 解析分区表镜像（0x1000 字节区）。MD5 尾条目与未写区
// 终止扫描；单条 magic 损坏即停（表损坏时不猜）。
func ParsePartitions(data []byte) ([]Partition, error) {
	var out []Partition
	for off := 0; off+entrySize <= len(data); off += entrySize {
		magic := binary.LittleEndian.Uint16(data[off : off+2])
		if magic == magicMD5 || magic == magicErased {
			break
		}
		if magic != magicPart {
			return out, fmt.Errorf("分区表条目 %d magic 异常 (0x%04X)——表可能损坏", off/entrySize, magic)
		}
		p := Partition{
			Type:    data[off+2],
			SubType: data[off+3],
			Offset:  binary.LittleEndian.Uint32(data[off+4 : off+8]),
			Size:    binary.LittleEndian.Uint32(data[off+8 : off+12]),
			Flags:   binary.LittleEndian.Uint32(data[off+28 : off+32]),
		}
		p.Label = trimNUL(data[off+12 : off+28])
		out = append(out, p)
	}
	return out, nil
}

func trimNUL(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// FormatPartitions: 分区表的文本渲染（CLI 输出与事件审计共用）。
func FormatPartitions(parts []Partition) []string {
	out := []string{fmt.Sprintf("%-4s %-4s %-6s %-10s %-10s %-16s", "TYPE", "SUB", "FLAGS", "OFFSET", "SIZE", "LABEL")}
	for _, p := range parts {
		out = append(out, fmt.Sprintf("%-4s 0x%02X 0x%04X  0x%08X 0x%08X %-16s",
			p.TypeName(), p.SubType, p.Flags, p.Offset, p.Size, p.Label))
	}
	return out
}
