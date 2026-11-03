// Package messages defines the gateway's finite Anthropic Messages dialect.
// Decoding a request does not assert that a CLI execution adapter is available.
package messages

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"

	"unbundle-go/src/internal/config"
	"unbundle-go/src/internal/jsonutil"
)

type Request struct {
	Model         string         `json:"model"`
	MaxTokens     int64          `json:"max_tokens"`
	Messages      []Message      `json:"messages"`
	System        jsontext.Value `json:"system,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	Tools         []Tool         `json:"tools,omitempty"`
	ToolChoice    *ToolChoice    `json:"tool_choice,omitempty"`
	Thinking      *Thinking      `json:"thinking,omitempty"`
	OutputConfig  *OutputConfig  `json:"output_config,omitempty"`
	Metadata      *Metadata      `json:"metadata,omitempty"`
	StopSequences []string       `json:"stop_sequences,omitempty"`
	// CacheTTL is derived from transport hints. The official CLI owns cache
	// breakpoints; annotations are removed before native history comparison.
	CacheTTL string `json:"-"`
}

type Message struct {
	Role string `json:"role"`
	// Raw content preserves string/array form, block order, whitespace, tool
	// arguments and nested results. It is never collapsed into chat text.
	Content      jsontext.Value `json:"content"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"`
	OutputConfig *OutputConfig  `json:"output_config,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type Tool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  jsontext.Value `json:"input_schema"`
	Type         string         `json:"type,omitempty"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"`
}

type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type Thinking struct {
	Type string `json:"type"`
}
type OutputConfig struct {
	Effort string `json:"effort"`
}

// Metadata is opaque transport data. UserID is never a session identifier.
type Metadata struct {
	UserID *string `json:"user_id,omitempty"`
}

type TextBlock struct {
	Type         string        `json:"type"`
	Text         *string       `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type ToolUseBlock struct {
	Type         string         `json:"type"`
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Input        jsontext.Value `json:"input"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"`
}

type ToolResultBlock struct {
	Type         string         `json:"type"`
	ToolUseID    string         `json:"tool_use_id"`
	Content      jsontext.Value `json:"content,omitempty"`
	IsError      *bool          `json:"is_error,omitempty"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"`
}

// Error contains stable, non-content diagnostics suitable for an API adapter.
type Error struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func (e *Error) Error() string            { return e.Code + ": " + e.Field + ": " + e.Message }
func invalid(field, message string) error { return &Error{"INVALID_REQUEST", field, message} }
func unsupported(field string) error {
	return &Error{"UNSUPPORTED_PARAMETER", field, "capability is not supported by this dialect"}
}

