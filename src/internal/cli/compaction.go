package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

// Capability checks do not submit model input or release any tool waiter.
func (n *native) prepareCompact(ctx context.Context, waiting bool) (map[string]modelUsage, error) {
	if n.timer != nil {
		n.timer.Stop()
	}
	initialized, err := n.control(ctx, map[string]any{"subtype": "initialize"})
	if err != nil {
		return nil, err
	}
	var info struct {
		Commands     []jsontext.Value `json:"commands"`
		Capabilities []string         `json:"capabilities"`
	}
	if json.Unmarshal(initialized, &info) != nil {
		return nil, protocol()
	}
	compact := false
	for _, raw := range info.Commands {
		var name string
		if raw.Kind() == '"' {
			_ = json.Unmarshal(raw, &name)
		} else {
			var command struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &command)
			name = command.Name
		}
		compact = compact || name == "compact"
	}
	if !compact {
		return nil, validation("NATIVE_COMPACT_UNAVAILABLE", "official CLI does not advertise native compact; the conversation was preserved")
	}
	if waiting && (!slices.Contains(info.Capabilities, "interrupt_receipt_v1") || !slices.Contains(info.Capabilities, "interrupt_cancel_queued_v1")) {
		return nil, validation("NATIVE_COMPACT_INTERRUPT_UNAVAILABLE", "tool-boundary compaction requires confirmed interruption and queue cancellation; submit the real results normally")
	}
	usage, err := n.modelUsage(ctx)
	if err != nil {
		return nil, err
	}
	if len(usage) == 0 {
		return nil, validation("NATIVE_COMPACT_USAGE_UNAVAILABLE", "official CLI did not provide cumulative usage; the conversation was preserved")
	}
	return usage, nil
}

func (n *native) interruptForCompact(ctx context.Context) error {
	n.interrupting = true
	defer func() { n.interrupting = false }()
	for id := range n.expected {
		n.bridge.interruptedIDs.Store(id, true)
	}
	for _, call := range n.pending {
		call.interrupted.Store(true)
	}
	terminal := false
	observe := func(line []byte) (bool, error) {
		var frame struct {
			Type    string `json:"type"`
			Session string `json:"session_id"`
			Reason  string `json:"terminal_reason"`
		}
		if json.Unmarshal(line, &frame) != nil {
			return false, protocol()
		}
		if frame.Type == "result" {
			if terminal || frame.Session != n.id || !slices.Contains([]string{"aborted_streaming", "aborted_tools"}, frame.Reason) {
				return true, protocol()
			}
			terminal = true
			return true, nil
		}
		// Native cancellation placeholders are never presented as local results.
		if frame.Type == "user" {
			return true, nil
		}
		return false, nil
	}
	receipt, err := n.control(ctx, map[string]any{"subtype": "interrupt", "cancel_queued": true}, observe)
	if err != nil {
		return err
	}
	var ack struct {
		StillQueued *[]string `json:"still_queued"`
		Cancelled   *[]string `json:"cancelled"`
	}
	if json.Unmarshal(receipt, &ack) != nil || ack.StillQueued == nil || len(*ack.StillQueued) != 0 || ack.Cancelled == nil {
		return Failure("NATIVE_COMPACT_INTERRUPT_UNCONFIRMED", "compaction", "native turn interruption was not confirmed", 502)
	}
	var output int64
	for !terminal {
		line, err := n.maintenanceLine(ctx, &output)
		if err != nil {
			return err
		}
		handled, err := observe(line)
		if err != nil {
			return err
		}
		if !handled {
			if err := n.p.accept(line); err != nil {
				return err
			}
		}
	}
	for _, call := range n.pending {
		close(call.abandon)
	}
	n.pending = make(map[string]*toolCall)
	n.expected = make(map[string]block)
	n.results = make(map[string]*mcp.CallToolResult)
	n.returned = make(map[string]bool)
	n.accounted = zeroUsage()
	return nil
}

// Buffering must be acknowledged as zero inference before /compact can run.
func (n *native) bufferCompact(ctx context.Context, message messages.Message) error {
	id := uuid.NewString()
	if err := n.writeFrame(map[string]any{"type": "user", "uuid": id, "message": message, "shouldQuery": false, "client_composed": true, "is_meta": true}); err != nil {
		return err
	}
	var output int64
	for {
		line, err := n.maintenanceLine(ctx, &output)
		if err != nil {
			return err
		}
		var frame struct {
			Type     string   `json:"type"`
			Session  string   `json:"session_id"`
			Error    bool     `json:"is_error"`
			Turns    *int     `json:"num_turns"`
			Duration *int     `json:"duration_api_ms"`
			ID       string   `json:"user_message_uuid"`
			IDs      []string `json:"user_message_uuids"`
		}
		if json.Unmarshal(line, &frame) != nil {
			return protocol()
		}
		if frame.Type == "result" {
			if frame.ID != "" {
				frame.IDs = append(frame.IDs, frame.ID)
			}
			if frame.Session != n.id || frame.Error || frame.Turns == nil || *frame.Turns != 0 || frame.Duration == nil || *frame.Duration != 0 || len(frame.IDs) == 0 {
				return Failure("COMPACTION_BUFFER_UNCONFIRMED", "compaction", "real context was not confirmed as buffered without inference", 502)
			}
			for _, receipt := range frame.IDs {
				if receipt != id {
					return protocol()
				}
			}
			return nil
		}
		if frame.Type != "system" && frame.Type != "rate_limit_event" {
			return protocol()
		}
		if err := n.p.accept(line); err != nil {
			return err
		}
	}
}

