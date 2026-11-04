package cli

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"unbundle-go/src/internal/logging"
	"unbundle-go/src/internal/messages"
)

func logFailure(logger *slog.Logger, event string, err error) {
	level := slog.LevelError
	attrs := []any{logging.Cause(err)}
	var failure *Error
	var invalid *messages.Error
	if errors.As(err, &failure) {
		attrs = append(attrs, "error_code", failure.Code, "stage", failure.Stage, "outcome", failure.Outcome)
		if failure.Status < 500 {
			level = slog.LevelWarn
		}
	} else if errors.As(err, &invalid) {
		level = slog.LevelWarn
		attrs = append(attrs, "error_code", invalid.Code, "stage", "validation")
	}
	logger.Log(context.Background(), level, event, attrs...)
}

// Count stderr without retaining or logging its potentially sensitive content.
type diagnosticCount struct{ bytes atomic.Int64 }

func (d *diagnosticCount) Write(data []byte) (int, error) {
	d.bytes.Add(int64(len(data)))
	return len(data), nil
}
