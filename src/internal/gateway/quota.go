package gateway

import (
	"fmt"
	"strconv"
	"time"

	"unbundle-go/src/internal/cli"
	"unbundle-go/src/internal/messages"
)

func (s *Server) quotaError(w *loggedResponse, failure *cli.Error, streaming bool) *cli.Error {
	now := time.Now()
	e := *failure
	if !streaming {
		w.Header().Set("X-Should-Retry", "false")
	}
	if e.QuotaResetAt <= now.Unix() {
		e.QuotaResetAt = 0
		return &e
	}
	if e.Outcome != "not_started" {
		// Subsequent requests for this model need not start the CLI while the
		// upstream's confirmed quota window is still closed.
		cached := e
		cached.Outcome = "not_started"
		cached.Usage = messages.Usage{}
		s.quotas.Store(w.model, cached)
	}
	seconds := e.QuotaResetAt - now.Unix()
	reset := time.Unix(e.QuotaResetAt, 0).UTC()
	if !streaming {
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(e.QuotaResetAt, 10))
		switch e.QuotaWindow {
		case "five_hour":
			w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "5h")
		case "seven_day":
			w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "7d")
		}
	}
	minutes := (seconds + 59) / 60
	remaining := fmt.Sprintf("%d min", minutes)
	if minutes >= 60 {
		remaining = fmt.Sprintf("%d hr %d min", minutes/60, minutes%60)
	}
	if e.QuotaWindow == "five_hour" {
		e.Message = fmt.Sprintf("%s · resets %s (UTC) · about %s remaining", e.Message, reset.Format("3:04pm"), remaining)
	} else {
		e.Message = fmt.Sprintf("%s. Resets in about %s (%s).", e.Message, remaining, reset.Format("2006-01-02 15:04 UTC"))
	}
	return &e
}
