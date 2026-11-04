package session

import (
	"encoding/json/v2"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

// Alternate only the API's string shorthand and a single plain text block.
// Stored history and native input retain their original representation.
func alternateTextForm(message messages.Message) (messages.Message, bool) {
	blocks, err := messages.Blocks(message.Content)
	if err != nil {
		return message, false
	}
	if message.Content.Kind() == '"' {
		message.Content, err = json.Marshal(blocks)
		return message, err == nil
	}
	if len(blocks) != 1 {
		return message, false
	}
	var block messages.TextBlock
	if jsonutil.Object(blocks[0], &block) != nil || block.Type != "text" || block.Text == nil || block.CacheControl != nil || !jsonutil.Equal(blocks[0], block) {
		return message, false
	}
	message.Content, err = json.Marshal(*block.Text)
	return message, err == nil
}

func messageEqual(a, b messages.Message) bool {
	if jsonutil.Equal(a, b) {
		return true
	}
	alternate, ok := alternateTextForm(a)
	return ok && jsonutil.Equal(alternate, b)
}

// Match either equivalent spelling against existing keyed fingerprints without
// rewriting checkpoints or weakening comparisons of other message fields.
func (s *Store) messageMatches(fingerprint string, message messages.Message) bool {
	digest, err := s.digest(message)
	if err == nil && digest == fingerprint {
		return true
	}
	alternate, ok := alternateTextForm(message)
	if !ok {
		return false
	}
	digest, err = s.digest(alternate)
	return err == nil && digest == fingerprint
}
