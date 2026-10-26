package app

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger 依据配置创建日志器。文件为空时写 stdout（交给 launchd/systemd 收集）。
func NewLogger(cfg *Config) (*slog.Logger, io.Closer, error) {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Log.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var w io.Writer = os.Stdout
	var closer io.Closer
	if cfg.Log.File != "" {
		path := ExpandPath(cfg.Log.File)
		if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
			return nil, nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w = f
		closer = f
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(cfg.Log.Format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h), closer, nil
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i > 0 {
		return path[:i]
	}
	return "."
}
