// Package logutil 提供按天滚动的文件日志，与应用日志（stderr）同时输出。
package logutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DailyFileWriter 是一个按天滚动的 io.Writer。
// 每次写入前按当前本地日期检查是否需要切换到新文件，日志文件名形如 2006-01-02.log。
type DailyFileWriter struct {
	dir           string
	prefix        string
	retentionDays int // 日志文件保留天数；<=0 表示不清理旧文件
	mu            sync.Mutex
	curDay        string // 当前已打开文件对应的日期（2006-01-02）
	file          *os.File
}

// NewDailyFileWriter 在 dir 下创建按天滚动的日志文件，文件名 <prefix>-YYYY-MM-DD.log。
// 不返回 error：目录创建失败或文件无法打开时退回 io.Discard，不影响服务启动。
func NewDailyFileWriter(dir, prefix string) *DailyFileWriter {
	return NewDailyFileWriterWithRetention(dir, prefix, 0)
}

// NewDailyFileWriterWithRetention 在 dir 下创建按天滚动的日志文件，并保留最近 retentionDays 天。
// retentionDays<=0 表示只滚动、不清理旧文件。
func NewDailyFileWriterWithRetention(dir, prefix string, retentionDays int) *DailyFileWriter {
	w := &DailyFileWriter{dir: dir, prefix: prefix, retentionDays: retentionDays}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return w
	}
	// 立即打开今天的文件，让启动早期的日志也能落盘。
	_ = w.rotate(time.Now())
	// 启动时先清理一次超出保留期的旧文件。
	if retentionDays > 0 {
		w.Cleanup(time.Now())
	}
	return w
}

func (w *DailyFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	day := now.Format("2006-01-02")
	if w.file == nil || w.curDay != day {
		if err := w.rotate(now); err != nil {
			// 打开新文件失败时丢弃本次日志，避免拖垮业务。
			return len(p), nil
		}
		// 跨天滚动后顺带清理一次过期文件。
		if w.retentionDays > 0 {
			w.cleanupLocked(now)
		}
	}
	return w.file.Write(p)
}

// rotate 关闭旧文件并打开当天的新文件。调用方需持有 w.mu。
func (w *DailyFileWriter) rotate(now time.Time) error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	name := fmt.Sprintf("%s-%s.log", w.prefix, now.Format("2006-01-02"))
	path := filepath.Join(w.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.file = f
	w.curDay = now.Format("2006-01-02")
	return nil
}

// Cleanup 删除超出保留期的日志文件（按文件名日期解析）。可并发安全调用。
func (w *DailyFileWriter) Cleanup(now time.Time) {
	if w.retentionDays <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cleanupLocked(now)
}

// cleanupLocked 删除 dir 下匹配 <prefix>-YYYY-MM-DD.log、且日期早于保留期的文件。调用方需持有 w.mu。
func (w *DailyFileWriter) cleanupLocked(now time.Time) {
	cutoff := now.AddDate(0, 0, -w.retentionDays)
	prefix := w.prefix + "-"
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		dateStr := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".log")
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue // 无法解析日期的文件不处理，避免误删
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(w.dir, name))
		}
	}
}

// Close 关闭当前文件。服务退出时调用。
func (w *DailyFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// NewMultiWriter 将 stderr 与按天滚动文件组合，供 slog 使用。
func NewMultiWriter(stderr io.Writer, file *DailyFileWriter) io.Writer {
	return io.MultiWriter(stderr, file)
}
