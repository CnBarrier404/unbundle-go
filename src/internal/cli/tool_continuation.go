package cli

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

type toolBatch struct {
	results     map[string]*mcp.CallToolResult
	receipts    []messages.ToolResultBlock
	userContext []jsontext.Value
}

// A recovered native tool boundary accepts only the exact caller batch. The
// CLI receives real receipts as user input; no old tool call is executed again.
func resumedToolReceipts(assistant, user messages.Message) (map[string]bool, error) {
	blocks, err := messages.Blocks(assistant.Content)
	if err != nil || assistant.Role != "assistant" {
		return nil, protocol()
	}
	expected := make(map[string]block)
	for _, raw := range blocks {
		var call block
		if json.Unmarshal(raw, &call) != nil {
			return nil, protocol()
		}
		if call.Type == "tool_use" {
			if call.ID == "" || expected[call.ID].ID != "" {
				return nil, protocol()
			}
			expected[call.ID] = call
		}
	}
	batch, err := toolContinuation(user, expected)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(batch.results))
	for id := range batch.results {
		ids[id] = true
	}
	return ids, nil
}

// Validate the complete batch before exposing any result to the MCP waiters.
// User annotations remain explicitly identified beside the original output.
func toolContinuation(delta messages.Message, expected map[string]block) (*toolBatch, error) {
	invalid := func(reason string) error {
		return &messages.Error{Code: "INVALID_REQUEST", Field: "tool_result", Message: reason}
	}
	if delta.Role != "user" || len(expected) == 0 {
		return nil, invalid("requires an outstanding tool batch")
	}
	blocks, err := messages.Blocks(delta.Content)
	if err != nil {
		return nil, err
	}
	batch := &toolBatch{results: make(map[string]*mcp.CallToolResult, len(expected))}
	var last *mcp.CallToolResult
	for _, raw := range blocks {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &kind) != nil {
			return nil, invalid("invalid content block")
		}
		switch kind.Type {
		case "tool_result":
			var receipt messages.ToolResultBlock
			if len(batch.userContext) != 0 || jsonutil.Object(raw, &receipt) != nil || expected[receipt.ToolUseID].ID == "" || batch.results[receipt.ToolUseID] != nil {
				return nil, invalid("requires each pending result exactly once before user text")
			}
			result, err := toolResult(receipt)
			if err != nil {
				return nil, err
			}
			batch.results[receipt.ToolUseID], last = result, result
			batch.receipts = append(batch.receipts, receipt)
		case "text":
			var text messages.TextBlock
			if len(batch.results) != len(expected) || jsonutil.Object(raw, &text) != nil || text.Text == nil {
				return nil, invalid("user text must follow all pending results")
			}
			batch.userContext = append(batch.userContext, raw)
		default:
			return nil, &messages.Error{Code: "UNSUPPORTED_PARAMETER", Field: "tool_result", Message: "only text may accompany a tool result batch"}
		}
	}
	if len(batch.results) != len(expected) {
		return nil, invalid("requires every pending result exactly once")
	}
	if len(batch.userContext) > 0 {
		// Keep source, role, block order and text explicit. Do not merge the
		// annotation into the original text/image blocks or alter error state.
		payload, err := json.Marshal(struct {
			Kind    string           `json:"kind"`
			Role    string           `json:"role"`
			Content []jsontext.Value `json:"content"`
		}{"caller_user_context", "user", batch.userContext})
		if err != nil {
			return nil, err
		}
		last.Content = append(last.Content, &mcp.TextContent{Text: string(payload)})
	}
	return batch, nil
}
