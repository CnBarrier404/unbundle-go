package session

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

const SummarySystem = "You are a helpful AI assistant tasked with summarizing conversations."
const SummaryHeader = "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation."

// InstructionUpdate is private recovery data, never ordinary diagnostic output.
type InstructionUpdate struct {
	Index   int            `json:"messageIndex"`
	Role    string         `json:"role"`
	Content jsontext.Value `json:"content"`
}

// Compaction stores keyed receipts. The authoritative summary stays in the
// official CLI transcript; retries read it back rather than run a second model.
type Compaction struct {
	Request       string         `json:"request"`
	Source        []string       `json:"source"`
	Summary       string         `json:"summary"`
	SummaryLength int            `json:"summary_length"`
	Boundary      string         `json:"boundary"`
	ResponseID    string         `json:"response_id"`
	Usage         messages.Usage `json:"usage"`
	Leading       int            `json:"leading"`
}

func Text(content jsontext.Value) (string, bool) {
	blocks, err := messages.Blocks(content)
	if err != nil {
		return "", false
	}
	var parts []string
	for _, raw := range blocks {
		var block messages.TextBlock
		if json.Unmarshal(raw, &block) != nil || block.Type != "text" || block.Text == nil {
			return "", false
		}
		parts = append(parts, *block.Text)
	}
	return strings.Join(parts, "\n"), true
}

// Recognition is isolated from native command execution. A similar user text
// alone is never permission to run /compact.
func CompactionIntent(r messages.Request, identity Identity) (messages.Request, bool) {
	if len(r.Messages) == 0 {
		return r, false
	}
	text, ok := Text(r.Messages[len(r.Messages)-1].Content)
	if !ok || r.Messages[len(r.Messages)-1].Role != "user" {
		return r, false
	}
	text = strings.TrimPrefix(text, "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n")
	prompt := strings.HasPrefix(text, "Your task is to create a detailed summary of the conversation so far,") ||
		strings.HasPrefix(text, "Your task is to create a detailed summary of this conversation.")
	if !prompt {
		return r, false
	}
	system, _ := Text(r.System)
	summary := summarySystem(system)
	filtered := slices.Clone(r.Messages[:len(r.Messages)-1])
	for i := 0; i < len(filtered); i++ {
		value, _ := Text(filtered[i].Content)
		if filtered[i].Role == "system" && summarySystem(value) {
			if summary {
				return r, false
			}
			summary = true
			filtered = slices.Delete(filtered, i, i+1)
			i--
		}
	}
	if !summary && identity.RequestClass != "compaction" {
		return r, false
	}
	r.Messages = filtered
	return r, true
}

func summarySystem(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) == 1 {
		return lines[0] == SummarySystem
	}
	return len(lines) == 2 && lines[1] == SummarySystem && slices.Contains([]string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
	}, lines[0])
}

func compactSource(candidate Candidate, source messages.Request) ([]messages.Message, bool) {
	if candidate.Request.Model != source.Model || candidate.Assistant.Role != "assistant" {
		return nil, false
	}
	if system, _ := Text(source.System); !summarySystem(system) && !jsonutil.Equal(source.System, candidate.Request.System) {
		return nil, false
	}
	expected := append(slices.Clone(candidate.Request.Messages), candidate.Assistant)
	if len(source.Messages) >= len(expected) && prefix(candidate.Request.Messages, source.Messages) &&
		assistantReceiptEqual(candidate.Assistant, source.Messages[len(candidate.Request.Messages)], candidate.Request.Messages) {
		tail := source.Messages[len(expected):]
		if candidate.State == WaitingTools && len(tail) == 1 && tail[0].Role == "user" {
			return tail, true
		}
		return nil, len(tail) == 0 && candidate.State != WaitingTools
	}
	// Desktop may summarize the last completed API input. The authoritative
	// final assistant is already in the native session and is not replayed.
	return nil, candidate.State != WaitingTools && jsonutil.Equal(source.Messages, candidate.Request.Messages)
}

func (s *Store) requestDigest(r messages.Request) (string, error) {
	r.Stream, r.Metadata = false, nil
	return s.digest(r)
}

func (s *Store) CompactedDelta(record *Compaction, incoming messages.Request) ([]messages.Message, bool) {
	if record == nil || len(incoming.Messages) == 0 {
		return nil, false
	}
	start := 0
	for start < len(incoming.Messages) && incoming.Messages[start].Role == "system" {
		start++
	}
	if start == len(incoming.Messages) || incoming.Messages[start].Role != "user" {
		return nil, false
	}
	if record.Leading < 0 || record.Leading > len(record.Source) || start != record.Leading {
		return nil, false
	}
	for i := 0; i < start; i++ {
		digest, err := s.digest(incoming.Messages[i])
		if err != nil || digest != record.Source[i] {
			return nil, false
		}
	}
	text, ok := Text(incoming.Messages[start].Content)
	if !ok || !strings.HasPrefix(text, SummaryHeader+"\n\n") {
		return nil, false
	}
	text = StripSummaryTools(strings.TrimPrefix(text, SummaryHeader+"\n\n"))
	if record.SummaryLength < 1 || len(text) < record.SummaryLength {
		return nil, false
	}
	summary := text[:record.SummaryLength]
	digest, err := s.digest(summary)
	if err != nil || digest != record.Summary {
		return nil, false
	}
	suffix := text[record.SummaryLength:]
	if suffix != "" && !strings.HasPrefix(suffix, "\n") {
		return nil, false
	}
	remaining := incoming.Messages[start+1:]
	retained := 0
	for count := min(len(record.Source), len(remaining)); count > 0; count-- {
		match := true
		for i := 0; i < count; i++ {
			digest, err := s.digest(remaining[i])
			if err != nil || digest != record.Source[len(record.Source)-count+i] {
				match = false
				break
			}
		}
		if match {
			retained = count
			break
		}
	}
	delta := slices.Clone(remaining[retained:])
	if len(delta) == 0 {
		if strings.TrimSpace(suffix) == "" {
			return nil, false
		}
		content, _ := json.Marshal("Caller continuation after compaction:\n" + suffix)
		delta = []messages.Message{{Role: "user", Content: content}}
	} else if suffix != "" {
		// Keep caller continuation text as user data, before the fresh message.
		for i := range delta {
			if delta[i].Role != "user" {
				continue
			}
			blocks, err := messages.Blocks(delta[i].Content)
			if err != nil {
				return nil, false
			}
			head, _ := json.Marshal(map[string]string{"type": "text", "text": "Caller continuation after compaction:\n" + suffix})
			delta[i].Content, _ = json.Marshal(append([]jsontext.Value{head}, blocks...))
			break
		}
	}
	return delta, newInput(delta)
}

// Strip only the two documented tool-list wrappers, never arbitrary XML.
func StripSummaryTools(text string) string {
	if strings.HasPrefix(text, "Tools called before this summary: ") {
		if end := strings.IndexByte(text, '\n'); end >= 0 {
			return text[end+1:]
		}
	}
	const open = "<tools-used-before-summary>"
	const close = "</tools-used-before-summary>\n"
	if strings.HasPrefix(text, open) {
		if end := strings.Index(text, close); end >= len(open) {
			return text[end+len(close):]
		}
	}
	return text
}
