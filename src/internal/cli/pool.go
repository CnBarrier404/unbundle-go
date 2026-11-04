package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"unbundle-go/src/internal/config"
	"unbundle-go/src/internal/messages"
	"unbundle-go/src/internal/session"
)

// Adapter owns native conversations and their in-memory boundaries. The pool
// mutex is never held while starting, advancing, or closing a CLI process.
type Adapter struct {
	Config  config.Config
	Context context.Context

	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	entries   map[string]*conversation
	running   sync.WaitGroup
	closed    bool
	closeDone chan struct{}
}

type conversation struct {
	boundary session.Candidate
	native   *native
	failure  *Error
	busy     bool
	retiring bool
	lastUsed time.Time
}

// PoolStatus contains counts only, never prompts, tool arguments or identities.
type PoolStatus struct {
	Capacity       int `json:"capacity"`
	RequestLimit   int `json:"request_limit"`
	RecordLimit    int `json:"record_limit"`
	Records        int `json:"records"`
	Processes      int `json:"processes"`
	ActiveRequests int `json:"active_requests"`
	WaitingTools   int `json:"waiting_tools"`
	Idle           int `json:"idle"`
	Closed         int `json:"closed"`
	Closing        int `json:"closing"`
}

func (a *Adapter) initializeLocked() {
	if a.ctx == nil {
		parent := a.Context
		if parent == nil {
			parent = context.Background()
		}
		a.ctx, a.cancel = context.WithCancel(parent)
		a.entries = make(map[string]*conversation)
	}
}

// Reaping frees process capacity only after native cleanup has finished. The
// boundary remains, including tool IDs, so old history can never start a new CLI.
func (a *Adapter) reapLocked() {
	for _, entry := range a.entries {
		if entry.busy || entry.retiring || entry.native == nil {
			continue
		}
		select {
		case <-entry.native.cleaned:
			if entry.native.timer != nil {
				entry.native.timer.Stop()
			}
			entry.native = nil
			entry.boundary.State = session.Closed
		default:
		}
	}
}

func (a *Adapter) Status() PoolStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reapLocked()
	status := PoolStatus{Capacity: a.Config.Runtime.MaxSessions, RequestLimit: a.Config.Runtime.MaxActiveRequests, RecordLimit: a.Config.Runtime.MaxSessionRecords, Records: len(a.entries)}
	for _, entry := range a.entries {
		if entry.native != nil || entry.busy {
			status.Processes++ // includes startup reservations and closing processes
		}
		if entry.busy {
			status.ActiveRequests++
		} else if entry.retiring || entry.native != nil && entry.native.ctx.Err() != nil {
			status.Closing++
		} else {
			switch entry.boundary.State {
			case session.WaitingTools:
				status.WaitingTools++
			case session.Idle:
				status.Idle++
			case session.Closed:
				status.Closed++
			}
		}
	}
	return status
}

func (a *Adapter) acquire(ctx context.Context, request messages.Request, identity session.Identity) (*conversation, session.Match, session.State, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initializeLocked()
	if a.closed || a.ctx.Err() != nil {
		return nil, session.Match{}, "", capacity("GATEWAY_CLOSED", "gateway is shutting down")
	}
	if ctx.Err() != nil {
		return nil, session.Match{}, "", cancelled(ctx)
	}
	a.reapLocked()
	candidates := make([]session.Candidate, 0, len(a.entries))
	processes, active := 0, 0
	reclaiming := false
	var oldest *conversation
	for _, entry := range a.entries {
		candidates = append(candidates, entry.boundary)
		if entry.native != nil || entry.busy {
			processes++
		}
		if entry.busy {
			active++
		}
		if entry.retiring || !entry.busy && entry.native != nil && entry.native.ctx.Err() != nil {
			reclaiming = true
		}
		if !entry.busy && !entry.retiring && entry.native != nil && entry.boundary.State == session.Idle && (oldest == nil || entry.lastUsed.Before(oldest.lastUsed)) {
			oldest = entry
		}
	}
	match, err := session.Resolve(request, identity, candidates)
	if err != nil {
		var boundary *session.Error
		if errors.As(err, &boundary) {
			message := boundary.Code + ": request does not match a safe native session boundary"
			if boundary.Detail != "" {
				message += " (" + boundary.Detail + ")"
			}
			err = validation(boundary.Code, message)
		}
		return nil, match, "", err
	}
	entry := a.entries[match.CandidateID]
	if entry != nil && (entry.retiring || entry.boundary.State == session.Closed || entry.native.ctx.Err() != nil) {
		err := validation("NATIVE_SESSION_LOST", "native session ended; HTTP history cannot reconstruct it; start a new conversation")
		if entry.failure != nil {
			err.Message = "previous turn failed (" + entry.failure.Code + "): " + entry.failure.Message + "; start a new conversation"
			err.Outcome = entry.failure.Outcome
		}
		return nil, match, "", err
	}
	if match.Kind == session.Retry {
		err := validation("REQUEST_OUTCOME_UNKNOWN", "duplicate request will not repeat inference or redeliver tools")
		err.Outcome = "unknown"
		return nil, match, "", err
	}
	if active >= a.Config.Runtime.MaxActiveRequests {
		return nil, match, "", capacity("GATEWAY_BUSY", "inference request capacity is occupied")
	}
	if match.Kind == session.New {
		if len(a.entries) >= a.Config.Runtime.MaxSessionRecords {
			err := capacity("SESSION_RECORD_CAPACITY", "retained session boundary capacity is full; old boundaries cannot be discarded automatically")
			err.Retryable = false
			return nil, match, "", err
		}
		if processes >= a.Config.Runtime.MaxSessions {
			if oldest != nil && !reclaiming {
				a.retireLocked(oldest)
			}
			return nil, match, "", capacity("SESSION_CAPACITY", "native process capacity is occupied; any idle reclamation must finish before retrying")
		}
		id := rand.Text()
		entry = &conversation{boundary: session.Candidate{ID: id, Identity: identity, State: session.Idle, Request: request}, lastUsed: time.Now()}
		a.entries[id] = entry
	}
	previous := entry.boundary.State
	entry.busy, entry.boundary.State = true, session.Active
	a.running.Add(1)
	return entry, match, previous, nil
}

func (a *Adapter) retireLocked(entry *conversation) {
	entry.retiring = true
	entry.boundary.State = session.Closed
	n := entry.native
	a.running.Add(1)
	go func() {
		n.close()
		a.mu.Lock()
		entry.native, entry.retiring = nil, false
		a.mu.Unlock()
		a.running.Done()
	}()
}

func (a *Adapter) Close() {
	a.mu.Lock()
	if a.closed {
		done := a.closeDone
		a.mu.Unlock()
		<-done
		return
	}
	a.initializeLocked()
	a.closed = true
	a.closeDone = make(chan struct{})
	a.cancel()
	a.mu.Unlock()
	a.running.Wait()
	a.mu.Lock()
	var natives []*native
	for _, entry := range a.entries {
		if entry.native != nil {
			natives = append(natives, entry.native)
		}
	}
	a.mu.Unlock()
	// No new reservations are possible; stop idle processes in parallel too.
	var cleanup sync.WaitGroup
	for _, n := range natives {
		cleanup.Add(1)
		go func() { defer cleanup.Done(); n.close() }()
	}
	cleanup.Wait()
	a.mu.Lock()
	a.reapLocked()
	close(a.closeDone)
	a.mu.Unlock()
}

func capacity(code, message string) *Error {
	err := Failure(code, "session", message, 503)
	err.Type, err.Outcome, err.Retryable = "overloaded_error", "not_started", true
	return err
}
