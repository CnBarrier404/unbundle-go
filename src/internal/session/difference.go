package session

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

// Report only fixed field categories and indices, never history contents,
// tool arguments, caller-defined field names, signatures or fingerprints.
func historyDifference(expected, actual []messages.Message) string {
	for i := 0; i < min(len(expected), len(actual)); i++ {
		left, right := expected[i], actual[i]
		if jsonutil.Equal(left, right) {
			continue
		}
		field := "message options"
		if left.Role != right.Role {
			field = "role"
		} else if !jsonutil.Equal(left.Content, right.Content) {
			field = contentDifference(left.Content, right.Content)
		}
		return fmt.Sprintf("history message %d differs: %s", i, field)
	}
	return fmt.Sprintf("history length differs: expected at least %d messages, received %d", len(expected), len(actual))
}

func contentDifference(left, right jsontext.Value) string {
	if left.Kind() != right.Kind() {
		return "content form"
	}
	a, errA := messages.Blocks(left)
	b, errB := messages.Blocks(right)
	if errA != nil || errB != nil {
		return "content"
	}
	if len(a) != len(b) {
		return fmt.Sprintf("content block count (expected %d, received %d)", len(a), len(b))
	}
	for i := range a {
		if jsonutil.Equal(a[i], b[i]) {
			continue
		}
		var x, y map[string]jsontext.Value
		if json.Unmarshal(a[i], &x) != nil || json.Unmarshal(b[i], &y) != nil {
			return "content"
		}
		for _, field := range []string{"type", "text", "thinking", "signature", "data", "id", "name", "input", "tool_use_id", "is_error", "content"} {
			if !jsonutil.Equal(x[field], y[field]) {
				return fmt.Sprintf("content block %d %s", i, field)
			}
		}
		return fmt.Sprintf("content block %d fields", i)
	}
	return "content encoding"
}
