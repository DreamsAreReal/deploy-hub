package obs

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	logDirPerm  = 0o750
	logFilePerm = 0o600
	maxLogBytes = 10 << 20
	rotatedExt  = ".1"
)

func ParseLevel(v string) slog.Level {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.TrimSpace(v))); err != nil {
		return slog.LevelInfo
	}
	return lvl
}

func NewLogger(w io.Writer, level slog.Level, command string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h).With("service", "deploy-hub", "command", command)
}

func OpenLogFile(path string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), logDirPerm); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		if err := os.Rename(path, path+rotatedExt); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY|os.O_CREATE, logFilePerm)
}
