package app

import (
	"fmt"
	"os"
	"sync"
)

// A bounded file writer, serialized even when multiple slog handlers share it.
// The active file plus .1 ... .N are retained; .1 is the newest backup.
type rotatingWriter struct {
	mu      sync.Mutex
	path    string
	max     int64
	backups int
	file    *os.File
	size    int64
}

func newRotatingWriter(path string, max int64, backups int) (*rotatingWriter, error) {
	if max <= 0 {
		max = 10 << 20
	}
	if backups <= 0 {
		backups = 3
	}
	w := &rotatingWriter{path: path, max: max, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}
func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file, w.size = f, info.Size()
	return nil
}
func (w *rotatingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(b)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(b)
	w.size += int64(n)
	return n, err
}
func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	for i := w.backups; i >= 1; i-- {
		source := w.path
		if i > 1 {
			source = fmt.Sprintf("%s.%d", w.path, i-1)
		}
		dest := fmt.Sprintf("%s.%d", w.path, i)
		// Rename replaces the oldest backup; no unbounded archive is created.
		if err := os.Rename(source, dest); err != nil && !os.IsNotExist(err) {
			_ = w.open()
			return err
		}
	}
	return w.open()
}
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
