package daemon

import "testing"

func TestFlashMilestone(t *testing.T) {
	seen := map[string]bool{}

	// 前缀组：每条都记录（esptool 每镜像一条，重复无害）
	if _, ok := flashMilestone("Chip is ESP32-C3 (revision 0.4)", seen); !ok {
		t.Fatal("Chip is 应为里程碑")
	}
	if _, ok := flashMilestone("Chip is ESP32-C3 (revision 0.5)", seen); !ok {
		t.Fatal("前缀组不应节流")
	}
	if _, ok := flashMilestone("Connecting...", seen); !ok {
		t.Fatal("Connecting 应为里程碑")
	}
	if _, ok := flashMilestone("Connecting....", seen); ok {
		t.Fatal("Connecting 重试应被节流")
	}

	// 写入进度：只记整十
	seen2 := map[string]bool{}
	if _, ok := flashMilestone("Writing at 0x00042000... (7 %)", seen2); ok {
		t.Fatal("非整十进度不应记录")
	}
	if _, ok := flashMilestone("Writing at 0x00042000... (10 %)", seen2); !ok {
		t.Fatal("整十进度应为里程碑")
	}
	if _, ok := flashMilestone("Writing at 0x00042000... (10 %)", seen2); ok {
		t.Fatal("同一整十进度重复应被节流")
	}
	if _, ok := flashMilestone("Writing at 0x00042000... (20 %)", seen2); !ok {
		t.Fatal("新的整十进度应为里程碑")
	}

	// 空行/噪声不记
	if _, ok := flashMilestone("", seen); ok {
		t.Fatal("空行不应记录")
	}
	if _, ok := flashMilestone("ordinary stdout noise", seen); ok {
		t.Fatal("普通行不应记录")
	}

	// 错误行必须记（小写匹配）
	if _, ok := flashMilestone("A fatal error occurred: Failed to connect", seen); !ok {
		t.Fatal("错误行应为里程碑")
	}

	// 每镜像一条的 Wrote/verified 全部记录（不被 Compressed 子串吞掉）
	seen3 := map[string]bool{}
	wrote := "Wrote 21152 bytes (13401 compressed) at 0x00000000 in 0.4 seconds"
	if _, ok := flashMilestone(wrote, seen3); !ok {
		t.Fatal("Wrote 行应为里程碑")
	}
	if _, ok := flashMilestone("Wrote 1404416 bytes (821711 compressed) at 0x00010000", seen3); !ok {
		t.Fatal("第二个镜像的 Wrote 行也应记录（不被 compressed 字样节流）")
	}
}
