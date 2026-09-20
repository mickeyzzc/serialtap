package flash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeEsptool(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-esptool")
	os.WriteFile(p, []byte(script), 0o755)
	return p
}

func TestBuildArgsManualBins(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "app.bin")
	os.WriteFile(bin, []byte("x"), 0o644)

	args, err := BuildArgs("/dev/ttyFAKE0", Spec{
		Bins: []BinSpec{{Path: bin, Offset: "0x10000"}, {Path: bin, Offset: "0x0"}},
		Baud: 921600, Chip: "esp32s3",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--port /dev/ttyFAKE0", "--chip esp32s3", "--baud 921600",
		"write_flash", "0x10000 " + bin, "0x0 " + bin} {
		if !strings.Contains(joined, want) {
			t.Fatalf("参数缺 %q: %s", want, joined)
		}
	}
}

func TestBuildArgsValidation(t *testing.T) {
	if _, err := BuildArgs("/dev/x", Spec{}); err == nil {
		t.Fatal("空 bins 应报错")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "a.bin")
	os.WriteFile(bin, []byte("x"), 0o644)
	if _, err := BuildArgs("/dev/x", Spec{Bins: []BinSpec{{Path: bin, Offset: "not-hex"}}}); err == nil {
		t.Fatal("坏偏移应报错")
	}
	if _, err := BuildArgs("/dev/x", Spec{Bins: []BinSpec{{Path: "/nope.bin", Offset: "0x0"}}}); err == nil {
		t.Fatal("缺镜像应报错")
	}
}

func TestParseFlasherArgs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bootloader.bin"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.bin"), []byte("a"), 0o644)
	fa := filepath.Join(dir, "flasher_args.json")
	os.WriteFile(fa, []byte(`{
		"extra_esptool_args": {"--chip": "esp32s3", "--baud": "921600"},
		"flash_files": {"0x10000": "app.bin", "0x0": "bootloader.bin"}
	}`), 0o644)

	args, err := BuildArgs("/dev/ttyFAKE0", Spec{ArgsFile: fa})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	// 按 offset 排序：0x0 在前
	if !strings.Contains(joined, "write_flash 0x0 "+filepath.Join(dir, "bootloader.bin")+" 0x10000 "+filepath.Join(dir, "app.bin")) {
		t.Fatalf("flasher_args 解析/排序错误: %s", joined)
	}
	if !strings.Contains(joined, "--chip esp32s3") {
		t.Fatalf("chip 未透传: %s", joined)
	}

	// 无 flash_files → 报错
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"extra_esptool_args":{}}`), 0o644)
	if _, err := BuildArgs("/dev/x", Spec{ArgsFile: bad}); err == nil {
		t.Fatal("无 flash_files 应报错")
	}
	if _, err := BuildArgs("/dev/x", Spec{ArgsFile: filepath.Join(dir, "nope.json")}); err == nil {
		t.Fatal("缺 args 文件应报错")
	}
}

func TestResolveEsptool(t *testing.T) {
	fake := writeFakeEsptool(t, "#!/bin/sh\nexit 0\n")
	if p, err := ResolveEsptool(fake); err != nil || !strings.HasSuffix(p, "fake-esptool") {
		t.Fatalf("显式指定失败: %q %v", p, err)
	}
	if _, err := ResolveEsptool("/nonexistent/e"); err == nil {
		t.Fatal("无效路径应报错")
	}
}

func TestRunStreamsOutputAndExitCode(t *testing.T) {
	fake := writeFakeEsptool(t, `#!/bin/sh
echo "esptool@1 chip is ESP32-S3"
printf 'Writing at 0x0000... (25 %%)\r'
printf 'Writing at 0x1000... (50 %%)\r'
echo "Hash of data verified."
exit 0
`)
	var lines []string
	if err := Run(fake, "/dev/ttyFAKE0", Spec{Bins: []BinSpec{{Path: "/dev/null", Offset: "0x0"}}},
		func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"chip is ESP32-S3", "(25 %)", "(50 %)", "Hash of data verified"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("输出缺 %q: %q", want, joined)
		}
	}

	fail := writeFakeEsptool(t, "#!/bin/sh\necho boom\nexit 2\n")
	err := Run(fail, "/dev/ttyFAKE0", Spec{Bins: []BinSpec{{Path: "/dev/null", Offset: "0x0"}}},
		func(string) {})
	if err == nil || !strings.Contains(err.Error(), "2") {
		t.Fatalf("非零退出码应报错: %v", err)
	}
}

func TestSplitCRLF(t *testing.T) {
	advance, token, err := splitCRLF([]byte("a\rb\nc"), false)
	if err != nil || string(token) != "a" || advance != 2 {
		t.Fatalf("splitCRLF 错误: %d %q %v", advance, token, err)
	}
}
