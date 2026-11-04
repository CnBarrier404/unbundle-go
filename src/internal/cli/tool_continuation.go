package cli

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

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
	results, err := toolContinuation(user, expected)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(results))
	for id := range results {
		ids[id] = true
	}
	return ids, nil
}

// Validate the complete batch before exposing any result to the MCP waiters.
// User annotations remain explicitly identified beside the original output.
func toolContinuation(delta messages.Message, expected map[string]block) (map[string]*mcp.CallToolResult, error) {
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
	results := make(map[string]*mcp.CallToolResult, len(expected))
	var last *mcp.CallToolResult
	var context []jsontext.Value
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
			if len(context) != 0 || jsonutil.Object(raw, &receipt) != nil || expected[receipt.ToolUseID].ID == "" || results[receipt.ToolUseID] != nil {
				return nil, invalid("requires each pending result exactly once before user text")
			}
			result, err := toolResult(receipt)
			if err != nil {
				return nil, err
			}
			results[receipt.ToolUseID], last = result, result
		case "text":
			var text messages.TextBlock
			if len(results) != len(expected) || jsonutil.Object(raw, &text) != nil || text.Text == nil {
				return nil, invalid("user text must follow all pending results")
			}
			context = append(context, raw)
		default:
			return nil, &messages.Error{Code: "UNSUPPORTED_PARAMETER", Field: "tool_result", Message: "only text may accompany a tool result batch"}
		}
	}
	if len(results) != len(expected) {
		return nil, invalid("requires every pending result exactly once")
	}
	if len(context) > 0 {
		// Keep source, role, block order and text explicit. Do not merge the
		// annotation into the original text/image blocks or alter error state.
		payload, err := json.Marshal(struct {
			Kind    string           `json:"kind"`
			Role    string           `json:"role"`
			Content []jsontext.Value `json:"content"`
		}{"caller_user_context", "user", context})
		if err != nil {
			return nil, err
		}
		last.Content = append(last.Content, &mcp.TextContent{Text: string(payload)})
	}
	return results, nil
}