// Decode enforces syntax, structural tool relationships and deployment limits.
// Unknown fields (also nested), duplicate keys and extra JSON documents fail.
func Decode(r io.Reader, c config.Config) (Request, error) {
	var request Request
	if err := jsonutil.Decode(r, c.MaxRequestBytes, &request); err != nil {
		return Request{}, err
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	model, ok := c.Model(request.Model)
	if !ok {
		return Request{}, &Error{"UNKNOWN_MODEL", "model", "model is not in the configured catalog"}
	}
	if request.MaxTokens > model.OutputTokenLimit {
		return Request{}, invalid("max_tokens", "exceeds the deployment limit for this model")
	}
	outputs := []*OutputConfig{request.OutputConfig}
	for _, message := range request.Messages {
		outputs = append(outputs, message.OutputConfig)
	}
	for _, output := range outputs {
		if output != nil && !slices.Contains(model.Efforts, output.Effort) {
			return Request{}, unsupported("output_config.effort")
		}
	}
	return request.NormalizeCache()
}

func (r Request) Validate() error {
	if r.Model == "" {
		return invalid("model", "is required")
	}
	if r.MaxTokens <= 0 {
		return invalid("max_tokens", "must be a positive integer")
	}
	if len(r.Messages) == 0 {
		return invalid("messages", "must not be empty")
	}
	if len(r.StopSequences) > 0 {
		return unsupported("stop_sequences")
	}
	if r.Thinking != nil && r.Thinking.Type != "adaptive" {
		return unsupported("thinking")
	}
	if err := validateOutput(r.OutputConfig); err != nil {
		return err
	}
	if r.Metadata != nil && r.Metadata.UserID != nil && utf8.RuneCountInString(*r.Metadata.UserID) > 512 {
		return invalid("metadata.user_id", "exceeds 512 characters")
	}
	if len(r.System) > 0 {
		if err := textContent(r.System, false); err != nil {
			return err
		}
	}
	toolNames := make(map[string]bool)
	for _, tool := range r.Tools {
		if tool.Name == "" || toolNames[tool.Name] {
			return invalid("tools.name", "must be nonempty and unique")
		}
		toolNames[tool.Name] = true
		if tool.Type != "" && tool.Type != "custom" {
			return unsupported("tools.type")
		}
		if tool.InputSchema.Kind() != '{' || !tool.InputSchema.IsValid() {
			return invalid("tools.input_schema", "must be a JSON object")
		}
		if err := validateCache(tool.CacheControl); err != nil {
			return err
		}
	}
	if choice := r.ToolChoice; choice != nil {
		if choice.Type != "auto" && choice.Type != "none" {
			return unsupported("tool_choice.type")
		}
		if choice.Name != "" || choice.DisableParallelToolUse != nil {
			return unsupported("tool_choice")
		}
	}
	pending := make(map[string]bool)
	seen := make(map[string]bool)
	for i, message := range r.Messages {
		field := fmt.Sprintf("messages[%d]", i)
		if !slices.Contains([]string{"user", "assistant", "system"}, message.Role) {
			return invalid(field+".role", "unsupported role")
		}
		if err := validateCache(message.CacheControl); err != nil {
			return err
		}
		if message.OutputConfig != nil && message.Role != "system" {
			return invalid(field+".output_config", "requires the system role")
		}
		if err := validateOutput(message.OutputConfig); err != nil {
			return err
		}
		if len(pending) > 0 && message.Role != "user" {
			return invalid(field, "outstanding tool calls require results in the next user message")
		}
		blocks, err := Blocks(message.Content)
		if err != nil {
			return err
		}
		if len(blocks) == 0 && !(message.Role == "system" && message.OutputConfig != nil) {
			return invalid(field+".content", "must not be empty")
		}
		pastResults := false
		for _, raw := range blocks {
			kind, err := blockType(raw)
			if err != nil {
				return err
			}
			switch kind {
			case "text":
				if message.Role == "user" && len(pending) > 0 {
					return invalid(field, "tool results must precede text")
				}
				if err := textBlock(raw); err != nil {
					return err
				}
				pastResults = true
			case "tool_use":
				if message.Role != "assistant" {
					return invalid(field, "tool_use requires the assistant role")
				}
				var block ToolUseBlock
				if err := jsonutil.Object(raw, &block); err != nil {
					return err
				}
				if block.ID == "" || block.Name == "" || block.Input.Kind() != '{' || seen[block.ID] {
					return invalid(field, "tool_use requires a unique ID, name and object input")
				}
				if err := validateCache(block.CacheControl); err != nil {
					return err
				}
				seen[block.ID] = true
				pending[block.ID] = true
			case "tool_result":
				if message.Role != "user" || pastResults {
					return invalid(field, "tool_result requires the beginning of a user message")
				}
				var block ToolResultBlock
				if err := jsonutil.Object(raw, &block); err != nil {
					return err
				}
				if !pending[block.ToolUseID] {
					return invalid(field, "tool_result does not match an outstanding call")
				}
				if len(block.Content) > 0 {
					if err := resultContent(block.Content); err != nil {
						return err
					}
				}
				if err := validateCache(block.CacheControl); err != nil {
					return err
				}
				delete(pending, block.ToolUseID)
			case "thinking", "redacted_thinking":
				if message.Role != "assistant" {
					return invalid(field, "thinking requires assistant role")
				}
				if kind == "thinking" {
					var block struct {
						Type      string  `json:"type"`
						Thinking  *string `json:"thinking"`
						Signature *string `json:"signature"`
					}
					if err := jsonutil.Object(raw, &block); err != nil {
						return err
					}
					if block.Thinking == nil || block.Signature == nil {
						return invalid(field, "thinking requires content and signature")
					}
				} else {
					var block struct {
						Type string  `json:"type"`
						Data *string `json:"data"`
					}
					if err := jsonutil.Object(raw, &block); err != nil {
						return err
					}
					if block.Data == nil {
						return invalid(field, "redacted thinking requires data")
					}
				}
			default:
				return unsupported(field + ".content.type")
			}
		}
		if message.Role == "user" && len(pending) > 0 {
			return invalid(field, "must provide all outstanding tool results")
		}
	}
	if r.Messages[len(r.Messages)-1].Role == "assistant" {
		return unsupported("messages: assistant prefill")
	}
	return nil
}

// Blocks provides a view for validation/adaptation. The original Message.Content
// remains authoritative; string shorthand is not rewritten in stored history.
func Blocks(content jsontext.Value) ([]jsontext.Value, error) {
	if content.Kind() == '"' {
		var text string
		if err := json.Unmarshal(content, &text); err != nil {
			return nil, invalid("content", "invalid text")
		}
		block, err := json.Marshal(TextBlock{Type: "text", Text: &text})
		return []jsontext.Value{block}, err
	}
	if content.Kind() != '[' {
		return nil, invalid("content", "must be a string or block array")
	}
	var blocks []jsontext.Value
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, invalid("content", "invalid block array")
	}
	return blocks, nil
}

func blockType(raw jsontext.Value) (string, error) {
	var fields map[string]jsontext.Value
	if err := jsonutil.Object(raw, &fields); err != nil {
		return "", err
	}
	var kind string
	if err := json.Unmarshal(fields["type"], &kind); err != nil || kind == "" {
		return "", invalid("content.type", "is required")
	}
	return kind, nil
}

func textBlock(raw jsontext.Value) error {
	var block TextBlock
	if err := jsonutil.Object(raw, &block); err != nil {
		return err
	}
	if block.Type != "text" || block.Text == nil {
		return invalid("content", "requires a text block")
	}
	return validateCache(block.CacheControl)
}

func textContent(content jsontext.Value, allowEmpty bool) error {
	blocks, err := Blocks(content)
	if err != nil {
		return err
	}
	if len(blocks) == 0 && !allowEmpty {
		return invalid("content", "must not be empty")
	}
	for _, block := range blocks {
		if err := textBlock(block); err != nil {
			return err
		}
	}
	return nil
}

func validateCache(cache *CacheControl) error {
	if cache != nil && (cache.Type != "ephemeral" || !slices.Contains([]string{"", "5m", "1h"}, cache.TTL)) {
		return unsupported("cache_control")
	}
	return nil
}

func validateOutput(output *OutputConfig) error {
	if output != nil && !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, output.Effort) {
		return unsupported("output_config.effort")
	}
	return nil
}
