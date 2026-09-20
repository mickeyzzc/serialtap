//go:build !windows

package flash

import (
	"path/filepath"
)

// esptoolEnvGlobs: ESP-IDF python_env 里的 esptool（Linux/macOS：venv 的 bin/）。
// root 为 .espressif 目录（生产取 $IDF_TOOLS_PATH 或 ~/.espressif，测试可注入）。
func esptoolEnvGlobs(root string) []string {
	return []string{filepath.Join(root, "python_env", "*", "bin", "esptool")}
}

// esptoolUserGlobs: 平台特有的额外搜索路径（无则为空）。
func esptoolUserGlobs() []string {
	return nil
}
