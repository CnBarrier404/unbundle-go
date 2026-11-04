package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	"unbundle-go/src/internal/config"
	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

// Checkpoint contains keyed history fingerprints and private caller instructions.
// Ready requires a verified native boundary after process exit. PendingTools
// accepts only real caller receipts for the saved, uncompleted tool batch.
type Checkpoint struct {
	ID              string              `json:"id"`
	Identity        Identity            `json:"identity"`
	NativeID        string              `json:"native_id"`
	InitialMessages int                 `json:"initial_messages"`
	Configuration   string              `json:"configuration"`
	Deployment      string              `json:"deployment,omitempty"`
	History         []string            `json:"history"`
	Assistant       string              `json:"assistant,omitempty"`
	Ready           bool                `json:"ready"`
	Interrupted     bool                `json:"interrupted,omitempty"`
	PendingTools    bool                `json:"pending_tools,omitempty"`
	Snapshot        NativeSnapshot      `json:"snapshot"`
	SystemPrompt    string              `json:"system_prompt,omitempty"`
	Instructions    []InstructionUpdate `json:"instructions,omitempty"`
	Compaction      *Compaction         `json:"compaction,omitempty"`
	Settings        *messages.Request   `json:"settings,omitempty"`
	CacheTTL        string              `json:"cache_ttl,omitempty"`
}

type Store struct {
	db  *bolt.DB
	key []byte
}

var recordsBucket = []byte("checkpoints")

func OpenStore(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(directory, "checkpoints.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists([]byte("metadata"))
		if err != nil {
			return err
		}
		if version := meta.Get([]byte("version")); version != nil && string(version) != "1" {
			return errors.New("unsupported checkpoint version")
		}
		if err := meta.Put([]byte("version"), []byte("1")); err != nil {
			return err
		}
		if meta.Get([]byte("key")) == nil {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return err
			}
			if err := meta.Put([]byte("key"), key); err != nil {
				return err
			}
		}
		s.key = append([]byte(nil), meta.Get([]byte("key"))...)
		if len(s.key) != 32 {
			return errors.New("invalid checkpoint key")
		}
		_, err = tx.CreateBucketIfNotExists(recordsBucket)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Load() ([]Checkpoint, error) {
	var records []Checkpoint
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucket).ForEach(func(id, raw []byte) error {
			var row Checkpoint
			if json.Unmarshal(raw, &row) != nil || row.ID != string(id) || uuid.Validate(row.NativeID) != nil || len(row.History) == 0 || row.InitialMessages < 1 || row.InitialMessages > len(row.History) {
				return errors.New("invalid checkpoint record")
			}
			if row.PendingTools && (!row.Ready || row.Interrupted || row.Compaction != nil) {
				return errors.New("invalid pending tool checkpoint")
			}
			if record := row.Compaction; record != nil {
				if !row.Ready || uuid.Validate(record.Boundary) != nil || record.SummaryLength < 1 || record.SummaryLength > 256<<20 || record.Leading < 0 || record.Leading > len(record.Source) || len(record.Summary) != 64 || len(record.Request) != 64 || record.ResponseID == "" {
					return errors.New("invalid compaction receipt")
				}
			}
			records = append(records, row)
			return nil
		})
	})
	return records, err
}

func (s *Store) Put(record Checkpoint) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(recordsBucket).Put([]byte(record.ID), data)
	})
}

func (s *Store) digest(value any) (string, error) {
	data, err := jsonutil.Canonical(value)
	if err != nil {
		return "", err
	}
	h := hmac.New(sha256.New, s.key)
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Store) configuration(request messages.Request, c config.Config) (string, error) {
	request.Messages, request.Metadata, request.Stream = nil, nil, false
	model, _ := c.Model(request.Model)
	return s.digest(struct {
		Request messages.Request
		TTL     string
		Model   config.Model
		CLI     string
	}{request, request.CacheTTL, model, c.CLI.Version})
}

func (s *Store) deployment(modelID string, c config.Config) (string, error) {
	model, _ := c.Model(modelID)
	return s.digest(struct {
		Model config.Model
		CLI   config.CLI
	}{model, *c.CLI})
}

