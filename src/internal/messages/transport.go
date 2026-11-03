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

// ValidateHeaders defines the initial transport dialect. It deliberately does
// not authenticate a request. Only markers for the native CLI, thinking blocks
// and mapped effort are accepted; ?beta=true alone grants no capabilities.
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
			case "", "claude-code-20250219", "interleaved-thinking-2025-05-14", "effort-2025-11-24":
			default:
				return &Error{"UNSUPPORTED_BETA", "anthropic-beta", "beta semantics have not been verified by the CLI adapter"}
			}
		}
	}
	// Credentials and identity headers are consumed separately; ordinary SDK
	// tracing and transport headers are not promoted to inference instructions.
	return nil
}
