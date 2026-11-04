package session

import (
	"strings"

	"unbundle-go/src/internal/messages"
)

// These official client prompts produce UI metadata omitted from main history.
// Match their protocol wording, not ordinary user requests to summarize work.
func AuxiliaryIntent(request messages.Request) string {
	if len(request.Messages) == 0 {
		return ""
	}
	last := request.Messages[len(request.Messages)-1]
	text, ok := Text(last.Content)
	if !ok || last.Role != "user" {
		return ""
	}
	if strings.HasPrefix(text, "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]\n\n") {
		return "suggestion"
	}
	if text == "The user stepped away and is coming back. Recap in under 40 words, 1-2 plain sentences, no markdown. Lead with the overall goal and current task, then the one next action. Skip root-cause narrative, fix internals, secondary to-dos, and em-dash tangents." {
		return "recap"
	}
	return ""
}
