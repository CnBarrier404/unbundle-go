package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"time"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/logging"
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
	logger := logging.From(ctx).With("session_id", entry.boundary.ID)
	ctx = logging.With(ctx, logger)
	logger.Debug("session_acquired", "match", string(match.Kind), "previous_state", string(previous))
	start := time.Now()
	defer func() {
		if err != nil {
			logFailure(logger, "session_turn_failed", err)
			return
		}
		logger.Info("session_turn_finished", "duration_ms", time.Since(start).Milliseconds(), "stop_reason", response.StopReason, "response_reused", response.Reused)
	}()
	if match.Kind == session.Compact || match.Kind == session.CompactRetry {
		return a.runCompact(ctx, entry, match, previous, request, emit)
	}
	// The reservation gives this request exclusive ownership of native protocol
	// state. Pool locks protect only boundaries and resource accounting.
	n := entry.native
	started := n == nil
	accepted := false
	var content jsontext.Value
	defer func() {
		if n != nil && (accepted && err != nil || !accepted && started || a.ctx.Err() != nil) {
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
	if match.Kind == session.Continue && !jsonutil.Equal(entry.boundary.Request.System, request.System) {
		old, _ := messages.Blocks(entry.boundary.Request.System)
		next, _ := messages.Blocks(request.System)
		content, _ := json.Marshal(next[len(old):])
		updates = append(updates, instructionUpdate{Index: len(request.Messages) - len(match.Delta), Role: "system", Content: content})
	}
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
	if started {
		var resume *session.Checkpoint
		if match.Kind != session.New {
			resume = entry.checkpoint
			if err := a.store.Verify(*resume); err != nil {
				logger.Warn("checkpoint_verify_failed", logging.Cause(err))
				return response, validation("NATIVE_CHECKPOINT_CHANGED", "native completion checkpoint is unavailable or changed")
			}
		}
		model, _ := a.Config.Model(request.Model)
		b, err := prepareBridge(request, model.WebSearch)
		if err != nil {
			return response, err
		}
		lifetime := logging.With(a.ctx, logging.From(a.ctx).With("session_id", entry.boundary.ID))
		n, err = (Runner{Config: a.Config}).start(lifetime, ctx, request, b, resume)
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
	binding := entry.boundary.Identity
	if binding.RequestClass == "" {
		binding.RequestClass = identity.RequestClass
	}
	if binding.ParentAgentID == "" {
		binding.ParentAgentID = identity.ParentAgentID
	}
	activeRecord, err := a.store.Active(entry.boundary.ID, binding, n.id, n.initialMessages, request, a.Config)
	if err != nil {
		logger.Error("checkpoint_consume_failed", logging.Cause(err))
		failure := Failure("CHECKPOINT_CONSUME_FAILED", "checkpoint", "could not commit the new boundary; no input or tool result was submitted", 503)
		failure.Outcome = "not_started"
		return response, failure
	}
	a.mu.Lock()
	entry.boundary.Identity, entry.checkpoint = binding, &activeRecord
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
	activeRecord.SystemPrompt, activeRecord.Instructions = n.prompt, n.instructions
	content, err = json.Marshal(response.Content)
	if err != nil {
		return messages.Response{}, protocol()
	}
	response.Recovery = &messages.Recovery{State: "unavailable"}
	if *response.StopReason == messages.StopToolUse {
		response.Recovery.State = "waiting_tools"
	} else if *response.StopReason == messages.StopEndTurn && n.p.result {
		snapshot, saveErr := a.store.Snapshot(ctx, n.id, n.p.response.ID, n.p.response.Content)
		if saveErr == nil {
			var completed session.Checkpoint
			completed, saveErr = a.store.Complete(activeRecord, messages.Message{Role: "assistant", Content: content}, snapshot)
			if saveErr == nil {
				a.mu.Lock()
				entry.checkpoint = &completed
				a.mu.Unlock()
				response.Recovery.State = "ready"
			}
		}
		if saveErr != nil {
			logger.Error("checkpoint_save_failed", logging.Cause(saveErr))
			response.Recovery.Code = "CHECKPOINT_SAVE_FAILED"
		}
	}
	logger.Debug("checkpoint_state", "recovery", response.Recovery.State)
	if *response.StopReason == messages.StopMaxTokens || *response.StopReason == messages.StopRefusal {
		n.close()
	} else {
		n.armIdle(*response.StopReason)
	}
	return response, nil
}

func validation(code, message string) *Error {
	err := Failure(code, "session", message, 409)
	err.Type, err.Outcome = "invalid_request_error", "not_started"
	return err
}
