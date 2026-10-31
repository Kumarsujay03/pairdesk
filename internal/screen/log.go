package screen

import "log/slog"

// slogLogger aliases *slog.Logger so option structs read naturally.
type slogLogger = slog.Logger

func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
