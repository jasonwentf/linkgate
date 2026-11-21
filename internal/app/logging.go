package app

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger 依据配置创建日志器。文件为空时写 stdout（交给 launchd/systemd 收集）。
func NewLogger(cfg *Config) (*slog.Logger, io.Closer, error) {
	level := &slog.LevelVar{}
	level.Set(parseLogLevel(cfg.Log.Level))

	var w io.Writer = os.Stdout
	var closer io.Closer
	if cfg.Log.File != "" {
		path := ExpandPath(cfg.Log.File)
		if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
			return nil, nil, err
		}
		f, err := newRotatingWriter(path, cfg.Log.MaxSizeBytes, cfg.Log.MaxBackups)
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
	return slog.New(&levelHandler{Handler: h, level: level}), closer, nil
}

// All derived loggers share one atomic level; file/format remain startup resources.
type levelHandler struct {
	slog.Handler
	level *slog.LevelVar
}

func (h *levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithAttrs(attrs), level: h.level}
}
func (h *levelHandler) WithGroup(name string) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithGroup(name), level: h.level}
}
func parseLogLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
func setLoggerLevel(logger *slog.Logger, name string) {
	if h, ok := logger.Handler().(*levelHandler); ok {
		h.level.Set(parseLogLevel(name))
	}
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i > 0 {
		return path[:i]
	}
	return "."
}
