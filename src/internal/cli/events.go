package cli

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strings"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

type block struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	Signature string         `json:"signature,omitempty"`
	Data      string         `json:"data,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     jsontext.Value `json:"input,omitempty"`
	partial   string
}

type wireMessage struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    []block        `json:"content"`
	StopReason *string        `json:"stop_reason"`
	Usage      messages.Usage `json:"usage"`
}

type wireEvent struct {
	Type         string       `json:"type"`
	Message      *wireMessage `json:"message"`
	Index        int          `json:"index"`
	ContentBlock *block       `json:"content_block"`
	Delta        struct {
		Type        string  `json:"type"`
		Text        string  `json:"text"`
		Thinking    string  `json:"thinking"`
		Signature   string  `json:"signature"`
		PartialJSON string  `json:"partial_json"`
		StopReason  *string `json:"stop_reason"`
	} `json:"delta"`
	Usage messages.Usage `json:"usage"`
}

type parser struct {
	nativeID string
	model    string
	emit     func(Event) error
	response messages.Response
	blocks   []block
	closed   []bool
	started  bool
	terminal bool
	result   bool
	// Tool events are buffered until the native MCP handoff is verified.
	tools    map[string]string // native name -> caller name
	streamed map[string]bool
	servers  map[string]bool
}

func protocol() *Error {
	return Failure("CLI_PROTOCOL_ERROR", "output", "official CLI returned an unsupported or inconsistent event", 502)
}

func (p *parser) send(kind string, data any) error {
	if p.emit == nil {
		return nil
	}
	return p.emit(Event{Type: kind, Data: data})
}

func (p *parser) start(m wireMessage) error {
	if m.ID == "" || m.Role != "assistant" || p.started {
		return protocol()
	}
	p.started = true
	p.response = messages.Response{ID: m.ID, Type: "message", Role: "assistant", Model: p.model, Content: []jsontext.Value{}}
	if err := mergeUsage(&p.response.Usage, m.Usage); err != nil {
		return err
	}
	return p.send("message_start", map[string]any{"type": "message_start", "message": p.response})
}

func (p *parser) begin(index int, b block) error {
	if !p.started || p.terminal || index != len(p.blocks) || index > 0 && !p.closed[index-1] {
		return protocol()
	}
	if b.Type != "text" && b.Type != "thinking" && b.Type != "redacted_thinking" && b.Type != "tool_use" {
		return protocol()
	}
	if b.Type != "text" && b.Text != "" || b.Type != "thinking" && (b.Thinking != "" || b.Signature != "") || b.Type != "redacted_thinking" && b.Data != "" {
		return protocol()
	}
	p.blocks = append(p.blocks, b)
	p.closed = append(p.closed, false)
	if b.Type == "tool_use" {
		if b.ID == "" || p.tools[b.Name] == "" || b.Input.Kind() != '{' {
			return protocol()
		}
		return nil
	}
	// Text/thinking fields are required even when their initial values are empty.
	content := map[string]any{"type": b.Type}
	switch b.Type {
	case "text":
		content["text"] = b.Text
	case "thinking":
		content["thinking"] = b.Thinking
		content["signature"] = b.Signature
	case "redacted_thinking":
		content["data"] = b.Data
	}
	return p.send("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": content})
}

func (p *parser) delta(index int, kind, value string) error {
	if p.terminal || index < 0 || index >= len(p.blocks) || p.closed[index] {
		return protocol()
	}
	b := &p.blocks[index]
	field := ""
	switch {
	case kind == "input_json_delta" && b.Type == "tool_use":
		b.partial += value
		return nil
	case kind == "text_delta" && b.Type == "text":
		b.Text += value
		field = "text"
	case kind == "thinking_delta" && b.Type == "thinking":
		b.Thinking += value
		field = "thinking"
	case kind == "signature_delta" && b.Type == "thinking":
		b.Signature += value
		field = "signature"
	default:
		return protocol()
	}
	return p.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": kind, field: value}})
}

func (p *parser) closeBlock(index int) error {
	if index < 0 || index >= len(p.blocks) || p.closed[index] {
		return protocol()
	}
	p.closed[index] = true
	if b := &p.blocks[index]; b.Type == "tool_use" {
		if b.partial != "" {
			b.Input = jsontext.Value(b.partial)
			b.partial = ""
		}
		if b.Input.Kind() != '{' || !b.Input.IsValid() {
			return protocol()
		}
		return nil
	}
	return p.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func mergeUsage(dst *messages.Usage, src messages.Usage) error {
	for _, pair := range [][2]*int64{{dst.InputTokens, src.InputTokens}, {dst.OutputTokens, src.OutputTokens}, {dst.CacheReadInputTokens, src.CacheReadInputTokens}, {dst.CacheCreationInputTokens, src.CacheCreationInputTokens}} {
		if pair[1] != nil && (*pair[1] < 0 || pair[0] != nil && *pair[1] < *pair[0]) {
			return protocol()
		}
	}
	if src.InputTokens != nil {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens != nil {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheReadInputTokens != nil {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.CacheCreationInputTokens != nil {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	return nil
}

func (p *parser) finish() error {
	if !p.started || p.response.StopReason == nil {
		return protocol()
	}
	reason := *p.response.StopReason
	if reason != messages.StopEndTurn && reason != messages.StopMaxTokens && reason != messages.StopRefusal && reason != messages.StopToolUse {
		return protocol()
	}
	text := false
	ids := make(map[string]bool)
	content := make([]jsontext.Value, 0, len(p.blocks))
	for i, b := range p.blocks {
		if !p.closed[i] {
			return protocol()
		}
		if b.Type == "text" && b.Text != "" {
			text = true
		}
		var raw []byte
		var err error
		if b.Type == "text" {
			raw, err = json.Marshal(map[string]string{"type": b.Type, "text": b.Text})
		} else if b.Type == "thinking" {
			raw, err = json.Marshal(map[string]string{"type": b.Type, "thinking": b.Thinking, "signature": b.Signature})
		} else if b.Type == "tool_use" {
			if ids[b.ID] || reason != messages.StopToolUse || p.tools[b.Name] == "" {
				return protocol()
			}
			ids[b.ID] = true
			raw, err = json.Marshal(messages.ToolUseBlock{Type: "tool_use", ID: b.ID, Name: p.tools[b.Name], Input: b.Input})
		} else {
			raw, err = json.Marshal(b)
		}
		if err != nil {
			return protocol()
		}
		content = append(content, raw)
	}
	if reason == messages.StopToolUse && len(ids) == 0 {
		return protocol()
	}
	if !text && reason == messages.StopEndTurn {
		return Failure("CLI_EMPTY_OUTPUT", "output", "CLI completed without text output", 502)
	}
	p.response.Content = content
	p.terminal = true
	return nil
}

func (p *parser) accept(line []byte) error {
	var frame struct {
		Type            string           `json:"type"`
		SessionID       string           `json:"session_id"`
		Subtype         string           `json:"subtype"`
		Event           jsontext.Value   `json:"event"`
		Message         wireMessage      `json:"message"`
		IsError         bool             `json:"is_error"`
		APIError        bool             `json:"is_api_error_message"`
		APIErrorStatus  int              `json:"api_error_status"`
		APIErrorKind    string           `json:"api_error"`
		Usage           messages.Usage   `json:"usage"`
		Tools           []string         `json:"tools"`
		MCPServers      []jsontext.Value `json:"mcp_servers"`
		ParentToolUseID *string          `json:"parent_tool_use_id"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return protocol()
	}
	if frame.ParentToolUseID != nil {
		return protocol()
	}
	switch frame.Type {
	case "system":
		if frame.Subtype == "compact_boundary" {
			return Failure("NATIVE_COMPACTION_UNSUPPORTED", "inference", "native compaction is not supported at this stage", 502)
		}
		if frame.Subtype == "init" {
			if frame.SessionID != p.nativeID {
				return Failure("CLI_SESSION_ID_MISMATCH", "startup", "official CLI did not confirm the requested native session", 502)
			}
			if len(frame.Tools) != len(p.tools) {
				return Failure("CLI_TOOL_SET_MISMATCH", "startup", "CLI did not load exactly the caller tool set", 502)
			}
			for _, name := range frame.Tools {
				if p.tools[name] == "" {
					return Failure("CLI_TOOLS_ENABLED", "startup", "CLI reported a tool outside the caller tool set", 502)
				}
			}
			for _, raw := range frame.MCPServers {
				var server struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(raw, &server) != nil || !p.servers[server.Name] {
					return protocol()
				}
			}
		}
		return nil
	case "assistant":
		if frame.APIError {
			return upstreamFailure(frame.APIErrorStatus, frame.APIErrorKind)
		}
		if frame.Message.StopReason == nil {
			return nil
		} // incomplete snapshot
		return p.snapshot(frame.Message)
	case "result":
		if frame.SessionID != p.nativeID {
			return Failure("CLI_SESSION_ID_MISMATCH", "output", "official CLI completed a different native session", 502)
		}
		if p.result {
			return protocol()
		}
		if err := mergeUsage(&p.response.Usage, frame.Usage); err != nil {
			return err
		}
		if frame.IsError || frame.Subtype != "success" {
			return upstreamFailure(0, frame.Subtype)
		}
		if !p.terminal {
			return protocol()
		}
		p.result = true
		return nil
	case "stream_event":
		if p.result {
			return protocol()
		}
		// System notifications may use a string event; only stream_event
		// frames carry an Anthropic event object.
		var e wireEvent
		if json.Unmarshal(frame.Event, &e) != nil {
			return protocol()
		}
		switch e.Type {
		case "message_start":
			if e.Message == nil {
				return protocol()
			}
			return p.start(*e.Message)
		case "content_block_start":
			if e.ContentBlock == nil {
				return protocol()
			}
			return p.begin(e.Index, *e.ContentBlock)
		case "content_block_delta":
			value := e.Delta.Text
			if e.Delta.Type == "thinking_delta" {
				value = e.Delta.Thinking
			}
			if e.Delta.Type == "signature_delta" {
				value = e.Delta.Signature
			}
			if e.Delta.Type == "input_json_delta" {
				value = e.Delta.PartialJSON
			}
			return p.delta(e.Index, e.Delta.Type, value)
		case "content_block_stop":
			if err := p.closeBlock(e.Index); err != nil {
				return err
			}
			if b := p.blocks[e.Index]; b.Type == "tool_use" {
				if p.streamed == nil {
					p.streamed = make(map[string]bool)
				}
				p.streamed[b.ID] = true
			}
			return nil
		case "message_delta":
			if !p.started || e.Delta.StopReason == nil {
				return protocol()
			}
			if p.response.StopReason != nil && *p.response.StopReason != *e.Delta.StopReason {
				return protocol()
			}
			p.response.StopReason = e.Delta.StopReason
			return mergeUsage(&p.response.Usage, e.Usage)
		case "message_stop":
			return p.finish()
		case "ping":
			return nil
		default:
			return protocol()
		}
	case "rate_limit_event":
		return nil // advisory, not a terminal failure
	default:
		return protocol()
	}
}

