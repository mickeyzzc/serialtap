package board

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// —— 合成 fixture 构造（严禁使用真机 NVS 镜像：内含明文凭据） ——

func partEntry(typ, sub byte, off, size uint32, label string) []byte {
	e := make([]byte, 32)
	binary.LittleEndian.PutUint16(e[0:2], magicPart)
	e[2], e[3] = typ, sub
	binary.LittleEndian.PutUint32(e[4:8], off)
	binary.LittleEndian.PutUint32(e[8:12], size)
	copy(e[12:28], label)
	return e
}

func md5Tail() []byte {
	e := make([]byte, 32)
	binary.LittleEndian.PutUint16(e[0:2], magicMD5)
	return e
}

func TestParsePartitions(t *testing.T) {
	var tbl []byte
	tbl = append(tbl, partEntry(1, 0x02, 0x9000, 0x6000, "nvs")...)
	tbl = append(tbl, partEntry(1, 0x00, 0xf000, 0x2000, "otadata")...)
	tbl = append(tbl, partEntry(0, 0x10, 0x10000, 0x500000, "ota_0")...)
	tbl = append(tbl, md5Tail()...)
	pad := make([]byte, PartTableSize-len(tbl))
	for i := range pad {
		pad[i] = 0xFF
	}
	tbl = append(tbl, pad...)

	parts, err := ParsePartitions(tbl)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("条目数 = %d, want 3", len(parts))
	}
	nvs, ok := Find(parts, "nvs")
	if !ok || nvs.Offset != 0x9000 || nvs.Size != 0x6000 || nvs.TypeName() != "data" {
		t.Fatalf("nvs 分区解析错误: %+v", nvs)
	}
	if parts[2].TypeName() != "app" {
		t.Fatalf("ota_0 类型应为 app")
	}
	lines := FormatPartitions(parts)
	if len(lines) != 4 || !strings.Contains(lines[1], "nvs") {
		t.Fatalf("渲染异常: %v", lines)
	}
}

func TestParsePartitionsCorruptMagic(t *testing.T) {
	tbl := make([]byte, 64) // 全零：第一条 magic=0 → 应报错
	if _, err := ParsePartitions(tbl); err == nil {
		t.Fatal("magic 异常必须报错，不能静默吞掉")
	}
}

// nvsEntry: 造一条 NVS 条目。字符串类型值放条目头后的槽位（span=2）。
func nvsEntry(ns byte, typ uint8, key, strVal string, inline []byte) []byte {
	e := make([]byte, nvsEntrySize)
	e[0], e[1] = ns, typ
	copy(e[8:23], key)
	switch typ {
	case typeString:
		e[2] = 2 // span：头 + 1 数据槽
		binary.LittleEndian.PutUint16(e[24:26], uint16(len(strVal)+1))
	case typeBlobIndex:
		binary.LittleEndian.PutUint32(e[24:28], binary.LittleEndian.Uint32(inline))
	default:
		copy(e[24:32], inline)
	}
	return e
}

func strSlot(v string) []byte {
	b := make([]byte, nvsEntrySize)
	copy(b, v)
	return b
}

// synthNVS: 一页合成 NVS：命名空间 testcfg + 布尔/数值/字符串/凭据字符串/blob。
func synthNVS() []byte {
	pg := make([]byte, nvsPageSize)
	binary.LittleEndian.PutUint32(pg[0:4], 0xFFFFFFFC) // active 页状态
	off := nvsEntryStart
	put := func(b ...[]byte) {
		for _, x := range b {
			copy(pg[off:], x)
			off += len(x)
		}
	}
	// 命名空间定义（真机格式）：ns=0 + u8，data[0]=分配索引 4
	put(nvsEntry(0, typeU8, "testcfg", "", []byte{4, 0, 0, 0, 0, 0, 0, 0}))
	put(nvsEntry(4, typeU8, "cam_fps", "", []byte{12, 0, 0, 0, 0, 0, 0, 0}))
	put(nvsEntry(4, typeString, "device_name", "synth-cam-01", nil), strSlot("synth-cam-01\x00"))
	put(nvsEntry(4, typeString, "wifi_pass", "hunter2-secret", nil), strSlot("hunter2-secret\x00"))
	put(nvsEntry(4, typeString, "wifi_ssid", "testnet", nil), strSlot("testnet\x00"))
	put(nvsEntry(4, typeBlobIndex, "cal_data", "", []byte{61, 0, 0, 0, 3, 0, 0, 0}))
	return pg
}

func TestParseNVSMasking(t *testing.T) {
	entries := ParseNVS(synthNVS(), false)
	byKey := map[string]Entry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}
	if len(entries) != 5 { // 命名空间定义不计入
		t.Fatalf("条目数 = %d, want 5: %+v", len(entries), entries)
	}
	if e := byKey["cam_fps"]; e.Value != "12" || e.Type != "u8" || e.Namespace != "testcfg" {
		t.Fatalf("u8 解析错误: %+v", e)
	}
	if e := byKey["device_name"]; e.Value != "synth-cam-01" || e.Type != "str" {
		t.Fatalf("字符串解析错误: %+v", e)
	}
	if e := byKey["wifi_pass"]; e.Value != "****" {
		t.Fatalf("凭据键必须默认掩码: %+v", e)
	}
	if e := byKey["wifi_ssid"]; e.Value != "testnet" {
		t.Fatalf("SSID 非凭据键，不应掩码: %+v", e)
	}
	if e := byKey["cal_data"]; !strings.HasPrefix(e.Value, "<blob 61") {
		t.Fatalf("blob 尺寸解析错误: %+v", e)
	}

	// showSecrets=true：明文
	for _, e := range ParseNVS(synthNVS(), true) {
		if e.Key == "wifi_pass" && e.Value != "hunter2-secret" {
			t.Fatalf("--show-secrets 必须明文: %+v", e)
		}
	}
}

func TestExecNVSAndDump(t *testing.T) {
	tbl := partEntry(1, 0x02, 0x9000, 0x1000, "nvs")
	pad := make([]byte, PartTableSize-32)
	for i := range pad {
		pad[i] = 0xFF
	}
	table := append(tbl, pad...)
	nvsImg := synthNVS()
	read := func(addr, size uint32) ([]byte, error) {
		switch addr {
		case PartTableOffset:
			return table, nil
		case 0x9000:
			return nvsImg, nil
		}
		return nil, os.ErrNotExist
	}

	var lines []string
	collect := func(l string) { lines = append(lines, l) }

	if err := Exec("ttyTEST", Spec{Action: ActionNVS}, 0, collect, read); err != nil {
		t.Fatalf("nvs exec: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "wifi_pass") || !strings.Contains(joined, "****") {
		t.Fatalf("nvs 输出缺掩码凭据行: %s", joined)
	}

	// dump：写文件 0600
	out := filepath.Join(t.TempDir(), "dump.bin")
	lines = nil
	if err := Exec("ttyTEST", Spec{Action: ActionDump, Addr: "0x9000", Size: "0x1000", OutPath: out}, 0, collect, read); err != nil {
		t.Fatalf("dump exec: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Size() != 0x1000 {
		t.Fatalf("dump 产物异常: %v %v", fi, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("dump 产物权限必须 0600: %v", fi.Mode())
	}
}
