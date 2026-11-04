package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

func compactResponse(model, id, body string, usage messages.Usage) messages.Response {
	text, _ := json.Marshal(map[string]string{"type": "text", "text": "<analysis></analysis><summary>\n" + body + "\n</summary>"})
	reason := messages.StopEndTurn
	return messages.Response{ID: id, Type: "message", Role: "assistant", Model: model, Content: []jsontext.Value{text}, StopReason: &reason, Usage: usage, Recovery: &messages.Recovery{State: "ready"}}
}

func (a *Adapter) runCompact(ctx context.Context, entry *conversation, match session.Match, previous session.State, request messages.Request, emit func(Event) error) (response messages.Response, err error) {
	n := entry.native
	accepted := false
	keep := true
	arm := false
	defer func() {
		if n != nil {
			if !keep || accepted && err != nil || a.ctx.Err() != nil {
				n.close()
			} else if arm {
				reason := messages.StopEndTurn
				if previous == session.WaitingTools && !accepted {
					reason = messages.StopToolUse
				}
				n.armIdle(reason)
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		defer a.running.Done()
		entry.busy = false
		entry.boundary.State = previous
		if accepted {
			entry.boundary.State = session.Idle
		}
		if accepted && err != nil {
			entry.boundary.State = session.Closed
			var failure *Error
			if errors.As(err, &failure) {
				failure.Usage = n.currentUsage
				copy := *failure
				copy.Usage = n.currentUsage
				entry.failure = &copy
			}
		}
		entry.lastUsed = time.Now()
		a.reapLocked()
	}()
	row := *entry.checkpoint
	if match.Kind == session.CompactRetry {
		if err := a.store.Verify(row); err != nil {
			return response, validation("NATIVE_CHECKPOINT_CHANGED", "committed compaction transcript changed; the summary will not be regenerated")
		}
		body, _, err := a.store.CompactSnapshot(ctx, row.NativeID, row.Compaction.Boundary, a.Config.Runtime.MaxOutputBytes)
		if err != nil {
			return response, validation("COMPACTION_SUMMARY_UNAVAILABLE", "committed native summary is unavailable; the summary will not be regenerated")
		}
		response = compactResponse(request.Model, row.Compaction.ResponseID, body, row.Compaction.Usage)
		response.Reused = true
		if emit != nil {
			err = emitResponse(response, emit)
		}
		return response, err
	}
	if row.Compaction != nil {
		return response, validation("COMPACTION_ALREADY_COMMITTED", "consume the committed summary before requesting another compaction")
	}
	if n == nil {
		if err := a.store.Verify(row); err != nil {
			return response, validation("NATIVE_CHECKPOINT_CHANGED", "native completion checkpoint is unavailable or changed")
		}
		base := entry.boundary.Request
		model, _ := a.Config.Model(base.Model)
		b, err := prepareBridge(base, model.WebSearch)
		if err != nil {
			return response, err
		}
		n, err = (Runner{Config: a.Config}).start(a.ctx, ctx, base, b, &row)
		if err != nil {
			return response, err
		}
		a.mu.Lock()
		entry.native = n
		a.mu.Unlock()
		arm = true
	}
	stop := context.AfterFunc(ctx, n.cancel)
	defer stop()
	if previous == session.WaitingTools {
		if len(match.Delta) != 1 {
			return response, validation("COMPACTION_RESULTS_REQUIRED", "compaction requires exactly the real pending tool results")
		}
		if err := n.receiveResults(match.Delta[0]); err != nil {
			return response, err
		}
	}
	arm = true
	before, err := n.prepareCompact(ctx, previous == session.WaitingTools)
	if err != nil {
		var failure *Error
		keep = errors.As(err, &failure) && failure.Outcome == "not_started"
		return response, err
	}
	if ctx.Err() != nil {
		return response, cancelled(ctx)
	}
	// Revoke recovery before either interrupting a tool wait, buffering real
	// context, or committing /compact. A crash can never reuse the old boundary.
	row.Ready = false
	if err := a.store.Put(row); err != nil {
		return response, validation("CHECKPOINT_CONSUME_FAILED", "could not revoke the previous checkpoint; compaction was not submitted")
	}
	a.mu.Lock()
	entry.checkpoint = &row
	a.mu.Unlock()
	accepted = true
	n.currentUsage = messages.Usage{}
	boundary, usage, err := n.compact(ctx, request, match.Delta, before)
	if err != nil {
		return response, err
	}
	body, snapshot, err := a.store.CompactSnapshot(ctx, n.id, boundary, a.Config.Runtime.MaxOutputBytes)
	if err != nil {
		return response, Failure("NATIVE_COMPACT_NOT_PERSISTED", "compaction", "native compaction did not produce a verifiable persisted summary; do not repeat the operation", 502)
	}
	id := "msg_" + uuid.NewString()
	payload := "Summary:\n" + body
	for strings.Contains(payload, "\n\n\n") {
		payload = strings.ReplaceAll(payload, "\n\n\n", "\n\n")
	}
	source := append(slices.Clone(entry.boundary.Request.Messages), entry.boundary.Assistant)
	source = append(source, match.Delta...)
	row.SystemPrompt, row.Instructions = n.prompt, slices.Clone(n.instructions)
	completed, err := a.store.RecordCompaction(row, request, boundary, payload, id, usage, snapshot, source)
	if err != nil {
		return response, Failure("COMPACTION_CHECKPOINT_SAVE_FAILED", "compaction", "native compaction committed but its recovery receipt could not be saved; do not repeat the operation", 503)
	}
	a.mu.Lock()
	entry.checkpoint = &completed
	a.mu.Unlock()
	response = compactResponse(request.Model, id, body, usage)
	// Commit precedes response delivery. A disconnect reuses this exact receipt;
	// it must never consume another model request or replay a tool operation.
	accepted = false
	previous = session.Idle
	if emit != nil {
		err = emitResponse(response, emit)
	}
	return response, err
}
