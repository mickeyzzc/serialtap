package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const tsFormat = "2006-01-02 15:04:05.000"

func stamp(t time.Time) string { return t.Format(tsFormat) }

// SanitizeName: 设备目录名只留安全字符。
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "dev"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// DeviceWriter: 单设备双文件写入器。
// <root>/<name>/serial-YYYYMMDD.log  全量日志（每行带毫秒时间戳）
// <root>/<name>/events-YYYYMMDD.log  事件流（签名命中 + 采集器生命周期标记）
// 轮转：日期变更开新文件；serial 超过 RotateMB 加 .001/.002 后缀继续。
type DeviceWriter struct {
	root     string
	name     string
	maxBytes int64

	mu         sync.Mutex
	day        string
	serialF    *os.File
	eventsF    *os.File
	serialSize int64
	suffix     int
}

func NewDeviceWriter(root, name string, maxMB int) (*DeviceWriter, error) {
	name = SanitizeName(name)
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DeviceWriter{root: root, name: name, maxBytes: int64(maxMB) * 1024 * 1024}, nil
}

func (w *DeviceWriter) Dir() string  { return filepath.Join(w.root, w.name) }
func (w *DeviceWriter) Name() string { return w.name }

func (w *DeviceWriter) filePath(kind, day string, suffix int) string {
	base := fmt.Sprintf("%s-%s", kind, day)
	if suffix > 0 {
		base += fmt.Sprintf(".%03d", suffix)
	}
	return filepath.Join(w.root, w.name, base+".log")
}

func (w *DeviceWriter) ensureDayLocked(now time.Time) error {
	day := now.Format("20060102")
	if w.day == day && w.serialF != nil {
		return nil
	}
	if w.serialF != nil {
		_ = w.serialF.Close()
		w.serialF = nil
	}
	if w.eventsF != nil {
		_ = w.eventsF.Close()
		w.eventsF = nil
	}
	sf, err := os.OpenFile(w.filePath("serial", day, 0), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	ef, err := os.OpenFile(w.filePath("events", day, 0), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		_ = sf.Close()
		return err
	}
	if fi, err := sf.Stat(); err == nil {
		w.serialSize = fi.Size()
	} else {
		w.serialSize = 0
	}
	w.day, w.serialF, w.eventsF, w.suffix = day, sf, ef, 0
	return nil
}

// WriteLine: 写一行全量日志（内部加时间戳）。
func (w *DeviceWriter) WriteLine(line string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if err := w.ensureDayLocked(now); err != nil {
		return err
	}
	if w.maxBytes > 0 && w.serialSize >= w.maxBytes {
		_ = w.serialF.Close()
		w.suffix++
		nf, err := os.OpenFile(w.filePath("serial", w.day, w.suffix), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			w.serialF = nil
			return err
		}
		w.serialF = nf
		w.serialSize = 0
	}
	n, err := fmt.Fprintf(w.serialF, "[%s] %s\n", stamp(now), line)
	w.serialSize += int64(n)
	return err
}

// WriteEvent: 写一行事件流（调用方已格式化好内容）。
func (w *DeviceWriter) WriteEvent(line string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if err := w.ensureDayLocked(now); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w.eventsF, "[%s] %s\n", stamp(now), line)
	return err
}

func (w *DeviceWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.serialF != nil {
		_ = w.serialF.Close()
		w.serialF = nil
	}
	if w.eventsF != nil {
		_ = w.eventsF.Close()
		w.eventsF = nil
	}
}

// SweepRetention: 删除 mtime 早于 N 天前的 <root>/*/*.log。
func SweepRetention(root string, days int) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".log") {
				continue
			}
			info, err := f.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, f.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}
