//go:build windows

package flash

import (
	"os"
	"path/filepath"
)

// esptoolEnvGlobs: ESP-IDF python_env 里的 esptool（Windows：venv 的 Scripts\）。
func esptoolEnvGlobs(root string) []string {
	return []string{filepath.Join(root, "python_env", "*", "Scripts", "esptool.exe")}
}

// esptoolUserGlobs: pip --user / python.org 安装的 Scripts 目录（常不在 PATH）。
func esptoolUserGlobs() []string {
	var pats []string
	if d := os.Getenv("APPDATA"); d != "" { // pip install --user
		pats = append(pats, filepath.Join(d, "Python", "*", "Scripts", "esptool.exe"))
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" { // python.org 安装器
		pats = append(pats, filepath.Join(d, "Programs", "Python", "*", "Scripts", "esptool.exe"))
	}
	return pats
}
