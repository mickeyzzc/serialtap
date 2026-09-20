package flash

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mickeyzzc/serialtap/internal/testutil"
)

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
	fake := testutil.FakeTool(t)
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "")
	if p, err := ResolveEsptool(fake); err != nil || p != fake {
		t.Fatalf("显式指定失败: %q %v", p, err)
	}
	if _, err := ResolveEsptool("/nonexistent/e"); err == nil {
		t.Fatal("无效路径应报错")
	}
}

func TestRunStreamsOutputAndExitCode(t *testing.T) {
	fake := testutil.FakeTool(t)
	// 镜像文件要真实存在（BuildArgs 会 Stat；/dev/null 在 Windows 没有）
	dir := t.TempDir()
	bin := filepath.Join(dir, "x.bin")
	os.WriteFile(bin, []byte("x"), 0o644)

	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "esptool@1 chip is ESP32-S3\n"+
		"Writing at 0x0000... (25 %)\r"+
		"Writing at 0x1000... (50 %)\r"+
		"Hash of data verified.\n")
	var lines []string
	if err := Run(fake, "/dev/ttyFAKE0", Spec{Bins: []BinSpec{{Path: bin, Offset: "0x0"}}}, 0,
		func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"chip is ESP32-S3", "(25 %)", "(50 %)", "Hash of data verified"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("输出缺 %q: %q", want, joined)
		}
	}

	t.Setenv("FAKE_EXIT", "2")
	t.Setenv("FAKE_OUT", "boom\n")
	err := Run(fake, "/dev/ttyFAKE0", Spec{Bins: []BinSpec{{Path: bin, Offset: "0x0"}}}, 0,
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

// 真实 IDF flasher_args.json 形状：extra_esptool_args 混有 bool/number
func TestParseFlasherArgsHeterogeneousValues(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.bin"), []byte("a"), 0o644)
	fa := filepath.Join(dir, "flasher_args.json")
	os.WriteFile(fa, []byte(`{
		"extra_esptool_args": {"--chip": "esp32s3", "--baud": "921600", "stub": true, "trace": 0},
		"flash_files": {"0x0": "app.bin"}
	}`), 0o644)
	args, err := BuildArgs("/dev/x", Spec{ArgsFile: fa})
	if err != nil {
		t.Fatalf("异构 extra_esptool_args 解析失败: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "--chip esp32s3") {
		t.Fatalf("chip 未提取: %v", args)
	}
}

// —— esptool 自动发现（PATH 之外的 glob 途径）——

// 把 FakeTool 的二进制复制到假 python_env 树的 esptool 位置
func plantFakeEsptool(t *testing.T, envRoot string) {
	t.Helper()
	sub := "bin"
	name := "esptool"
	if runtime.GOOS == "windows" {
		sub, name = "Scripts", "esptool.exe"
	}
	dir := filepath.Join(envRoot, "python_env", "esp5.3_test_env", sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(testutil.FakeTool(t))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveEsptoolEspressifEnvGlob(t *testing.T) {
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "")
	env := t.TempDir()
	plantFakeEsptool(t, env)

	// 直接测 glob 注入口（ResolveEsptool 全链路的 home 不可注入）
	p, ok := globLast(esptoolEnvGlobs(env))
	if !ok || !strings.Contains(filepath.ToSlash(p), "/python_env/esp5.3_test_env/") {
		t.Fatalf("python_env glob 未命中: %q ok=%v", p, ok)
	}
	// 空 glob 不命中
	if _, ok := globLast([]string{filepath.Join(env, "nope", "*", "x")}); ok {
		t.Fatal("空 glob 不应命中")
	}
}

// 多个 python_env 命中时取排序后最后一个（与 addr2line 策略一致）
func TestGlobLastTakesHighestSorted(t *testing.T) {
	env := t.TempDir()
	plantFakeEsptool(t, env) // esp5.3_test_env
	// 再放一个排序更小的 env
	older := "esp5.0_env"
	sub, name := "bin", "esptool"
	if runtime.GOOS == "windows" {
		sub, name = "Scripts", "esptool.exe"
	}
	os.MkdirAll(filepath.Join(env, "python_env", older, sub), 0o755)
	os.WriteFile(filepath.Join(env, "python_env", older, sub, name), []byte("x"), 0o755)

	p, ok := globLast(esptoolEnvGlobs(env))
	if !ok || !strings.Contains(p, "esp5.3_test_env") {
		t.Fatalf("应取排序最后的 env: %q", p)
	}
}

func TestPlan(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "app.bin")
	os.WriteFile(bin, []byte("x"), 0o644)
	tool := testutil.FakeTool(t)

	argv, err := Plan("/dev/ttyFAKE0", Spec{Esptool: tool,
		Bins: []BinSpec{{Path: bin, Offset: "0x10000"}}, Chip: "esp32s3"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--port /dev/ttyFAKE0", "--chip esp32s3", "write_flash", "0x10000"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Plan 缺 %q: %s", want, joined)
		}
	}
	// 坏 Spec（无镜像）应报错
	if _, err := Plan("/dev/x", Spec{Esptool: tool}); err == nil {
		t.Fatal("空 bins 应报错")
	}
}

func TestRunTimeoutKillsEsptool(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "x.bin")
	os.WriteFile(bin, []byte("x"), 0o644)
	t.Setenv("FAKE_SLEEP", "3s")
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "")
	start := time.Now()
	err := Run(testutil.FakeTool(t), "/dev/ttyFAKE0",
		Spec{Bins: []BinSpec{{Path: bin, Offset: "0x0"}}}, 300*time.Millisecond, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时应杀进程并报错: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未及时返回: %s", elapsed)
	}
}

// eim 布局：tool_install_folder_name 指向别处（如 C:\Espressif\tools），
// python_env 在 tools 同级 —— 解析该行并返回候选根
func TestEimRootsAt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "eim_config.toml"),
		[]byte("path = 'C:\\Users\\x\\.espressif'\ntool_install_folder_name = 'C:\\Espressif\\tools'\n"+
			"python_env_folder_name = \"python\"\n"), 0o644)
	got := eimRootsAt(dir)
	if len(got) == 0 || got[0] != filepath.Dir(`C:\Espressif\tools`) {
		t.Fatalf("eim 根解析错误: %v", got)
	}
	if r := eimRootsAt(filepath.Join(dir, "nope")); r != nil {
		t.Fatal("无配置文件应返回 nil")
	}
	// 无 tool_install_folder_name 行 → nil
	os.WriteFile(filepath.Join(dir, "eim_config.toml"), []byte("a = 'b'\n"), 0o644)
	if r := eimRootsAt(dir); r != nil {
		t.Fatal("无目标行应返回 nil")
	}
}