// A complete snapshot verifies streamed blocks and fills only missing suffixes.
// It never re-emits text already delivered through partial events.
func (p *parser) snapshot(m wireMessage) error {
	if !p.started {
		if err := p.start(m); err != nil {
			return err
		}
	}
	if m.ID != p.response.ID || m.Role != "assistant" || len(m.Content) < len(p.blocks) {
		return protocol()
	}
	if err := mergeUsage(&p.response.Usage, m.Usage); err != nil {
		return err
	}
	if p.response.StopReason != nil && *p.response.StopReason != *m.StopReason {
		return protocol()
	}
	if p.terminal {
		if !jsonutil.Equal(p.blocks, m.Content) {
			return protocol()
		}
		return nil
	}
	for i, expected := range m.Content {
		if i == len(p.blocks) {
			if err := p.begin(i, expected); err != nil {
				return err
			}
		} else if !jsonutil.Equal(p.blocks[i], expected) {
			actual := p.blocks[i]
			if actual.Type == "tool_use" {
				if p.closed[i] || actual.ID != expected.ID || actual.Name != expected.Name || expected.Input.Kind() != '{' || !expected.Input.IsValid() || actual.partial != "" && !jsonutil.Equal(jsontext.Value(actual.partial), expected.Input) {
					return protocol()
				}
				p.blocks[i] = expected
				if err := p.closeBlock(i); err != nil {
					return err
				}
				continue
			}
			if p.closed[i] || actual.Type != expected.Type || actual.Data != expected.Data ||
				!strings.HasPrefix(expected.Text, actual.Text) || !strings.HasPrefix(expected.Thinking, actual.Thinking) || !strings.HasPrefix(expected.Signature, actual.Signature) {
				return protocol()
			}
			for _, suffix := range []struct{ kind, value string }{
				{"text_delta", strings.TrimPrefix(expected.Text, actual.Text)},
				{"thinking_delta", strings.TrimPrefix(expected.Thinking, actual.Thinking)},
				{"signature_delta", strings.TrimPrefix(expected.Signature, actual.Signature)},
			} {
				if suffix.value != "" {
					if err := p.delta(i, suffix.kind, suffix.value); err != nil {
						return err
					}
				}
			}
		}
		if !p.closed[i] {
			if err := p.closeBlock(i); err != nil {
				return err
			}
		}
	}
	p.response.StopReason = m.StopReason
	return p.finish()
}

