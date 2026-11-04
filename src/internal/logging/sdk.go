package logging

import (
	"context"
	"log/slog"
)

// SDK accepts only known static messages. The SDK embeds errors in some
// messages and may attach protocol payloads, so all its attributes are omitted.
func SDK(logger *slog.Logger) *slog.Logger {
	return slog.New(&sdkHandler{logger.Handler()})
}

type sdkHandler struct{ slog.Handler }

func (h *sdkHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level < slog.LevelWarn {
		level = slog.LevelDebug
	}
	return h.Handler.Enabled(ctx, level)
}

func (h *sdkHandler) Handle(ctx context.Context, record slog.Record) error {
	message := "mcp_sdk_diagnostic"
	switch record.Message {
	case "server connecting", "server session connected", "server session disconnected", "session initialized",
		"initialized before initialize", "duplicate initialized notification", "duplicate initialize request",
		"jsonrpc2 internal error", "server connect error":
		message = record.Message
	}
	level := record.Level
	if level < slog.LevelWarn {
		level = slog.LevelDebug
	}
	return h.Handler.Handle(ctx, slog.NewRecord(record.Time, level, message, 0))
}

func (h *sdkHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *sdkHandler) WithGroup(string) slog.Handler      { return h }
