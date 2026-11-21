package gateway

import (
	"log/slog"
	"net/http"
	"time"

	"unbundle-go/src/internal/cli"
	"unbundle-go/src/internal/logging"
	"unbundle-go/src/internal/messages"
)

// Unwrap keeps ResponseController deadlines and flushing available for SSE.
type loggedResponse struct {
	http.ResponseWriter
	status   int
	bytes    int64
	wrote    bool
	failure  *cli.Error
	writeErr error
	result   string
	model    string
	stream   bool
	usage    messages.Usage
	partial  bool
	reused   bool
}

func (w *loggedResponse) recordInference(response messages.Response, err error) {
	w.reused = response.Reused
	if w.reused {
		return // A replayed receipt represents a prior request's usage.
	}
	w.usage = response.Usage
	if err != nil {
		w.partial = true
		if usage := apiError(err).Usage; usage != (messages.Usage{}) {
			w.usage = usage
		}
	}
}

func (w *loggedResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *loggedResponse) WriteHeader(status int) {
	if !w.wrote && status >= 200 {
		w.status, w.wrote = status, true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggedResponse) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	if err != nil {
		w.writeErr = err
	}
	return n, err
}

func (w *loggedResponse) finish(logger *slog.Logger, r *http.Request, start time.Time) {
	level := slog.LevelInfo
	attrs := []any{"http_status", w.status, "duration_ms", time.Since(start).Milliseconds(), "response_bytes", w.bytes}
	if w.model != "" {
		attrs = append(attrs, "model", w.model, "stream", w.stream, "response_reused", w.reused)
	}
	if w.failure != nil {
		level = slog.LevelWarn
		if w.failure.Status >= 500 {
			level = slog.LevelError
		}
		w.result = "failed"
		attrs = append(attrs, "error_code", w.failure.Code, "stage", w.failure.Stage, "outcome", w.failure.Outcome, "retryable", w.failure.Retryable)
		if w.failure.QuotaWindow != "" {
			attrs = append(attrs, "quota_window", w.failure.QuotaWindow)
		}
		if w.failure.QuotaResetAt > 0 {
			attrs = append(attrs, "quota_reset_at", w.failure.QuotaResetAt)
		}
		if w.writeErr != nil {
			attrs = append(attrs, logging.Cause(w.writeErr))
		}
	} else if w.writeErr != nil || r.Context().Err() != nil {
		w.result, level = "disconnected", slog.LevelWarn
		cause := w.writeErr
		if cause == nil {
			cause = r.Context().Err()
		}
		attrs = append(attrs, logging.Cause(cause))
	}
	var usage []any
	for _, counter := range []struct {
		name  string
		value *int64
	}{
		{"input_tokens", w.usage.InputTokens},
		{"output_tokens", w.usage.OutputTokens},
		{"cache_read_input_tokens", w.usage.CacheReadInputTokens},
		{"cache_creation_input_tokens", w.usage.CacheCreationInputTokens},
	} {
		if counter.value != nil {
			usage = append(usage, slog.Int64(counter.name, *counter.value))
		}
	}
	if len(usage) > 0 {
		attrs = append(attrs, slog.Group("usage", usage...), "usage_partial", w.partial || len(usage) < 4)
	}
	attrs = append(attrs, "result", w.result)
	logger.Log(r.Context(), level, "request_finished", attrs...)
}

// Only configured routes are logged; arbitrary URLs and methods can carry data.
func requestRoute(r *http.Request) (string, string) {
	method := r.Method
	if method != http.MethodGet && method != http.MethodPost {
		method = "other"
	}
	switch r.URL.Path {
	case messages.StatusPath, messages.ModelsPath, messages.MessagesPath:
		return method, r.URL.Path
	default:
		return method, "other"
	}
}
