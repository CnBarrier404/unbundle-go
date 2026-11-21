package cli

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

// Called after process exit. Only committed input with no subsequent native
// assistant output can be recovered; caller history is never replayed.
func (a *Adapter) saveInterrupted(n *native, row session.Checkpoint, request messages.Request, input *messages.Message) *session.Checkpoint {
	if n == nil || input == nil || len(request.Messages) == 0 {
		return nil
	}
	if blocks, err := session.TextInput(input.Content); err == nil {
		snapshot, err := a.store.SnapshotInput(n.id, blocks)
		if err != nil {
			n.logger.Warn("interrupted_checkpoint_unavailable")
			return nil
		}
		row.SystemPrompt, row.Instructions = n.prompt, n.instructions
		row.Ready, row.Interrupted, row.PendingInput, row.Snapshot = true, true, true, snapshot
		if err := a.store.Put(row); err != nil {
			n.logger.Error("interrupted_checkpoint_save_failed")
			return nil
		}
		n.logger.Info("interrupted_input_checkpoint_ready")
		return &row
	}
	blocks, err := messages.Blocks(request.Messages[len(request.Messages)-1].Content)
	// advance clears expected when the next assistant message starts.
	// returned and the native transcript still identify committed receipts.
	if err != nil || len(blocks) == 0 {
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
