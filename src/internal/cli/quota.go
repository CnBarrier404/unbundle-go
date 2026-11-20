package cli

import (
	"net/http"
	"regexp"
	"strings"
	"time"
)

const QuotaExhausted = "UPSTREAM_QUOTA_EXHAUSTED"

type quotaInfo struct {
	Status   string `json:"status"`
	Window   string `json:"rateLimitType"`
	ResetsAt int64  `json:"resetsAt"`
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