func (s *Store) configurationMismatch(row Checkpoint, incoming messages.Request, c config.Config) bool {
	if row.Settings != nil && row.Deployment != "" {
		deployment, err := s.deployment(row.Settings.Model, c)
		return err != nil || deployment != row.Deployment
	}
	signature, err := s.configuration(incoming, c)
	return err != nil || signature != row.Configuration
}

// Active must reach disk before any incremental input or real tool receipt is
// submitted. Replacing Ready atomically consumes the previous checkpoint.
func (s *Store) Active(id string, identity Identity, nativeID string, initialMessages int, request messages.Request, c config.Config) (Checkpoint, error) {
	row := Checkpoint{ID: id, Identity: identity, NativeID: nativeID, InitialMessages: min(initialMessages, len(request.Messages))}
	settings := request
	settings.Messages, settings.Metadata, settings.Stream = nil, nil, false
	row.Settings, row.CacheTTL = &settings, request.CacheTTL
	var err error
	row.Deployment, err = s.deployment(request.Model, c)
	if err != nil {
		return row, err
	}
	row.Configuration, err = s.configuration(request, c)
	if err != nil {
		return row, err
	}
	for _, message := range request.Messages {
		value, err := s.digest(message)
		if err != nil {
			return row, err
		}
		row.History = append(row.History, value)
	}
	return row, s.Put(row)
}

// Materialize uses only a verified incoming prefix to recover the in-memory
// matching view. The returned messages are never replayed into the native CLI.
func (s *Store) Materialize(row Checkpoint, incoming messages.Request, c config.Config) Candidate {
	view := Candidate{ID: row.ID, Identity: row.Identity, State: Closed}
	if row.Compaction != nil {
		view.Compaction = row.Compaction
		digest, err := s.requestDigest(incoming)
		view.CompactRetry = err == nil && digest == row.Compaction.Request
		view.RebasedDelta, _ = s.CompactedDelta(row.Compaction, incoming)
		if view.CompactRetry || view.RebasedDelta != nil {
			view.Request = incoming
			if row.Settings != nil {
				view.Request = *row.Settings
				view.Request.CacheTTL = row.CacheTTL
			}
			if row.Ready {
				view.State = Idle
			}
			view.ConfigMismatch = s.configurationMismatch(row, incoming, c)
			return view
		}
	}
	for i := 0; i < min(len(row.History), len(incoming.Messages)); i++ {
		if !s.messageMatches(row.History[i], incoming.Messages[i]) {
			return view
		}
	}
	view.Request = incoming
	if row.Settings != nil {
		view.Request = *row.Settings
		view.Request.CacheTTL = row.CacheTTL
		view.Request.Messages = incoming.Messages
	}
	if len(incoming.Messages) < len(row.History) {
		// A shortened old prefix is a retry/fork, never a fresh conversation.
		return view
	}
	view.Request.Messages = incoming.Messages[:len(row.History)]
	if row.Ready {
		view.State = Idle
	}
	view.ConfigMismatch = s.configurationMismatch(row, incoming, c)
	if len(incoming.Messages) > len(row.History) {
		message := incoming.Messages[len(row.History)]
		if s.messageMatches(row.Assistant, message) {
			view.Assistant = message
		}
	}
	return view
}

func (s *Store) RecordCompaction(row Checkpoint, request messages.Request, boundary, summary, responseID string, usage messages.Usage, snapshot NativeSnapshot, source []messages.Message) (Checkpoint, error) {
	record := &Compaction{Boundary: boundary, ResponseID: responseID, Usage: usage, SummaryLength: len(summary)}
	var err error
	record.Request, err = s.requestDigest(request)
	if err != nil {
		return row, err
	}
	record.Summary, err = s.digest(summary)
	if err != nil {
		return row, err
	}
	for _, message := range source {
		digest, err := s.digest(message)
		if err != nil {
			return row, err
		}
		record.Source = append(record.Source, digest)
		if len(record.Source) == record.Leading+1 && message.Role == "system" {
			record.Leading++
		}
	}
	row.Ready, row.Snapshot, row.Compaction = true, snapshot, record
	return row, s.Put(row)
}

func (s *Store) Complete(row Checkpoint, assistant messages.Message, snapshot NativeSnapshot) (Checkpoint, error) {
	var err error
	row.Assistant, err = s.digest(assistant)
	if err != nil {
		return row, err
	}
	row.Ready, row.Snapshot = true, snapshot
	return row, s.Put(row)
}
