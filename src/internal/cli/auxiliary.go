package cli

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"unbundle-go/src/internal/logging"
	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

// Background UI requests read a verified completion through the official CLI's
// fork operation. Neither their input nor their output advances the main turn.
func (a *Adapter) runAuxiliary(ctx context.Context, request messages.Request, identity session.Identity, kind string, emit func(Event) error) (response messages.Response, err error) {
	entry, match, previous, err := a.acquire(ctx, request, identity)
	if err != nil {
		return response, err
	}
	reserved := false
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if reserved {
			a.auxiliary--
		}
		entry.busy, entry.boundary.State = false, previous
		if match.Kind == session.New {
			delete(a.entries, entry.boundary.ID)
		}
		a.running.Done()
	}()
	if match.Kind != session.Continue || previous != session.Idle || entry.checkpoint == nil || !entry.checkpoint.Ready || entry.checkpoint.Interrupted || entry.checkpoint.PendingTools || len(match.Delta) != 1 {
		return response, validation("AUXILIARY_CONTEXT_REQUIRED", "background requests require one prompt after a verified native completion")
	}
	// acquire already reserved a process slot when the parent is on disk.
	if entry.native != nil {
		a.mu.Lock()
		processes := a.auxiliary
		for _, conversation := range a.entries {
			if conversation.native != nil || conversation.busy {
				processes++
			}
		}
		if processes < a.Config.Runtime.MaxSessions {
			a.auxiliary++
			reserved = true
		}
		a.mu.Unlock()
		if !reserved {
			return response, capacity("SESSION_CAPACITY", "native process capacity is occupied")
		}
	}
	row := *entry.checkpoint
	if err := a.store.Verify(row); err != nil {
		return response, validation("NATIVE_CHECKPOINT_CHANGED", "background request source checkpoint is unavailable or changed")
	}
	// The native side_question denies tool execution while keeping the same
	// tool schemas and system prefix as the parent for prompt cache reads.
	model, _ := a.Config.Model(request.Model)
	b, err := prepareBridge(request, model.WebSearch)
	if err != nil {
		return response, err
	}
	logger := logging.From(ctx).With("session_id", entry.boundary.ID, "auxiliary_kind", kind)
	ctx = logging.With(ctx, logger)
	lifetime := logging.With(a.ctx, logger)
	n, err := (Runner{Config: a.Config}).start(lifetime, ctx, request, b, &row, true)
	if err != nil {
		return response, err
	}
	defer n.close()
	stop := context.AfterFunc(ctx, n.cancel)
	defer stop()
	if err := n.updateThinking(ctx, request); err != nil {
		return response, err
	}
	question, _ := session.Text(request.Messages[len(request.Messages)-1].Content)
	response, err = n.sideQuestion(ctx, request.Model, question)
	if err != nil {
		logFailure(logger, "auxiliary_turn_failed", err)
		return response, err
	}
	logger.Info("auxiliary_turn_finished")
	if emit != nil {
		err = emitResponse(response, emit)
	}
	return response, err
}

func (n *native) sideQuestion(ctx context.Context, model, question string) (response messages.Response, err error) {
	before, err := n.modelUsage(ctx)
	if err != nil {
		return response, err
	}
	n.lastQuota = quotaInfo{}
	raw, err := n.requestControl(ctx, map[string]string{"subtype": "side_question", "question": question})
	if err != nil {
		if ctx.Err() != nil {
			return response, cancelled(ctx)
		}
		return response, err
	}
	// The side_question reply only contains text. Its tokens are included in
	// the native cost ledger, which can already contain the parent's usage.
	quota := n.lastQuota
	after, err := n.modelUsage(ctx)
	if err == nil {
		response.Usage, err = modelUsageDifference(before, after)
	}
	if err != nil {
		// The answer already exists. Leave unavailable counters unknown so a
		// failed accounting query cannot cause the client to repeat inference.
		logFailure(logging.From(ctx), "auxiliary_usage_unavailable", err)
	}
	var result struct {
		Text      *string `json:"response"`
		Synthetic *bool   `json:"synthetic"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Synthetic == nil {
		return response, protocol()
	}
	if *result.Synthetic || result.Text == nil {
		failure := auxiliaryFailure(result.Text, quota)
		failure.Usage = response.Usage
		return response, failure
	}
	text, err := json.Marshal(messages.TextBlock{Type: "text", Text: result.Text})
	if err != nil {
		return response, protocol()
	}
	reason := messages.StopEndTurn
	return messages.Response{ID: "msg_" + rand.Text(), Type: "message", Role: "assistant", Model: model, Content: []jsontext.Value{text}, StopReason: &reason, Usage: response.Usage}, nil
}
