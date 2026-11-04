// Package logging provides structured diagnostics without request content.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
)

type contextKey struct{}

func New(out io.Writer, level, format string) *slog.Logger {
	var minimum slog.Level
	_ = minimum.UnmarshalText([]byte(level)) // configuration validates the level
	options := &slog.HandlerOptions{Level: minimum}
	if format == "text" {
		return slog.New(slog.NewTextHandler(out, options))
	}
	return slog.New(slog.NewJSONHandler(out, options))
}

func With(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, logger)
}

func From(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(contextKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

// Cause deliberately excludes Error(): errors may contain paths, credentials,
// request bodies or native output. OS error numbers and exit codes are safe.
func Cause(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	attrs := []any{slog.String("type", fmt.Sprintf("%T", err))}
	var errno syscall.Errno
	var exit *exec.ExitError
	switch {
	case errors.As(err, &errno):
		attrs = append(attrs, slog.Uint64("errno", uint64(errno)), slog.String("reason", errno.Error()))
	case errors.As(err, &exit):
		attrs = append(attrs, slog.Int("exit_code", exit.ExitCode()))
	case errors.Is(err, context.DeadlineExceeded):
		attrs = append(attrs, slog.String("reason", "deadline_exceeded"))
	case errors.Is(err, context.Canceled):
		attrs = append(attrs, slog.String("reason", "cancelled"))
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		attrs = append(attrs, slog.String("reason", "not_found"))
	case errors.Is(err, os.ErrPermission):
		attrs = append(attrs, slog.String("reason", "permission_denied"))
	case errors.Is(err, io.EOF):
		attrs = append(attrs, slog.String("reason", "eof"))
	}
	return slog.Group("cause", attrs...)
}
