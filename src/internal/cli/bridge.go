package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	stdjson "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

const mcpServerName = "local"

type toolDefinition struct {
	schema *jsonschema.Schema
}

// The MCP handler only waits for a real local result. It never runs a tool.
type toolCall struct {
	id     string
	name   string
	input  jsontext.Value
	result chan *mcp.CallToolResult
	ctx    context.Context
}

type bridge struct {
	names               map[string]string
	definitions         map[string]toolDefinition
	calls               chan *toolCall
	errors              chan error
	server              *http.Server
	handler             *mcp.StreamableHTTPHandler
	config              []byte
	ctx                 context.Context
	cancel              context.CancelFunc
	onFailure           func()
	webSchema           *jsonschema.Schema
	regexFailed         atomic.Bool
	address             string
	auth                string
	instructionMu       sync.RWMutex
	instructionHandlers map[string]*mcp.StreamableHTTPHandler
	instructionRevision int
	instructionName     string
}

func prepareBridge(request messages.Request, webSearch bool) (*bridge, error) {
	b := &bridge{names: make(map[string]string), definitions: make(map[string]toolDefinition), calls: make(chan *toolCall), errors: make(chan error, 1)}
	if request.ToolChoice != nil && request.ToolChoice.Type == "none" {
		return b, nil
	}
	if webSearch {
		declared := false
		for _, tool := range request.Tools {
			if tool.Name == "WebSearch" {
				declared = true
			}
		}
		if !declared {
			// Gateway clients may hide their own WebSearch tool. Deployment opt-in
			// exposes the server CLI's official search capability independently.
			request.Tools = append(slices.Clone(request.Tools), messages.Tool{Name: "WebSearch", InputSchema: jsontext.Value(`{"type":"object","properties":{"query":{"type":"string","minLength":2},"allowed_domains":{"type":"array","items":{"type":"string"}},"blocked_domains":{"type":"array","items":{"type":"string"}}},"required":["query"],"additionalProperties":false}`)})
		}
	}
	for _, tool := range request.Tools {
		// Stable safe aliases avoid MCP/CLI name restrictions without truncating
		// or changing the caller's name, description or schema.
		sum := sha256.Sum256([]byte(tool.Name))
		alias := fmtAlias(sum)
		if _, exists := b.definitions[alias]; exists {
			return nil, protocol()
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(tool.InputSchema))
		object, ok := document.(map[string]any)
		if err != nil || !ok || object["type"] != "object" {
			return nil, &messages.Error{Code: "INVALID_TOOL_SCHEMA", Field: "tools.input_schema", Message: "requires a supported object JSON schema"}
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseRegexpEngine(b.compileRegexp)
		compiler.UseLoader(jsonschema.SchemeURLLoader{}) // refuse all external file/network references
		const schemaURL = "urn:unbundle:tool"
		if err := compiler.AddResource(schemaURL, document); err != nil {
			return nil, invalidSchema(tool.Name)
		}
		resolved, err := compiler.Compile(schemaURL)
		if err != nil || b.regexFailed.Load() {
			return nil, invalidSchema(tool.Name)
		}
		if webSearch && tool.Name == "WebSearch" {
			// Opt-in routing is limited to the official CLI's public arguments.
			properties, ok := object["properties"].(map[string]any)
			if !ok || properties["query"] == nil {
				return nil, invalidSchema(tool.Name)
			}
			query, ok := properties["query"].(map[string]any)
			if !ok || query["type"] != "string" {
				return nil, invalidSchema(tool.Name)
			}
			for name := range properties {
				if name != "query" && name != "allowed_domains" && name != "blocked_domains" {
					return nil, invalidSchema(tool.Name)
				}
				if name != "query" {
					property, ok := properties[name].(map[string]any)
					if !ok || property["type"] != "array" {
						return nil, invalidSchema(tool.Name)
					}
					items, ok := property["items"].(map[string]any)
					if !ok || items["type"] != "string" {
						return nil, invalidSchema(tool.Name)
					}
				}
			}
			b.webSchema = resolved
			b.names["WebSearch"] = "WebSearch"
			continue
		}
		b.definitions[alias] = toolDefinition{resolved}
		b.names["mcp__"+mcpServerName+"__"+alias] = tool.Name
	}
	return b, nil
}

func fmtAlias(sum [32]byte) string {
	return "t_" + hex.EncodeToString(sum[:])
}

func (b *bridge) fail(err error) {
	select {
	case b.errors <- err:
	default:
	}
	if b.onFailure != nil {
		b.onFailure()
	}
}

func (b *bridge) start(ctx context.Context, request messages.Request, maxBytes int64, wait time.Duration) error {
	b.ctx, b.cancel = context.WithCancel(ctx)
	s := mcp.NewServer(&mcp.Implementation{Name: "unbundle-local-tools", Version: "1"}, &mcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, tool := range request.Tools {
		sum := sha256.Sum256([]byte(tool.Name))
		alias := fmtAlias(sum)
		if _, ok := b.definitions[alias]; !ok {
			continue
		}
		s.AddTool(&mcp.Tool{Name: alias, Description: tool.Description, InputSchema: stdjson.RawMessage(tool.InputSchema)}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, _ := req.Params.Meta["claudecode/toolUseId"].(string)
			call := &toolCall{id: id, name: req.Params.Name, input: jsontext.Value(req.Params.Arguments), result: make(chan *mcp.CallToolResult, 1), ctx: ctx}
			if id == "" || len(id) > 256 || !call.input.IsValid() || call.input.Kind() != '{' {
				b.fail(protocol())
				return nil, errors.New("invalid native tool call")
			}
			select {
			case b.calls <- call:
			case <-ctx.Done():
				b.fail(protocol())
				return nil, errors.New("native tool connection lost")
			case <-b.ctx.Done():
				return nil, errors.New("session closed")
			}
			select {
			case result := <-call.result:
				return result, nil
			case <-ctx.Done():
				b.fail(Failure("MCP_CONNECTION_LOST", "tools", "native tool connection was lost; local execution outcome is unknown", 502))
				return nil, errors.New("native tool connection lost")
			case <-b.ctx.Done():
				return nil, errors.New("session closed")
			}
		})
	}
	b.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.close()
		return Failure("MCP_START_FAILED", "startup", "could not bind private MCP listener", 503)
	}
	token := rand.Text() + rand.Text()
	b.address, b.auth = listener.Addr().String(), "Bearer "+token
	b.instructionHandlers = make(map[string]*mcp.StreamableHTTPHandler)
	digest := sha256.Sum256([]byte("Bearer " + token))
	// Bound connections independently of the SDK's protocol/session handling.
	slots := make(chan struct{}, 64)
	b.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return b.ctx }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(actual[:], digest[:]) != 1 {
			w.WriteHeader(401)
			return
		}
		handler := b.handler
		if r.URL.Path != "/mcp" {
			b.instructionMu.RLock()
			handler = b.instructionHandlers[strings.TrimPrefix(r.URL.Path, "/instructions/")]
			b.instructionMu.RUnlock()
			if !strings.HasPrefix(r.URL.Path, "/instructions/") {
				handler = nil
			}
		}
		if handler == nil || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.WriteHeader(503)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		defer r.Body.Close()
		handler.ServeHTTP(w, r)
	})}
	servers := make(map[string]any)
	if len(b.definitions) > 0 {
		servers[mcpServerName] = map[string]any{"type": "http", "url": "http://" + b.address + "/mcp", "headers": map[string]string{"Authorization": b.auth}, "timeout": wait.Milliseconds()}
	}
	b.config, err = json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		_ = listener.Close()
		b.close()
		return protocol()
	}
	go func() {
		if err := b.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.fail(Failure("MCP_SERVER_FAILED", "tools", "private MCP listener failed", 502))
		}
	}()
	return nil
}

