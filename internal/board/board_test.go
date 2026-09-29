package board

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("dump 产物权限必须 0600: %v", fi.Mode())
	}
}

func TestPlanValidation(t *testing.T) {
	// info / partitions：argv 形状
	argv, err := Plan("ttyX", Spec{Action: ActionInfo, Chip: "esp32s3", Baud: 460800})
	if err != nil || argv[0] != "--port" || argv[1] != "ttyX" {
		t.Fatalf("info argv 异常: %v %v", argv, err)
	}
	if got := argv[len(argv)-1]; got != "flash_id" {
		t.Fatalf("info 应以 flash_id 收尾: %v", argv)
	}
	if _, err := Plan("ttyX", Spec{Action: ActionPartitions}); err != nil {
		t.Fatalf("partitions plan: %v", err)
	}
	// dump：缺输出 / 坏地址 / 坏长度
	if _, err := Plan("ttyX", Spec{Action: ActionDump, Addr: "0x1000", Size: "0x10"}); err == nil {
		t.Fatal("dump 缺 --out 必须报错")
	}
	if _, err := Plan("ttyX", Spec{Action: ActionDump, Addr: "zz", Size: "0x10", OutPath: "f"}); err == nil {
		t.Fatal("坏 addr 必须报错")
	}
	if _, err := Plan("ttyX", Spec{Action: ActionDump, Addr: "0x10", Size: "", OutPath: "f"}); err == nil {
		t.Fatal("空 size 必须报错")
	}
	if _, err := Plan("ttyX", Spec{Action: "explode"}); err == nil {
		t.Fatal("未知操作必须报错")
	}
}

func TestParseNVSNumericTypes(t *testing.T) {
	pg := make([]byte, nvsPageSize)
	binary.LittleEndian.PutUint32(pg[0:4], 0xFFFFFFFC)
	off := nvsEntryStart
	put := func(b ...[]byte) {
		for _, x := range b {
			copy(pg[off:], x)
			off += len(x)
		}
	}
	put(nvsEntry(0, typeU8, "num", "", []byte{7, 0, 0, 0, 0, 0, 0, 0}))         // 命名空间 num=7
	put(nvsEntry(7, typeI8, "i8v", "", []byte{0xFE, 0, 0, 0, 0, 0, 0, 0}))      // -2
	put(nvsEntry(7, typeU16, "u16v", "", []byte{0x34, 0x12, 0, 0, 0, 0, 0, 0})) // 0x1234
	put(nvsEntry(7, typeI16, "i16v", "", []byte{0xD4, 0xFE, 0, 0, 0, 0, 0, 0})) // -300
	put(nvsEntry(7, typeU32, "u32v", "", []byte{0x78, 0x56, 0x34, 0x12, 0, 0, 0, 0}))
	put(nvsEntry(7, typeI32, "i32v", "", []byte{0x00, 0x00, 0x00, 0x80, 0, 0, 0, 0})) // -2^31
	put(nvsEntry(7, typeU64, "u64v", "", []byte{1, 0, 0, 0, 0, 0, 0, 0}))
	put(nvsEntry(7, typeI64, "i64v", "", []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})) // -1

	byKey := map[string]Entry{}
	for _, e := range ParseNVS(pg, false) {
		byKey[e.Key] = e
	}
	want := map[string]string{
		"i8v": "-2", "u16v": "4660", "i16v": "-300",
		"u32v": "305419896", "i32v": "-2147483648", "u64v": "1", "i64v": "-1",
	}
	for k, v := range want {
		if e := byKey[k]; e.Value != v {
			t.Errorf("%s = %q, want %q", k, e.Value, v)
		}
	}
	if e := byKey["u64v"]; e.Namespace != "num" {
		t.Errorf("命名空间解析: %+v", e)
	}
}

func TestParseNVSEdges(t *testing.T) {
	// 全擦除页：无输出
	erased := make([]byte, 2*nvsPageSize)
	if got := ParseNVS(erased, false); len(got) != 0 {
		t.Fatalf("擦除镜像应无条目: %+v", got)
	}
	// 未知类型 / FormatNVS 截断
	pg := make([]byte, nvsPageSize)
	binary.LittleEndian.PutUint32(pg[0:4], 0xFFFFFFFC)
	e := nvsEntry(1, 0x77, "weird", "", nil)
	copy(pg[nvsEntryStart:], e)
	entries := ParseNVS(pg, false)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Value, "<type 0x77>") || entries[0].Type != "0x77" {
		t.Fatalf("未知类型渲染: %+v", entries)
	}
	// Find 未命中
	if _, ok := Find([]Partition{}, "nope"); ok {
		t.Fatal("Find 未命中应返回 false")
	}
}

// TestEsptoolReaderWithFakeTool: 假 esptool 脚本按地址参数回吐不同镜像，
// 覆盖 EsptoolReader → RunExec → 临时文件全链路（含 info 分支的流式输出）。
func TestEsptoolReaderWithFakeTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh 假工具仅 POSIX；Windows 侧由 esptool.exe 真机验收覆盖")
	}
	dir := t.TempDir()
	tbl := partEntry(1, 0x02, 0x9000, 0x1000, "nvs")
	pad := make([]byte, PartTableSize-32)
	for i := range pad {
		pad[i] = 0xFF
	}
	if err := os.WriteFile(filepath.Join(dir, "table.bin"), append(tbl, pad...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nvs.bin"), synthNVS(), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-esptool")
	body := "#!/bin/sh\n" +
		"echo 'fake esptool running'\n" +
		"for last; do :; done\n" +
		"case \"$5\" in 0x8000) cat '" + filepath.Join(dir, "table.bin") + "' > \"$last\";;" +
		" *) cat '" + filepath.Join(dir, "nvs.bin") + "' > \"$last\";; esac\n" +
		"echo 'Hash of data verified (fake)'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := Spec{Action: ActionNVS, Esptool: script}
	var lines []string
	collect := func(l string) { lines = append(lines, l) }
	read := EsptoolReader("ttyFAKE", spec, 0, collect)
	data, err := read(PartTableOffset, PartTableSize)
	if err != nil || len(data) != PartTableSize {
		t.Fatalf("reader 表读取失败: %v %d", err, len(data))
	}
	nvsData, err := read(0x9000, 0x1000)
	if err != nil {
		t.Fatalf("nvs 读取失败: %v", err)
	}
	entries := ParseNVS(nvsData, false)
	if len(entries) == 0 {
		t.Fatal("假工具链路解析不出条目")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "fake esptool running") {
		t.Fatalf("esptool 输出未流式回传: %s", joined)
	}

	// info 分支：假工具回显 → RunExec 流式路径
	lines = nil
	if err := Exec("ttyFAKE", Spec{Action: ActionInfo, Esptool: script}, 0, collect, read); err != nil {
		t.Fatalf("info exec: %v", err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "fake esptool running") {
		t.Fatalf("info 输出缺失: %v", lines)
	}
}
