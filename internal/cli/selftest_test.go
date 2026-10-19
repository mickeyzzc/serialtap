package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickeyzzc/serialtap/internal/logstore"
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

// —— issue #62：--files ≤ 0 不再 panic（files[:N] 越界），钳到 1 ——
func TestRunSelftestFilesClamp(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "clamp-dev")
	writeSelftestLog(t, dev, "serial-20261010.log",
		"[10:00:00.000] m: SELFTEST: board=clamp-dev fw=v0.1 chip=esp32s3\n")

	for _, flagVal := range []string{"--files=0", "--files=-3"} {
		old := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		code := runSelftest([]string{"--root", root, flagVal})
		w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		if code != 0 || !strings.Contains(string(out), "clamp-dev") {
			t.Fatalf("%s: code=%d out=%s", flagVal, code, out)
		}
	}
}

// —— issue #53：扫描正则必须与 logstore 实际产物命名一致 ——
// 契约：logstore.FileName 生成的文件名（基础 + 大小轮转），selftest 的
// serialFileRe 都要能解析出日期/后缀；任一侧改名本测试即红。
func TestSelftestFileRegexMatchesLogstoreNaming(t *testing.T) {
	for _, sfx := range []int{0, 1, 7, 42, 999} {
		name := logstore.FileName("serial", "20260101", sfx)
		m := serialFileRe.FindStringSubmatch(name)
		if m == nil {
			t.Fatalf("正则不匹配 logstore 产物 %q", name)
		}
		if m[1] != "20260101" {
			t.Fatalf("日期组错误: %v", m)
		}
		want := "" // 基础文件无可选后缀组
		if sfx > 0 {
			want = fmt.Sprintf("%03d", sfx)
		}
		if m[2] != want {
			t.Fatalf("后缀组错误: %v（want %s）", m, want)
		}
	}
	// events 通道与"后缀在 .log 后"的旧误命名不得混入
	if serialFileRe.MatchString(logstore.FileName("events", "20260101", 0)) {
		t.Fatal("events 通道不应被 serial 扫描命中")
	}
	if serialFileRe.MatchString("serial-20260101.log.001") {
		t.Fatal(".log.NNN 不是 logstore 命名，不应匹配")
	}
}

// 轮转文件（serial-YYYYMMDD.NNN.log）同样可被检索，且同日取最新后缀。
func TestScanDeviceSelftestRotatedFiles(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "rot-dev")
	writeSelftestLog(t, dev, logstore.FileName("serial", "20260101", 0),
		"[09:00:00.000] m: SELFTEST: board=rot-old sensor=deferred\n")
	writeSelftestLog(t, dev, logstore.FileName("serial", "20260101", 7),
		"[11:00:00.000] m: SELFTEST: board=rot-new sensor=OV5640/0x5640\n")

	entry := scanDeviceSelftest(dev, 14)
	if entry.Missing || len(entry.Lines) != 1 || !strings.Contains(entry.Lines[0], "rot-new") {
		t.Fatalf("应取到同日最新轮转（.007）: Missing=%v Lines=%v", entry.Missing, entry.Lines)
	}
}

func TestRunSelftestCLI(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "dev-cli")
	writeSelftestLog(t, dev, "serial-20261010.log", strings.Join([]string{
		"[10:00:00.000] noise",
		"[10:00:02.000] I (100) main: SELFTEST: board=dev-cli fw=v0.1 chip=esp32s3 psram=none sensor=none heap=100KB",
		"[10:00:03.000] noise",
	}, "\n"))
	os.MkdirAll(filepath.Join(root, "dev-empty"), 0o755)
	writeSelftestLog(t, filepath.Join(root, "dev-empty"), "serial-20261010.log", "nothing\n")

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := runSelftest([]string{"--root", root})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	s := string(out)
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, s)
	}
	for _, want := range []string{"dev-cli", "SELFTEST: board=dev-cli", "dev-empty", "未见自检行", "1 台有自检行, 1 台未见", "传感器未检出"} {
		if !strings.Contains(s, want) {
			t.Errorf("输出应包含 %q\n%s", want, s)
		}
	}
}