func compactInput(batch *toolBatch) ([]messages.Message, error) {
	var buffered []messages.Message
	for _, receipt := range batch.receipts {
		isError := receipt.IsError != nil && *receipt.IsError
		head, err := json.Marshal(map[string]any{"type": "text", "text": "The local client completed tool call " + receipt.ToolUseID + ". Native waiting was interrupted only to compact. Preserve the following actual tool-output data and do not repeat the operation. is_error=" + map[bool]string{true: "true", false: "false"}[isError]})
		if err != nil {
			return nil, protocol()
		}
		content := []jsontext.Value{head}
		if len(receipt.Content) > 0 {
			results, err := messages.Blocks(receipt.Content)
			if err != nil {
				return nil, err
			}
			content = append(content, results...)
		}
		encoded, err := json.Marshal(content)
		if err != nil {
			return nil, protocol()
		}
		buffered = append(buffered, messages.Message{Role: "user", Content: encoded})
	}
	if len(batch.userContext) > 0 {
		encoded, err := json.Marshal(batch.userContext)
		if err != nil {
			return nil, protocol()
		}
		buffered = append(buffered, messages.Message{Role: "user", Content: encoded})
	}
	return buffered, nil
}

func (n *native) compact(ctx context.Context, request messages.Request, buffered []messages.Message, before map[string]modelUsage) (string, messages.Usage, error) {
	if len(buffered) > 0 {
		if err := n.interruptForCompact(ctx); err != nil {
			return "", messages.Usage{}, err
		}
		for _, message := range buffered {
			if err := n.bufferCompact(ctx, message); err != nil {
				return "", messages.Usage{}, err
			}
		}
	}
	prompt, _ := session.Text(request.Messages[len(request.Messages)-1].Content)
	if _, focus, ok := strings.Cut(prompt, "\n\nAdditional Instructions:\n"); ok && focus != "" {
		content, _ := json.Marshal("Caller requested compaction focus, for summarization only:\n" + focus)
		if err := n.bufferCompact(ctx, messages.Message{Role: "user", Content: content}); err != nil {
			return "", messages.Usage{}, err
		}
	}
	// Only this verified maintenance path omits client_composed. Ordinary input
	// always keeps slash commands and @paths as caller data.
	if err := n.writeFrame(map[string]any{"type": "user", "uuid": uuid.NewString(), "message": map[string]string{"role": "user", "content": "/compact"}}); err != nil {
		return "", messages.Usage{}, err
	}
	var boundary string
	var output int64
	for {
		line, err := n.maintenanceLine(ctx, &output)
		if err != nil {
			return "", messages.Usage{}, err
		}
		var frame struct {
			Type    string                `json:"type"`
			Subtype string                `json:"subtype"`
			UUID    string                `json:"uuid"`
			Session string                `json:"session_id"`
			Error   bool                  `json:"is_error"`
			Usage   map[string]modelUsage `json:"modelUsage"`
		}
		if json.Unmarshal(line, &frame) != nil {
			return "", messages.Usage{}, protocol()
		}
		switch frame.Type {
		case "system":
			if frame.Subtype == "compact_boundary" {
				if boundary != "" || frame.Session != n.id || uuid.Validate(frame.UUID) != nil {
					return "", messages.Usage{}, protocol()
				}
				boundary = frame.UUID
			} else if err := n.p.accept(line); err != nil {
				return "", messages.Usage{}, err
			}
		case "result":
			if frame.Session != n.id || frame.Error || frame.Subtype != "success" || boundary == "" {
				return "", messages.Usage{}, Failure("NATIVE_COMPACT_NOT_COMMITTED", "compaction", "official CLI did not confirm native compaction", 502)
			}
			if len(frame.Usage) == 0 {
				return "", messages.Usage{}, Failure("COMPACTION_USAGE_UNKNOWN", "compaction", "native compaction usage could not be verified; do not repeat the operation", 502)
			}
			usage, err := modelUsageDifference(before, frame.Usage)
			n.currentUsage = usage
			return boundary, usage, err
		case "rate_limit_event":
			n.logQuotaEvent(ctx, line)
		default:
			return "", messages.Usage{}, protocol()
		}
	}
}
