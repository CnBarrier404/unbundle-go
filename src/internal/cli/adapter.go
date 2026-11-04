package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"time"

	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

func (a *Adapter) Run(ctx context.Context, request messages.Request, identity session.Identity, emit func(Event) error) (response messages.Response, err error) {
	if err := request.Validate(); err != nil {
		return response, err
	}
	request, err = request.NormalizeCache()
	if err != nil {
		return response, err
	}
	if err := ValidateTurn(request); err != nil {
		return response, err
	}
	entry, match, previous, err := a.acquire(ctx, request, identity)
	if err != nil {
		return response, err
	}
	// The reservation gives this request exclusive ownership of native protocol
	// state. Pool locks protect only boundaries and resource accounting.
	n := entry.native
	accepted := false
	var content jsontext.Value
	defer func() {
		if n != nil && (accepted && err != nil || a.ctx.Err() != nil) {
			n.close() // never wait for process or MCP cleanup under the pool lock
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		defer a.running.Done()
		entry.busy = false
		if !accepted {
			if match.Kind == session.New {
				delete(a.entries, entry.boundary.ID)
				return
			}
			entry.boundary.State = previous
		} else if err != nil {
			entry.boundary.State = session.Closed
			var failure *Error
			if errors.As(err, &failure) {
				failure.Usage = n.currentUsage
				copy := *failure
				entry.failure = &copy
			}
		} else {
			entry.boundary.Assistant = messages.Message{Role: "assistant", Content: content}
			entry.boundary.State = session.Idle
			if *response.StopReason == messages.StopToolUse {
				entry.boundary.State = session.WaitingTools
			}
		}
		entry.lastUsed = time.Now()
		a.reapLocked()
	}()
	var user *messages.Message
	var updates []instructionUpdate
	for i := range match.Delta {
		if match.Delta[i].Role == "user" && user == nil {
			user = &match.Delta[i]
		} else if match.Delta[i].Role == "system" {
			if match.Kind == session.Continue {
				updates = append(updates, instructionUpdate{Index: len(request.Messages) - len(match.Delta) + i, Role: "system", Content: match.Delta[i].Content})
			}
		} else {
			return response, validation("NATIVE_INPUT_UNSUPPORTED", "only one incremental user message is supported")
		}
	}
	if user == nil {
		return response, validation("NATIVE_INPUT_UNSUPPORTED", "only one incremental user message is supported")
	}
	if match.Kind == session.New {
		model, _ := a.Config.Model(request.Model)
		b, err := prepareBridge(request, model.WebSearch)
		if err != nil {
			return response, err
		}
		n, err = (Runner{Config: a.Config}).start(a.ctx, ctx, request, b)
		if err != nil {
			return response, err
		}
		a.mu.Lock()
		entry.native = n
		a.mu.Unlock()
	}
	// Reject malformed tool continuations before replacing the accepted boundary
	// or touching the original MCP waiters. Validation errors preserve the session.
	if len(n.expected) > 0 {
		if err := n.receiveResults(*user); err != nil {
			return response, err
		}
	}
	if ctx.Err() != nil {
		if match.Kind == session.New {
			n.close()
		}
		return response, cancelled(ctx)
	}
	a.mu.Lock()
	if entry.boundary.Identity.RequestClass == "" {
		entry.boundary.Identity.RequestClass = identity.RequestClass
	}
	if entry.boundary.Identity.ParentAgentID == "" {
		entry.boundary.Identity.ParentAgentID = identity.ParentAgentID
	}
	entry.boundary.Request, entry.boundary.Assistant = request, messages.Message{}
	a.mu.Unlock()
	accepted = true
	response, err = n.advance(ctx, request, *user, updates, emit)
	if err != nil {
		if ctx.Err() != nil {
			err = cancelled(ctx)
		}
		return response, err
	}
	content, err = json.Marshal(response.Content)
	if err != nil {
		return messages.Response{}, protocol()
	}
	if *response.StopReason == messages.StopMaxTokens || *response.StopReason == messages.StopRefusal {
		n.close()
	}
	return response, nil
}

func validation(code, message string) *Error {
	err := Failure(code, "session", message, 409)
	err.Type, err.Outcome = "invalid_request_error", "not_started"
	return err
}
