package session

import "unbundle-go/src/internal/messages"

// This is the official client's local stop marker, never a model completion or
// input replayed into the native CLI. It is accepted only at a verified stop.
func InterruptionReceipt() messages.Message {
	return messages.Message{Role: "assistant", Content: []byte(`[{"type":"text","text":"No response requested."}]`)}
}
