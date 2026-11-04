package cli

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"unbundle-go/src/internal/logging"
	"unbundle-go/src/internal/session"
)

type instructionUpdate = session.InstructionUpdate

// New instruction servers expose no tools. Their authenticated initialize
// response carries only verified, appended caller system messages.
func (b *bridge) stageInstructions(updates []instructionUpdate) (string, any, error) {
	payload, err := json.Marshal(map[string]any{"kind": "caller_instruction_updates", "updates": updates})
	if err != nil {
		return "", nil, protocol()
	}
	b.instructionRevision++
	name := "caller_instructions_" + strconv.Itoa(b.instructionRevision)
	logger := logging.SDK(b.logger)
	server := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, &mcp.ServerOptions{Instructions: string(payload), Logger: logger})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger})
	b.instructionMu.Lock()
	b.instructionHandlers[name] = handler
	b.instructionMu.Unlock()
	config := map[string]any{"type": "http", "url": "http://" + b.address + "/instructions/" + name, "headers": map[string]string{"Authorization": b.auth}}
	return name, config, nil
}

func (n *native) updateInstructions(ctx context.Context, updates []instructionUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	all := append(slices.Clone(n.instructions), updates...)
	name, config, err := n.bridge.stageInstructions(all)
	if err != nil {
		return err
	}
	// An initialization notification may precede the control acknowledgement.
	n.p.servers[name] = true
	response, err := n.control(ctx, map[string]any{"subtype": "mcp_set_servers", "servers": map[string]any{name: config}})
	if err != nil {
		return err
	}
	var result struct {
		Added  []string          `json:"added"`
		Errors map[string]string `json:"errors"`
	}
	if json.Unmarshal(response, &result) != nil || len(result.Errors) > 0 || !slices.Contains(result.Added, name) {
		return Failure("CLI_INSTRUCTION_UPDATE_FAILED", "instructions", "official CLI did not confirm caller instructions; no input or tool result was submitted", 502)
	}
	n.bridge.instructionMu.Lock()
	for previous := range n.bridge.instructionHandlers {
		if previous != name {
			delete(n.bridge.instructionHandlers, previous)
		}
	}
	n.bridge.instructionMu.Unlock()
	n.bridge.instructionName = name
	n.instructions = all
	logging.From(ctx).Debug("caller_instructions_updated", "update_count", len(updates))
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
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Response  struct {
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
		case "system", "rate_limit_event":
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
		case "tool_progress":
			if n.expected[frame.ToolUseID].ID == "" {
				return nil, protocol()
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