func (b *bridge) close() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.server != nil {
		_ = b.server.Close()
	}
}

func (b *bridge) verify(call *toolCall, expected block) error {
	_, ok := b.definitions[call.name]
	if !ok || expected.ID != call.id || expected.Name != "mcp__"+mcpServerName+"__"+call.name || !jsonutil.Equal(call.input, expected.Input) {
		return Failure("MCP_CALL_MISMATCH", "tools", "native MCP ID, name or arguments do not match the observed call", 502)
	}
	return b.validate(expected)
}

func (b *bridge) validate(expected block) error {
	var schema *jsonschema.Schema
	if expected.Name == "WebSearch" && b.webSchema != nil {
		schema = b.webSchema
	} else {
		alias := strings.TrimPrefix(expected.Name, "mcp__"+mcpServerName+"__")
		definition, ok := b.definitions[alias]
		if !ok {
			return protocol()
		}
		schema = definition.schema
	}
	err := validateArguments(schema, expected.Input)
	if b.regexFailed.Load() {
		return Failure("TOOL_SCHEMA_PATTERN_FAILED", "tools", "tool schema pattern matching failed or exceeded its time limit", 502)
	}
	return err
}

func validateArguments(schema *jsonschema.Schema, raw jsontext.Value) error {
	input, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return protocol()
	}
	if err := schema.Validate(input); err != nil {
		return Failure("TOOL_ARGUMENTS_INVALID", "tools", "tool arguments do not satisfy the caller schema", 502)
	}
	return nil
}

func toolResult(raw messages.ToolResultBlock) (*mcp.CallToolResult, error) {
	result := &mcp.CallToolResult{Content: []mcp.Content{}, IsError: raw.IsError != nil && *raw.IsError}
	if len(raw.Content) == 0 {
		return result, nil
	}
	blocks, err := messages.Blocks(raw.Content)
	if err != nil {
		return nil, err
	}
	for _, item := range blocks {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &kind) != nil {
			return nil, protocol()
		}
		switch kind.Type {
		case "text":
			var text messages.TextBlock
			if json.Unmarshal(item, &text) != nil || text.Text == nil {
				return nil, protocol()
			}
			result.Content = append(result.Content, &mcp.TextContent{Text: *text.Text})
		case "image":
			data, media, err := messages.DecodeImage(item)
			if err != nil {
				return nil, err
			}
			result.Content = append(result.Content, &mcp.ImageContent{Data: data, MIMEType: media})
		default:
			return nil, protocol()
		}
	}
	return result, nil
}

func invalidSchema(tool string) *messages.Error {
	return &messages.Error{Code: "INVALID_TOOL_SCHEMA", Field: "tools." + tool + ".input_schema", Message: "schema is invalid or unsupported for tool " + tool}
}
