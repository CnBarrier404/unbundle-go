package cli

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

// Called after process exit. Recovery is limited to actual text tool results
// committed before cancellation, with no subsequent native assistant output.
func (a *Adapter) saveInterrupted(n *native, row session.Checkpoint, request messages.Request) *session.Checkpoint {
	if n == nil || len(n.expected) == 0 || len(request.Messages) == 0 {
		return nil
	}
	blocks, err := messages.Blocks(request.Messages[len(request.Messages)-1].Content)
	if err != nil || len(blocks) != len(n.expected) {
		return nil
	}
	var receipts []jsontext.Value
	for _, raw := range blocks {
		var receipt messages.ToolResultBlock
		if json.Unmarshal(raw, &receipt) != nil || receipt.Type != "tool_result" || !n.returned[receipt.ToolUseID] {
			return nil
		}
		content, err := messages.Blocks(receipt.Content)
		if err != nil {
			return nil
		}
		for _, block := range content {
			var text messages.TextBlock
			if json.Unmarshal(block, &text) != nil || text.Type != "text" || text.Text == nil {
				return nil
			}
		}
		receipt.Content, err = json.Marshal(content)
		if err != nil {
			return nil
		}
		if receipt.IsError != nil && !*receipt.IsError {
			receipt.IsError = nil
		}
		encoded, err := json.Marshal(receipt)
		if err != nil {
			return nil
		}
		receipts = append(receipts, encoded)
	}
	snapshot, err := a.store.SnapshotInput(n.id, receipts)
	if err != nil {
		n.logger.Warn("interrupted_checkpoint_unavailable")
		return nil
	}
	row.SystemPrompt, row.Instructions, row.Interrupted = n.prompt, n.instructions, true
	completed, err := a.store.Complete(row, session.InterruptionReceipt(), snapshot)
	if err != nil {
		n.logger.Error("interrupted_checkpoint_save_failed")
		return nil
	}
	n.logger.Info("interrupted_checkpoint_ready")
	return &completed
}