func upstreamFailure(status int, kind string) *Error {
	e := Failure("UPSTREAM_FAILED", "inference", "official CLI reported an upstream failure", http.StatusBadGateway)
	switch {
	case status == 401 || status == 403:
		e.Code, e.Message = "UPSTREAM_AUTH_FAILED", "official CLI authentication or account authorization failed"
	case status == 429:
		e.Code, e.Type, e.Retryable, e.Status = "UPSTREAM_RATE_LIMITED", "rate_limit_error", true, 429
	case status == 529 || status == 503:
		e.Code, e.Type, e.Retryable, e.Status = "UPSTREAM_OVERLOADED", "overloaded_error", true, 503
	case status == 400 || status == 413 || status == 422:
		e.Code, e.Message = "UPSTREAM_REQUEST_REJECTED", "upstream rejected the request"
	case status == 404:
		e.Code, e.Message = "UPSTREAM_MODEL_UNAVAILABLE", "upstream model or endpoint is unavailable"
	case status >= 500:
		e.Code, e.Retryable = "UPSTREAM_TEMPORARY_FAILURE", true
	case kind == "error_max_turns":
		e.Code = "CLI_MAX_TURNS"
	case kind == "error_max_budget_usd":
		e.Code = "CLI_BUDGET_EXHAUSTED"
	case kind == "max_output_tokens":
		e.Code = "CLI_OUTPUT_EXHAUSTED"
	}
	return e
}
