package messages

import (
	"net/http"
	"strings"
)

const (
	APIVersion   = "2023-06-01"
	MessagesPath = "/v1/messages"
	ModelsPath   = "/v1/models"
	StatusPath   = "/status"
)

// ValidateHeaders accepts the capability markers advertised by Claude Code.
// A marker alone does not activate a feature: Decode and the native adapter
// still validate its fields and content blocks before starting inference.
func ValidateHeaders(headers http.Header) error {
	values := func(name string) []string {
		var out []string
		for key, value := range headers {
			if strings.EqualFold(key, name) {
				out = append(out, value...)
			}
		}
		return out
	}
	version := values("anthropic-version")
	if len(version) != 1 || version[0] != APIVersion {
		return invalid("anthropic-version", "requires one supported API version")
	}
	for _, beta := range values("anthropic-beta") {
		for _, marker := range strings.Split(beta, ",") {
			switch strings.TrimSpace(marker) {
			case "", "claude-code-20250219", "interleaved-thinking-2025-05-14", "effort-2025-11-24",
				"context-management-2025-06-27", "per-turn-control-2026-07-01",
				"mid-conversation-tool-changes-2026-07-01", "inline-tools-2026-09-15",
				"advisor-tool-2026-03-01", "thinking-token-count-2026-05-13",
				"advanced-tool-use-2025-11-20", "thinking-display-updates-2026-08-18":
			default:
				return &Error{"UNSUPPORTED_BETA", "anthropic-beta", "beta semantics have not been verified by the CLI adapter"}
			}
		}
	}
	// Credentials and identity headers are consumed separately; ordinary SDK
	// tracing and transport headers are not promoted to inference instructions.
	return nil
}
