// Package logstore 管理设备双通道日志：写入、按日/大小轮转、保留期清理。
package logstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const tsFormat = "2006-01-02 15:04:05.000"

// Stamp: 事件行统一的时间戳格式。
func Stamp(t time.Time) string { return t.Format(tsFormat) }

// DeviceWriter: 单设备双文件写入器。
// <root>/<name>/serial-YYYYMMDD.log  全量日志（每行带毫秒时间戳）
// <root>/<name>/events-YYYYMMDD.log  事件流（签名命中 + 采集器生命周期标记）
// 轮转：日期变更开新文件；serial 超过 RotateMB 加 .001/.002 后缀继续。
type DeviceWriter struct {
	root     string
	name     string
	maxBytes int64

	mu             sync.Mutex
	day            string
	serialF        *os.File
	eventsF        *os.File
	serialSize     int64
	eventsSize     int64
	serialSuffix   int
	eventsSuffix   int
	eventsMaxBytes int64 // events 通道大小轮转阈值（默认同 maxBytes）
}

func NewDeviceWriter(root, name string, maxMB int) (*DeviceWriter, error) {
	// 契约：name 需为目录安全名（由 device.SanitizeName 统一生成）
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

// highestSuffix: 某通道当日既有文件的最大轮转后缀（0 = 只有基础文件/无）。
// 重启后续写要从这里续起，否则轮转会 O_APPEND 到旧编号文件上（issue #5）。
func (w *DeviceWriter) highestSuffix(kind, day string) int {
	best := 0
	for i := 1; ; i++ {
		if _, err := os.Stat(w.filePath(kind, day, i)); err != nil {
			return best
		}
		best = i
	}
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
	if fi, err := ef.Stat(); err == nil {
		w.eventsSize = fi.Size()
	} else {
		w.eventsSize = 0
	}
	// 重启续写：suffix 从既有最大编号续起，避免轮转时撞车（issue #5）
	w.day, w.serialF, w.eventsF = day, sf, ef
	w.serialSuffix = w.highestSuffix("serial", day)
	w.eventsSuffix = w.highestSuffix("events", day)
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
		w.serialSuffix++
		nf, err := os.OpenFile(w.filePath("serial", w.day, w.serialSuffix), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			w.serialF = nil
			return err
		}
		w.serialF = nf
		w.serialSize = 0
	}
	n, err := fmt.Fprintf(w.serialF, "[%s] %s\n", Stamp(now), line)
	w.serialSize += int64(n)
	return err
}

// WriteEvent: 写一行事件流（调用方已格式化好内容）。事件量低，
// 但刷屏型故障（如复位循环）下同样需要大小轮转兜底（issue #5）。
func (w *DeviceWriter) WriteEvent(line string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if err := w.ensureDayLocked(now); err != nil {
		return err
	}
	maxB := w.eventsMaxBytes
	if maxB == 0 {
		maxB = w.maxBytes
	}
	if maxB > 0 && w.eventsSize >= maxB {
		_ = w.eventsF.Close()
		w.eventsSuffix++
		nf, err := os.OpenFile(w.filePath("events", w.day, w.eventsSuffix), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			w.eventsF = nil
			return err
		}
		w.eventsF = nf
		w.eventsSize = 0
	}
	n, err := fmt.Fprintf(w.eventsF, "[%s] %s\n", Stamp(now), line)
	w.eventsSize += int64(n)
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
