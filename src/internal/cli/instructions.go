package cli

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"unbundle-go/src/internal/logging"
	"unbundle-go/src/internal/session"
)

// New instruction servers expose no tools. Their authenticated initialize
// response carries only verified, appended caller system messages.
func (b *bridge) stageInstructions(updates []session.InstructionUpdate) (string, any, error) {
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

func (n *native) updateInstructions(ctx context.Context, updates []session.InstructionUpdate) error {
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
