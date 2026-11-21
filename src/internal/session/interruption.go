package session

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"

	"unbundle-go/src/internal/config"
	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

// This is the official client's local stop marker, never a model completion or
// input replayed into the native CLI. It is accepted only at a verified stop.
func InterruptionReceipt() messages.Message {
	return messages.Message{Role: "assistant", Content: []byte(`[{"type":"text","text":"No response requested."}]`)}
}

// TextInput excludes tool receipts and calls from the queued-message path.
func TextInput(content jsontext.Value) ([]jsontext.Value, error) {
	blocks, err := messages.Blocks(content)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, errors.New("empty interrupted input")
	}
	for _, raw := range blocks {
		var text messages.TextBlock
		if json.Unmarshal(raw, &text) != nil || text.Type != "text" || text.Text == nil {
			return nil, errors.New("interrupted input must contain text only")
		}
	}
	return blocks, nil
}

// Older gateways did not save ordinary canceled input. Recover such a record
// only when the last native conversation message exactly matches its stored
// user fingerprint. This also covers exit between cancellation and saving.
func (s *Store) RecoverInterruptedInput(row Checkpoint) (Checkpoint, error) {
	if row.Ready || row.PendingTools || row.Compaction != nil || len(row.History) == 0 {
		return row, errors.New("checkpoint is not an interrupted input candidate")
	}
	file, _, err := openTranscript(row.NativeID)
	if err != nil {
		return row, err
	}
	defer file.Close()
	var last messages.Message
	scanner := transcriptLines(file)
	for scanner.Scan() {
		var native transcriptRow
		if err := json.Unmarshal(scanner.Bytes(), &native); err != nil {
			return row, err
		}
		if native.Type == "user" || native.Type == "assistant" {
			last = messages.Message{}
			if err := json.Unmarshal(native.Message, &last); err != nil {
				return row, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return row, err
	}
	if last.Role != "user" {
		return row, errors.New("native input does not match interrupted checkpoint")
	}
	blocks, err := messages.Blocks(last.Content)
	if err != nil {
		return row, err
	}
	_, textErr := TextInput(last.Content)
	fingerprint := row.History[len(row.History)-1]
	if textErr != nil {
		// The MCP bridge represents string results as single text blocks.
		// Compare the original spelling as well, keeping the native bytes for the snapshot.
		short := slices.Clone(blocks)
		for i, raw := range blocks {
			var receipt messages.ToolResultBlock
			if jsonutil.Object(raw, &receipt) != nil || receipt.Type != "tool_result" || receipt.ToolUseID == "" {
				return row, errors.New("invalid interrupted tool receipt")
			}
			if alt, ok := alternateTextForm(messages.Message{Content: receipt.Content}); ok && alt.Content.Kind() == '"' {
				receipt.Content = alt.Content
				short[i], _ = json.Marshal(receipt)
			}
		}
		if !s.messageMatches(fingerprint, last) {
			last.Content, _ = json.Marshal(short)
		}
	}
	if !s.messageMatches(fingerprint, last) {
		return row, errors.New("native input does not match interrupted checkpoint")
	}
	snapshot, err := s.SnapshotInput(row.NativeID, blocks)
	if err != nil {
		return row, err
	}
	row.Interrupted = true
	if textErr != nil {
		return s.Complete(row, InterruptionReceipt(), snapshot)
	}
	row.Ready, row.PendingInput, row.Snapshot = true, true, snapshot
	return row, s.Put(row)
}

// Ctrl+Enter can append queued text to the canceled user message instead of
// inserting an assistant receipt. Verify every old message and the exact old
// block prefix against the checkpoint; only the new suffix reaches the CLI.
func (s *Store) materializeInterrupted(row Checkpoint, incoming messages.Request, c config.Config) Candidate {
	view := Candidate{ID: row.ID, Identity: row.Identity, State: Closed}
	count := len(row.History)
	if count == 0 || len(incoming.Messages) < count {
		return view
	}
	for i := 0; i < count-1; i++ {
		if !s.messageMatches(row.History[i], incoming.Messages[i]) {
			return view
		}
	}
	last := incoming.Messages[count-1]
	if last.Role != "user" {
		return view
	}
	blocks, err := messages.Blocks(last.Content)
	if err != nil {
		return view
	}
	first, limit := 1, len(blocks)
	if !row.PendingInput {
		// Desktop merges the receipts, its Esc marker, and new user text.
		// Only the receipt prefix belongs to the verified checkpoint.
		first = 0
		for _, raw := range blocks {
			var receipt messages.ToolResultBlock
			if json.Unmarshal(raw, &receipt) != nil || receipt.Type != "tool_result" {
				break
			}
			first++
		}
		if first == 0 || first+1 >= len(blocks) || !interruptMarker(blocks[first]) {
			return view
		}
		limit = first
	} else if _, err := TextInput(last.Content); err != nil {
		return view
	}
	for end := first; end <= limit; end++ {
		original := last
		original.Content, _ = json.Marshal(blocks[:end])
		if !s.messageMatches(row.History[count-1], original) {
			continue
		}
		view.Request = incoming
		if row.Settings != nil {
			view.Request = *row.Settings
			view.Request.CacheTTL = row.CacheTTL
		}
		view.Request.Messages = slices.Clone(incoming.Messages[:count])
		view.Request.Messages[count-1] = original
		view.ConfigMismatch = s.configurationMismatch(row, incoming, c)
		if row.Ready {
			view.State = Idle
		}
		var delta []messages.Message
		start := end
		if start < len(blocks) && interruptMarker(blocks[start]) {
			start++
		}
		if start < len(blocks) {
			added := last
			added.Content, _ = json.Marshal(blocks[start:])
			delta = append(delta, added)
		}
		tail := incoming.Messages[count:]
		if len(delta) == 0 && len(tail) > 0 && messageEqual(tail[0], InterruptionReceipt()) {
			tail = tail[1:]
		}
		delta = append(delta, tail...)
		if newInput(delta) {
			for _, message := range delta {
				if message.Role == "user" {
					if _, err := TextInput(message.Content); err != nil {
						return view
					}
				}
			}
			view.RebasedDelta = delta
		}
		return view
	}
	return view
}

func interruptMarker(raw jsontext.Value) bool {
	var block messages.TextBlock
	return jsonutil.Object(raw, &block) == nil && block.Type == "text" && block.Text != nil &&
		(*block.Text == "[Request interrupted by user]" || *block.Text == "[Request interrupted by user]\n")
}
