// Package session defines identity and history boundaries, not a process pool.
// A successful match authorizes neither CLI startup nor replay of tool calls.
package session

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

type Identity struct {
	SessionID     string
	AgentID       string
	ParentAgentID string
	RequestClass  string
}

// IdentityFromHeaders reads documented CLI hints. They are not authentication,
// may be absent, and never substitute for verifying the message history.
// Custom proxy headers and metadata.user_id are intentionally not used.
func IdentityFromHeaders(h http.Header) (Identity, error) {
	read := func(name string) (string, error) {
		var values []string
		for key, items := range h {
			if strings.EqualFold(key, name) {
				values = append(values, items...)
			}
		}
		if len(values) == 0 {
			return "", nil
		}
		if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 128 {
			return "", errors.New("invalid session identity header")
		}
		for _, c := range values[0] {
			if c < 0x21 || c > 0x7e || c == ',' {
				return "", errors.New("invalid session identity header")
			}
		}
		return values[0], nil
	}
	var id Identity
	fields := []struct {
		name string
		out  *string
	}{
		{"x-claude-code-session-id", &id.SessionID},
		{"x-claude-code-agent-id", &id.AgentID},
		{"x-claude-code-parent-agent-id", &id.ParentAgentID},
		{"x-claude-code-request-class", &id.RequestClass},
	}
	for _, field := range fields {
		v, err := read(field.name)
		if err != nil {
			return Identity{}, err
		}
		*field.out = v
	}
	if (id.AgentID != "" || id.ParentAgentID != "") && id.SessionID == "" {
		return Identity{}, errors.New("agent identity requires a session identity")
	}
	if id.ParentAgentID != "" && id.AgentID == "" {
		return Identity{}, errors.New("parent identity requires an agent identity")
	}
	if id.RequestClass != "" && !slices.Contains([]string{"main", "subagent", "workflow", "auxiliary", "compaction"}, id.RequestClass) {
		return Identity{}, errors.New("unsupported request class")
	}
	return id, nil
}

type State string

const (
	Idle         State = "idle"
	WaitingTools State = "waiting_tools"
	Active       State = "active"
	Closed       State = "closed"
)

// Candidate is an in-memory boundary supplied by the session manager.
// Request is the previous accepted request; Assistant is its completed output.
// It is not an import format, a checkpoint or proof of HTTP tool delivery.
type Candidate struct {
	ID             string
	Identity       Identity
	State          State
	Request        messages.Request
	Assistant      messages.Message
	ConfigMismatch bool
	Compaction     *Compaction
	CompactRetry   bool
	RebasedDelta   []messages.Message
}

type Kind string

const (
	New          Kind = "new"
	Continue     Kind = "continue"
	Retry        Kind = "retry"
	Compact      Kind = "compact"
	CompactRetry Kind = "compact_retry"
)

type Match struct {
	Kind        Kind
	CandidateID string
	Delta       []messages.Message
}
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string { return e.Code }
func fail(code string) error   { return &Error{Code: code} }

