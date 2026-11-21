package cli

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

// Apply and acknowledge per-turn settings before any user input or tool result.
// Explicit nulls clear previous overrides when the caller returns to defaults.
// The CLI owns when effort changes take effect within an ongoing tool loop.
func (n *native) updateThinking(ctx context.Context, request messages.Request) error {
	settings := map[string]any{"effortLevel": nil, "alwaysThinkingEnabled": nil, "showThinkingSummaries": true}
	if request.OutputConfig != nil {
		settings["effortLevel"] = request.OutputConfig.Effort
	}
	if request.Thinking != nil {
		settings["alwaysThinkingEnabled"] = true
		if request.Thinking.Display == "updates" {
			settings["showThinkingSummaries"] = false
		}
	}
	value, err := json.Marshal(settings)
	if err != nil {
		return protocol()
	}
	if jsonutil.Equal(n.thinkingSettings, value) {
		return nil
	}
	if _, err := n.control(ctx, map[string]any{"subtype": "apply_flag_settings", "settings": settings}); err != nil {
		return err
	}
	n.thinkingSettings = value
	n.logger.Debug("native_thinking_updated")
	return nil
}

// Control messages are serialized with inference. Acknowledgement is required
// before submitting user input or releasing any pending local tool result.
func (n *native) control(ctx context.Context, request any, observers ...func([]byte) (bool, error)) (jsontext.Value, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id := "control_" + rand.Text()
	frame, err := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": request})
	if err != nil {
		return nil, protocol()
	}
	if _, err := n.stdin.Write(append(frame, '\n')); err != nil {
		return nil, Failure("CLI_CONTROL_FAILED", "instructions", "could not submit native instruction update", 502)
	}
	var output int64
	for {
		line, err := n.maintenanceLine(ctx, &output)
		if err != nil {
			return nil, err
		}
		for _, observe := range observers {
			handled, err := observe(line)
			if err != nil {
				return nil, err
			}
			if handled {
				line = nil
				break
			}
		}
		if line == nil {
			continue
		}
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string         `json:"subtype"`
				RequestID string         `json:"request_id"`
				Response  jsontext.Value `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &frame) != nil {
			return nil, protocol()
		}
		switch frame.Type {
		case "control_response":
			if frame.Response.RequestID != id || frame.Response.Subtype != "success" {
				return nil, Failure("CLI_CONTROL_REJECTED", "control", "official CLI rejected the native control request", 502)
			}
			return frame.Response.Response, nil
		case "rate_limit_event":
			n.logQuotaEvent(ctx, line)
		case "system", "tool_progress", "command_lifecycle":
			if err := n.p.accept(line); err != nil {
				return nil, err
			}
		case "assistant", "stream_event":
			// A streamed tool handoff can precede its matching snapshot or
			// final stream frames on stdout. Verify these against the completed
			// parser; never discard them or allow a new assistant turn here.
			if !n.p.terminal {
				return nil, protocol()
			}
			if err := n.p.accept(line); err != nil {
				return nil, err
			}
		default:
			return nil, protocol()
		}
	}
}

// Instruction updates and compaction consume the same bounded event source.
// Late serial MCP calls are still checked against the original tool batch.
func (n *native) maintenanceLine(ctx context.Context, output *int64) ([]byte, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, Failure("CLI_CONTROL_TIMEOUT", "control", "native operation did not finish before its deadline", 504)
		case <-n.ctx.Done():
			return nil, Failure("NATIVE_SESSION_LOST", "control", "native session ended during maintenance", 502)
		case err := <-n.bridge.errors:
			return nil, err
		case err := <-n.exit:
			return nil, err
		case call := <-n.bridge.calls:
			expected, ok := n.expected[call.id]
			if !ok || n.pending[call.id] != nil || n.returned[call.id] {
				return nil, protocol()
			}
			if err := n.bridge.verify(call, expected); err != nil {
				return nil, err
			}
			if n.interrupting {
				call.interrupted.Store(true)
			}
			n.pending[call.id] = call
		case line := <-n.lines:
			*output += int64(len(line)) + 1
			if *output > n.config.Runtime.MaxOutputBytes {
				return nil, Failure("CLI_OUTPUT_LIMIT", "control", "native maintenance output exceeded deployment limit", 502)
			}
			return line, nil
		}
	}
}

func (n *native) writeFrame(value any) error {
	frame, err := json.Marshal(value)
	if err != nil {
		return protocol()
	}
	if _, err = n.stdin.Write(append(frame, '\n')); err != nil {
		return Failure("CLI_INPUT_FAILED", "input", "could not submit native maintenance input", 502)
	}
	return nil
}
