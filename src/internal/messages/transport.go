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
// not authenticate a request. No beta is executable until the CLI adapter has
// verified its semantics; accepting ?beta=true alone grants no capabilities.
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
		if strings.TrimSpace(beta) != "" {
			return &Error{"UNSUPPORTED_BETA", "anthropic-beta", "beta semantics have not been verified by the CLI adapter"}
		}
	}
	// Credentials and identity headers are consumed separately; ordinary SDK
	// tracing and transport headers are not promoted to inference instructions.
	return nil
}
