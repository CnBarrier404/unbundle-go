package cli

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"unbundle-go/src/internal/logging"
)

const QuotaExhausted = "UPSTREAM_QUOTA_EXHAUSTED"

type quotaInfo struct {
	Status   string `json:"status"`
	Window   string `json:"rateLimitType"`
	ResetsAt int64  `json:"resetsAt"`
}

type quotaObservation struct {
	quotaInfo
	Utilization *float64 `json:"utilization"`
}

// A notification alone is advisory. Only a subsequent native API error can
// confirm rejection of an inference request.
func (n *native) logQuotaEvent(ctx context.Context, line []byte) {
	var event struct {
		SessionID string `json:"session_id"`
		Info      struct {
			quotaObservation
			Windows struct {
				FiveHour *quotaObservation `json:"five_hour"`
				SevenDay *quotaObservation `json:"seven_day"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &event) != nil || event.SessionID != n.id {
		return
	}
	n.lastQuota = event.Info.quotaInfo
	logger := logging.From(ctx)
	logWindow := func(window string, observation quotaObservation) {
		attrs := []any{"quota_window", window}
		switch observation.Status {
		case "allowed", "allowed_warning", "rejected":
			attrs = append(attrs, "quota_status", observation.Status)
		}
		if observation.ResetsAt > 0 {
			attrs = append(attrs, "quota_reset_at", observation.ResetsAt)
		}
		if observation.Utilization != nil && *observation.Utilization >= 0 {
			attrs = append(attrs, slog.Float64("quota_utilization", *observation.Utilization))
		}
		if len(attrs) > 2 {
			logger.InfoContext(ctx, "quota_observed", attrs...)
		}
	}
	if event.Info.Windows.FiveHour != nil {
		logWindow("five_hour", *event.Info.Windows.FiveHour)
	}
	if event.Info.Windows.SevenDay != nil {
		logWindow("seven_day", *event.Info.Windows.SevenDay)
	}
	switch event.Info.Window {
	case "five_hour":
		if event.Info.Windows.FiveHour != nil {
			return
		}
	case "seven_day":
		if event.Info.Windows.SevenDay != nil {
			return
		}
	case "seven_day_opus", "seven_day_sonnet", "seven_day_overage_included", "overage":
	default:
		return
	}
	logWindow(event.Info.Window, event.Info.quotaObservation)
}

// Side questions wrap upstream failures in synthetic text. The OAuth CLI also
// emits the rejected quota window separately from this reply.
func auxiliaryFailure(text *string, info quotaInfo) *Error {
	if text != nil {
		body, apiError := strings.CutPrefix(*text, "(API error: ")
		if apiError {
			body = strings.TrimSuffix(body, ")")
			switch info.Window {
			case "5h":
				info.Window = "five_hour"
			case "7d":
				info.Window = "seven_day"
			}
			if info.Status == "rejected" && info.Window != "" {
				return quotaFailure("", http.StatusTooManyRequests, info, body)
			}
			for _, prefix := range []string{"Request rejected (429)", "Server is temporarily limiting requests (not your usage limit)"} {
				if notice, ok := strings.CutPrefix(body, prefix); ok {
					if failure := quotaFailure("", 0, quotaInfo{}, strings.TrimPrefix(notice, " · ")); failure != nil {
						return failure
					}
					return upstreamFailure(http.StatusTooManyRequests, "")
				}
			}
			if failure := quotaFailure("", 0, quotaInfo{}, body); failure != nil {
				return failure
			}
			return upstreamFailure(0, "")
		}
	}
	return Failure("AUXILIARY_NO_ANSWER", "inference", "official CLI returned no model answer to the background request", http.StatusUnprocessableEntity)
}

// Only recognize the CLI's measured quota or its exact legacy five-hour
// notice. Upstream diagnostics and arbitrary text never reach the caller.
var fiveHourNotice = regexp.MustCompile(`^You've hit your 5-hour limit(?: · resets ([0-9]{1,2}:[0-9]{2}(?:am|pm)) \(UTC\))?$`)

func quotaFailure(kind string, status int, info quotaInfo, text string) *Error {
	if status != 0 && status != http.StatusTooManyRequests {
		return nil
	}
	notice := fiveHourNotice.FindStringSubmatch(strings.TrimSpace(text))
	if kind != "usage_limit_reached" && (status != http.StatusTooManyRequests || info.Status != "rejected") && notice == nil {
		return nil
	}
	now := time.Now().UTC()
	if info.Window == "" && notice != nil {
		info.Window = "five_hour"
	}
	if info.ResetsAt == 0 && notice != nil && notice[1] != "" {
		if clock, err := time.Parse("3:04pm", notice[1]); err == nil {
			reset := now.Truncate(24 * time.Hour).Add(time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute)
			if !reset.After(now) {
				reset = reset.Add(24 * time.Hour)
			}
			if reset.Sub(now) <= 5*time.Hour {
				info.ResetsAt = reset.Unix()
			}
		}
	}
	e := Failure(QuotaExhausted, "inference", "Claude usage limit reached", http.StatusTooManyRequests)
	e.Type = "billing_error"
	switch info.Window {
	case "five_hour":
		e.Message = "You've hit your 5-hour limit"
		e.QuotaWindow = info.Window
	case "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_overage_included", "overage":
		e.QuotaWindow = info.Window
	}
	if info.ResetsAt > now.Unix() && info.ResetsAt <= now.AddDate(0, 1, 1).Unix() {
		e.QuotaResetAt = info.ResetsAt
	}
	return e
}
