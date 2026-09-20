package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PauseState: USB 刷写纪律（esptool 与常驻采集器抢同一串口会把 ESP32
// 楔进 ROM 下载模式）。root/PAUSED 每行一个正则，匹配 tty/key/name/by-id 任一
// 即暂停该设备：采集器主动关口并等待，直到模式移除才重新 open。
type PauseState struct {
	mu   sync.RWMutex
	pats []*regexp.Regexp
}

func PauseFilePath(root string) string { return filepath.Join(root, "PAUSED") }

func NewPauseState() *PauseState { return &PauseState{} }

// Matches: 该设备当前是否被暂停。
func (p *PauseState) Matches(dev DeviceInfo) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.pats) == 0 {
		return false
	}
	for _, re := range p.pats {
		for _, f := range []string{dev.Tty, dev.Key, dev.Name, dev.ByID} {
			if f != "" && re.MatchString(f) {
				return true
			}
		}
	}
	return false
}

// LoadPauseFile: 读取 root/PAUSED（不存在 = 无暂停）。坏正则跳过。
// 返回文件 mtime 供轮询方检测变更。
func LoadPauseFile(root string) (*PauseState, time.Time, error) {
	st := NewPauseState()
	path := PauseFilePath(root)
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, time.Time{}, nil
		}
		return st, time.Time{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st, fi.ModTime(), err
	}
	var good []*regexp.Regexp
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if re, err := regexp.Compile(ln); err == nil {
			good = append(good, re)
		}
	}
	st.pats = good
	return st, fi.ModTime(), nil
}

// PauseCLI: serialtap pause/resume 的文件编辑端。pattern 省略 = 全部。
func PauseCLI(root string, pause bool, patterns []string) error {
	path := PauseFilePath(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		for _, ln := range strings.Split(string(data), "\n") {
			if ln = strings.TrimSpace(ln); ln != "" && !strings.HasPrefix(ln, "#") {
				lines = append(lines, ln)
			}
		}
	}
	if pause {
		if len(patterns) == 0 {
			patterns = []string{".*"}
		}
		lines = append(lines, patterns...)
	} else {
		if len(patterns) == 0 {
			lines = nil // 清空
		} else {
			var keep []string
			for _, ln := range lines {
				drop := false
				for _, pat := range patterns {
					if ln == pat {
						drop = true
						break
					}
				}
				if !drop {
					keep = append(keep, ln)
				}
			}
			lines = keep
		}
	}
	if len(lines) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			err = nil
		}
		if err != nil {
			return err
		}
		fmt.Println("采集已全部恢复（PAUSED 已移除）")
		return nil
	}
	header := "# serialtap 暂停清单 — 每行一个正则（匹配 tty/by-path/by-id/设备名）\n"
	if err := os.WriteFile(path, []byte(header+strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("已写入 %s（%d 条模式）：\n", path, len(lines))
	for _, ln := range lines {
		fmt.Println("  " + ln)
	}
	return nil
}
