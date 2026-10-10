package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写一个设备日志文件（当日），返回其路径。
func writeSelftestLog(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanDeviceSelftestFindsLatestBootCluster(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "fake-cam")
	writeSelftestLog(t, dev, "serial-20261009.log", strings.Join([]string{
		"[09:00:00.000] I (100) wifi_manager: SELFTEST: wifi aps=3 top=\"-26dBm ch1 A\" ours=\"A=-26dBm\"",
		"[09:00:01.000] I (200) main: SELFTEST: board=fake-cam fw=v0.1 chip=esp32s3 psram=8MB sensor=OV2640/0x0026 heap=8101KB",
		"…（几十万行噪声，不构成同组）…",
	}, "\n"))
	// 今天（更新）一次开机：wifi 行 + 整机行相邻；带 ANSI 色彩码模拟串口直通
	writeSelftestLog(t, dev, "serial-20261010.log", strings.Join([]string{
		"[10:00:00.000] noise line 1",
		"[10:00:02.000] I (4141) wifi_manager: \x1b[0;32mSELFTEST: wifi aps=0 top=\"-\" ours=\"A=-128dBm\"\x1b[0m",
		"[10:00:02.500] I (4745) main: \x1b[0;32mSELFTEST: board=fake-cam fw=v0.1 chip=esp32s3 psram=0MB sensor=none heap=100KB\x1b[0m",
		"[10:00:03.000] noise line 2",
	}, "\n"))

	entry := scanDeviceSelftest(dev, 14)
	if entry.Missing {
		t.Fatal("应找到自检行")
	}
	if len(entry.Lines) != 2 {
		t.Fatalf("应聚合同一开机的 2 行，得到 %d", len(entry.Lines))
	}
	if !strings.Contains(stripANSI(entry.Lines[0]), "aps=0") {
		t.Fatalf("应取到最新一日（aps=0），实际: %s", entry.Lines[0])
	}

	warns := selftestWarnings(entry.Lines)
	joined := strings.Join(warns, "; ")
	for _, want := range []string{"aps=0", "sensor=none", "psram=0MB", "-128dBm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("告警应包含 %q，实际: %v", want, warns)
		}
	}
}

func TestScanDeviceSelftestClusterGap(t *testing.T) {
	// 两次开机相隔很远（>cluster 行），只应取最近一组
	root := t.TempDir()
	dev := filepath.Join(root, "fake2")
	var lines []string
	lines = append(lines, "[09:00:00.000] I (100) m: SELFTEST: board=old-boot sensor=OV5640/0x5640")
	for i := 0; i < selftestCluster+10; i++ {
		lines = append(lines, "noise")
	}
	lines = append(lines, "[10:00:00.000] I (100) m: SELFTEST: board=new-boot sensor=OV2640/0x0026")
	writeSelftestLog(t, dev, "serial-20261010.log", strings.Join(lines, "\n"))

	entry := scanDeviceSelftest(dev, 14)
	if entry.Missing || len(entry.Lines) != 1 || !strings.Contains(entry.Lines[0], "new-boot") {
		t.Fatalf("应只取最近一组，实际 Missing=%v Lines=%v", entry.Missing, entry.Lines)
	}
}

func TestScanDeviceSelftestMissing(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "empty-dev")
	writeSelftestLog(t, dev, "serial-20261010.log", "no selftest here\n")
	writeSelftestLog(t, dev, "events-20261010.log", "event\n")

	entry := scanDeviceSelftest(dev, 14)
	if !entry.Missing {
		t.Fatalf("无自检行应 Missing=true, got %+v", entry)
	}
}
