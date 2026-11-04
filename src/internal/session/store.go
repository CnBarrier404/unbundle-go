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

// Checkpoint contains keyed boundary fingerprints, not conversation text.
// Ready is committed only after the official transcript reaches completion.
type Checkpoint struct {
	ID              string         `json:"id"`
	Identity        Identity       `json:"identity"`
	NativeID        string         `json:"native_id"`
	InitialMessages int            `json:"initial_messages"`
	Configuration   string         `json:"configuration"`
	History         []string       `json:"history"`
	Assistant       string         `json:"assistant,omitempty"`
	Ready           bool           `json:"ready"`
	Snapshot        NativeSnapshot `json:"snapshot"`
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

// Active must reach disk before any incremental input or real tool receipt is
// submitted. Replacing Ready atomically consumes the previous checkpoint.
func (s *Store) Active(id string, identity Identity, nativeID string, initialMessages int, request messages.Request, c config.Config) (Checkpoint, error) {
	row := Checkpoint{ID: id, Identity: identity, NativeID: nativeID, InitialMessages: initialMessages}
	var err error
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
	for i := 0; i < min(len(row.History), len(incoming.Messages)); i++ {
		value, err := s.digest(incoming.Messages[i])
		if err != nil || value != row.History[i] {
			return view
		}
	}
	view.Request = incoming
	if len(incoming.Messages) < len(row.History) {
		// A shortened old prefix is a retry/fork, never a fresh conversation.
		return view
	}
	view.Request.Messages = incoming.Messages[:len(row.History)]
	if row.Ready {
		view.State = Idle
	}
	signature, err := s.configuration(incoming, c)
	view.ConfigMismatch = err != nil || signature != row.Configuration
	if len(incoming.Messages) > len(row.History) {
		message := incoming.Messages[len(row.History)]
		value, err := s.digest(message)
		if err == nil && value == row.Assistant {
			view.Assistant = message
		}
	}
	return view
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