// Resolve requires a unique verifiable match. A retry is only classified; it
// must not advance a CLI or redeliver tools without later delivery accounting.
// Forks and modified history never trigger automatic historical reconstruction.
func Resolve(incoming messages.Request, identity Identity, candidates []Candidate) (Match, error) {
	if err := incoming.Validate(); err != nil {
		return Match{}, err
	}
	source, compact := CompactionIntent(incoming, identity)
	if identity.RequestClass == "compaction" && !compact {
		return Match{}, fail("COMPACTION_REQUEST_INVALID")
	}
	var matches []Match
	identified := false
	var rejection string
	var detail string
	for _, candidate := range candidates {
		if identity.SessionID != "" && (candidate.Identity.SessionID != identity.SessionID || candidate.Identity.AgentID != identity.AgentID) {
			continue
		}
		candidateIdentified := identity.SessionID != ""
		if candidateIdentified {
			identified = true
		}
		if identity.ParentAgentID != "" && candidate.Identity.ParentAgentID != "" && identity.ParentAgentID != candidate.Identity.ParentAgentID {
			rejection = "SESSION_IDENTITY_MISMATCH"
			continue
		}
		if compact {
			if candidateIdentified && candidate.State == Active {
				return Match{}, fail("SESSION_BUSY")
			}
			if candidate.CompactRetry {
				if candidate.State == Active {
					return Match{}, fail("SESSION_BUSY")
				}
				matches = append(matches, Match{Kind: CompactRetry, CandidateID: candidate.ID})
				continue
			}
			delta, ok := compactSource(candidate, source)
			if ok {
				if candidate.ConfigMismatch {
					rejection = "SESSION_DEPLOYMENT_CHANGED"
					continue
				}
				if candidate.State == Active {
					return Match{}, fail("SESSION_BUSY")
				}
				matches = append(matches, Match{Kind: Compact, CandidateID: candidate.ID, Delta: delta})
			} else if candidateIdentified {
				rejection = "COMPACTION_HISTORY_MISMATCH"
			}
			continue
		}
		if candidate.RebasedDelta != nil {
			if identity.RequestClass != "" && candidate.Identity.RequestClass != "" && identity.RequestClass != candidate.Identity.RequestClass {
				rejection = "SESSION_IDENTITY_MISMATCH"
				continue
			}
			if candidate.State == Active {
				return Match{}, fail("SESSION_BUSY")
			}
			if candidate.ConfigMismatch || !compatible(candidate.Request, incoming) {
				rejection = ConfigurationChange(candidate.Request, incoming)
				if rejection == "" {
					rejection = "SESSION_CONFIG_CHANGED"
				}
				continue
			}
			matches = append(matches, Match{Kind: Continue, CandidateID: candidate.ID, Delta: candidate.RebasedDelta})
			continue
		}
		if candidate.Compaction != nil {
			if candidateIdentified || len(candidate.Request.Messages) > 0 && prefix(candidate.Request.Messages, incoming.Messages) {
				rejection = "COMPACTION_RECEIPT_REQUIRED"
			}
			continue
		}
		sameRequest := jsonutil.Equal(incoming.Messages, candidate.Request.Messages)
		expected := append(slices.Clone(candidate.Request.Messages), candidate.Assistant)
		continues := candidate.Assistant.Role == "assistant" && len(incoming.Messages) > len(expected) &&
			prefix(candidate.Request.Messages, incoming.Messages) &&
			assistantReceiptEqual(candidate.Assistant, incoming.Messages[len(candidate.Request.Messages)], candidate.Request.Messages)
		// Session and agent identify the conversation. Optional hints may be
		// omitted, but conflicting supplied hints must not create a new one.
		if identity.RequestClass != "" && candidate.Identity.RequestClass != "" && candidate.Identity.RequestClass != identity.RequestClass ||
			identity.ParentAgentID != "" && candidate.Identity.ParentAgentID != "" && candidate.Identity.ParentAgentID != identity.ParentAgentID {
			if candidateIdentified || len(candidate.Request.Messages) > 0 && (sameRequest || continues || prefix(incoming.Messages, expected) || prefix(candidate.Request.Messages, incoming.Messages)) {
				rejection = "SESSION_IDENTITY_MISMATCH"
			}
			continue
		}
		if candidate.State == Active && (candidateIdentified || sameRequest || continues || prefix(candidate.Request.Messages, incoming.Messages)) {
			return Match{}, fail("SESSION_BUSY")
		}
		if !sameRequest && !continues && len(incoming.Messages) < len(expected) && prefix(incoming.Messages, expected) {
			rejection = "HISTORY_FORK_UNSUPPORTED"
			continue
		}
		if candidate.State != Idle && candidate.State != WaitingTools && candidate.State != Closed {
			continue
		}
		if !sameRequest && !continues {
			if candidateIdentified {
				rejection = "SESSION_HISTORY_MISMATCH"
				if len(candidate.Request.Messages) > 0 {
					detail = historyDifference(expected, incoming.Messages)
				} else {
					detail = "stored history fingerprint mismatch"
				}
			}
			continue
		}
		// A closed boundary still identifies retries and continuations, even if
		// a retry changes its budget. It never authorizes another CLI startup.
		if candidate.State != Closed && (candidate.ConfigMismatch || !compatible(candidate.Request, incoming)) {
			rejection = ConfigurationChange(candidate.Request, incoming)
			if rejection == "" {
				rejection = "SESSION_CONFIG_CHANGED"
			}
			continue
		}
		if sameRequest {
			matches = append(matches, Match{Kind: Retry, CandidateID: candidate.ID})
			continue
		}
		delta := incoming.Messages[len(expected):]
		if !newInput(delta) {
			rejection = "HISTORY_IMPORT_FORBIDDEN"
			continue
		}
		matches = append(matches, Match{Kind: Continue, CandidateID: candidate.ID, Delta: delta})
	}
	if len(matches) > 1 {
		return Match{}, fail("SESSION_AMBIGUOUS")
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if rejection != "" {
		return Match{}, &Error{Code: rejection, Detail: detail}
	}
	if identified {
		return Match{}, fail("SESSION_HISTORY_MISMATCH")
	}
	if compact {
		return Match{}, fail("COMPACTION_SESSION_REQUIRED")
	}
	for _, message := range incoming.Messages {
		text, _ := Text(message.Content)
		if message.Role == "user" && strings.HasPrefix(text, SummaryHeader+"\n\n") {
			return Match{}, fail("COMPACTION_RECEIPT_MISMATCH")
		}
	}
	if !newInput(incoming.Messages) {
		return Match{}, fail("NATIVE_SESSION_REQUIRED")
	}
	return Match{Kind: New, Delta: incoming.Messages}, nil
}

func prefix(a, b []messages.Message) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if !jsonutil.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Only one new user turn plus explicit system updates can be submitted. An
// assistant role would import history instead of supplying new native input.
func newInput(delta []messages.Message) bool {
	users := 0
	for _, m := range delta {
		switch m.Role {
		case "user":
			users++
		case "system":
		default:
			return false
		}
	}
	return users == 1
}

func compatible(a, b messages.Request) bool {
	return ConfigurationChange(a, b) == ""
}
