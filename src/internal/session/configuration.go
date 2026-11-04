package session

import (
	"slices"
	"strings"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

// ConfigurationChange names the actual incompatible setting. Append-only
// instructions and tool order changes have an explicit safe interpretation.
func ConfigurationChange(a, b messages.Request) string {
	if a.Model != b.Model {
		return "SESSION_MODEL_CHANGE_UNSUPPORTED"
	}
	if a.MaxTokens != b.MaxTokens {
		return "SESSION_OUTPUT_BUDGET_CHANGE_UNSUPPORTED"
	}
	if !jsonutil.Equal(a.Thinking, b.Thinking) || !jsonutil.Equal(a.OutputConfig, b.OutputConfig) {
		return "SESSION_THINKING_CHANGE_UNSUPPORTED"
	}
	if a.CacheTTL != b.CacheTTL {
		return "SESSION_CACHE_POLICY_CHANGE_UNSUPPORTED"
	}
	if !jsonutil.Equal(a.ToolChoice, b.ToolChoice) {
		return "SESSION_TOOL_POLICY_CHANGE_UNSUPPORTED"
	}
	a.Tools, b.Tools = sortedTools(a.Tools), sortedTools(b.Tools)
	if !jsonutil.Equal(a.Tools, b.Tools) {
		return "SESSION_TOOL_DEFINITIONS_CHANGE_UNSUPPORTED"
	}
	if !jsonutil.Equal(a.System, b.System) {
		old, errA := messages.Blocks(a.System)
		next, errB := messages.Blocks(b.System)
		if len(a.System) == 0 {
			old, errA = nil, nil
		}
		if errA != nil || errB != nil || len(next) <= len(old) {
			return "SESSION_SYSTEM_EDIT_UNSUPPORTED"
		}
		for i := range old {
			if !jsonutil.Equal(old[i], next[i]) {
				return "SESSION_SYSTEM_EDIT_UNSUPPORTED"
			}
		}
	}
	a.Messages, b.Messages = nil, nil
	a.Metadata, b.Metadata = nil, nil
	a.Stream, b.Stream = false, false
	a.System, b.System = nil, nil
	if !jsonutil.Equal(a, b) {
		return "SESSION_CONFIG_CHANGED"
	}
	return ""
}

func sortedTools(tools []messages.Tool) []messages.Tool {
	result := slices.Clone(tools)
	slices.SortFunc(result, func(a, b messages.Tool) int { return strings.Compare(a.Name, b.Name) })
	return result
}
