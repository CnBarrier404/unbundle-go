package cli

import (
	"context"

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
	row := *entry.checkpoint
	if err := a.store.Verify(row); err != nil {
		return response, validation("NATIVE_CHECKPOINT_CHANGED", "background request source checkpoint is unavailable or changed")
	}
	// Background UI requests cannot execute caller or server tools.
	request.ToolChoice = &messages.ToolChoice{Type: "none"}
	b, err := prepareBridge(request, false)
	if err != nil {
		return response, err
	}
	logger := logging.From(ctx).With("session_id", entry.boundary.ID, "auxiliary_kind", kind)
	lifetime := logging.With(a.ctx, logger)
	n, err := (Runner{Config: a.Config}).start(lifetime, ctx, request, b, &row, true)
	if err != nil {
		return response, err
	}
	defer n.close()
	response, err = n.advance(ctx, request, match.Delta[0], nil, emit)
	if err != nil {
		logFailure(logger, "auxiliary_turn_failed", err)
		return response, err
	}
	logger.Info("auxiliary_turn_finished")
	return response, nil
}
