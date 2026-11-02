package messages

import "encoding/json/jsontext"

// Usage keeps unavailable counters unknown. An eventual HTTP encoder must not
// replace these with fabricated zeros to manufacture a successful response.
type Usage struct {
	InputTokens              *int64 `json:"input_tokens,omitempty"`
	OutputTokens             *int64 `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
}

type Response struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []jsontext.Value `json:"content"`
	StopReason   *string          `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        Usage            `json:"usage"`
}

const (
	StopEndTurn   = "end_turn"
	StopMaxTokens = "max_tokens"
	StopToolUse   = "tool_use"
	StopRefusal   = "refusal"
)

// CheckStopReason never turns an unknown CLI terminal event into success.
func CheckStopReason(reason string) error {
	switch reason {
	case StopEndTurn, StopMaxTokens, StopToolUse, StopRefusal:
		return nil
	default:
		return &Error{"UPSTREAM_STOP_UNSUPPORTED", "stop_reason", "unrecognized or unsupported terminal reason"}
	}
}
