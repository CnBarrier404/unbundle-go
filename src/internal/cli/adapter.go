package cli

import (
	"context"
	"encoding/json/v2"
	"errors"
	"sync"

	"unbundle-go/src/internal/config"
	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

// Adapter deliberately retains one native conversation. A concurrent pool and
// persistent recovery are later stages; capacity never evicts tool waiters.
// Calls are serialized by the gateway's admission slot.
type Adapter struct {
	Config   config.Config
	Context  context.Context
	native   *native
	boundary *session.Candidate
	failure  *Error
	mu       sync.Mutex
}

func (a *Adapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.native != nil {
		a.native.close()
	}
}

func (a *Adapter) Run(ctx context.Context, request messages.Request, identity session.Identity, emit func(Event) error) (response messages.Response, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
	// A client may retry a failed stream as JSON with a smaller output budget.
	// Retain the first failure instead of masking it as a configuration mismatch.
	if a.failure != nil && a.boundary != nil && identity == a.boundary.Identity && jsonutil.Equal(request.Messages, a.boundary.Request.Messages) {
		err := validation("NATIVE_SESSION_LOST", "previous turn failed ("+a.failure.Code+"): "+a.failure.Message+"; native session ended; start a new conversation")
		err.Outcome = a.failure.Outcome
		return response, err
	}
	var candidates []session.Candidate
	if a.boundary != nil {
		candidates = append(candidates, *a.boundary)
	}
	match, err := session.Resolve(request, identity, candidates)
	if err != nil {
		var boundary *session.Error
		if errors.As(err, &boundary) {
			return response, validation(boundary.Code, boundary.Code+": request does not match a safe native session boundary")
		}
		return response, err
	}
	if match.Kind == session.Retry {
		err := validation("REQUEST_OUTCOME_UNKNOWN", "duplicate request will not repeat inference or redeliver tools")
		err.Outcome = "unknown"
		return response, err
	}
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
		if a.native != nil {
			return response, validation("SESSION_CAPACITY", "this stage retains one native conversation; a second session cannot evict it")
		}
		model, _ := a.Config.Model(request.Model)
		b, err := prepareBridge(request, model.WebSearch)
		if err != nil {
			return response, err
		}
		a.native, err = (Runner{Config: a.Config}).start(a.Context, request, b)
		if err != nil {
			return response, err
		}
		a.boundary = &session.Candidate{ID: "native", Identity: identity}
	} else if a.native.ctx.Err() != nil {
		a.native.close()
		return response, validation("NATIVE_SESSION_LOST", "native process expired or exited; HTTP history cannot reconstruct it")
	}
	// Check the result-only boundary before changing state or touching MCP.
	if len(a.native.expected) > 0 {
		if err := a.native.receiveResults(*user); err != nil {
			return response, err
		}
	}
	a.boundary.State, a.boundary.Request, a.boundary.Assistant = session.Active, request, messages.Message{}
	defer func() {
		if err != nil {
			a.native.close()
			var failure *Error
			if errors.As(err, &failure) {
				failure.Usage = a.native.currentUsage
				copy := *failure
				a.failure = &copy
			}
			// Keep the accepted boundary as a tombstone. Failure is not permission
			// to reconstruct history or repeat an unknown local operation.
			a.boundary.State = session.Idle
		}
	}()
	response, err = a.native.advance(ctx, request, *user, updates, emit)
	if err != nil {
		if ctx.Err() != nil {
			err = cancelled(ctx)
		}
		return response, err
	}
	content, err := json.Marshal(response.Content)
	if err != nil {
		return messages.Response{}, protocol()
	}
	a.boundary.Assistant = messages.Message{Role: "assistant", Content: content}
	a.boundary.State = session.Idle
	if *response.StopReason == messages.StopToolUse {
		a.boundary.State = session.WaitingTools
	}
	if *response.StopReason == messages.StopMaxTokens || *response.StopReason == messages.StopRefusal {
		a.native.close()
	}
	return response, nil
}

func validation(code, message string) *Error {
	err := Failure(code, "session", message, 409)
	err.Type, err.Outcome = "invalid_request_error", "not_started"
	return err
}
