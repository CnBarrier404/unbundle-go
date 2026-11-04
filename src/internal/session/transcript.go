package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"unbundle-go/src/internal/jsonutil"
	"unbundle-go/src/internal/messages"
)

type NativeSnapshot struct {
	Project string `json:"project"`
	Length  int64  `json:"length"`
	Digest  string `json:"digest"`
}

type transcriptRow struct {
	Type    string         `json:"type"`
	Subtype string         `json:"subtype"`
	Message jsontext.Value `json:"message"`
}

func nativeProjects() (string, error) {
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".claude")
	}
	return filepath.Join(root, "projects"), nil
}

// Keep the newline so byte offsets and prefix fingerprints remain exact on
// both platforms. A partially written row is not a committed native boundary.
func transcriptLines(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	scanner.Split(func(data []byte, eof bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i+1], nil
		}
		if eof && len(data) > 0 {
			return 0, nil, errors.New("incomplete native transcript row")
		}
		return 0, nil, nil
	})
	return scanner
}

func (s *Store) Snapshot(ctx context.Context, nativeID, assistantID string, content []jsontext.Value) (NativeSnapshot, error) {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		snapshot, err := s.snapshot(nativeID, assistantID, content)
		if err == nil {
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return NativeSnapshot{}, ctx.Err()
		case <-deadline.C:
			return NativeSnapshot{}, err
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *Store) snapshot(nativeID, assistantID string, content []jsontext.Value) (NativeSnapshot, error) {
	root, err := nativeProjects()
	if err != nil {
		return NativeSnapshot{}, err
	}
	projects, err := os.ReadDir(root)
	if err != nil {
		return NativeSnapshot{}, err
	}
	for _, project := range projects {
		if !project.IsDir() || project.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(root, project.Name(), nativeID+".jsonl")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return NativeSnapshot{}, err
		}
		snapshot, err := s.scanCompletion(file, assistantID, content)
		if err == nil {
			err = file.Sync()
		}
		_ = file.Close()
		if err != nil {
			return NativeSnapshot{}, err
		}
		snapshot.Project = project.Name()
		return snapshot, nil
	}
	return NativeSnapshot{}, errors.New("native transcript is not committed")
}

func (s *Store) scanCompletion(file io.Reader, assistantID string, content []jsontext.Value) (NativeSnapshot, error) {
	h := hmac.New(sha256.New, s.key)
	scanner := transcriptLines(file)
	var total int64
	var collected []jsontext.Value
	var snapshot NativeSnapshot
	lastAssistant := false
	for scanner.Scan() {
		line := scanner.Bytes()
		total += int64(len(line))
		_, _ = h.Write(line)
		var row transcriptRow
		if json.Unmarshal(line, &row) != nil {
			return snapshot, errors.New("invalid native transcript")
		}
		if row.Type == "user" || row.Type == "assistant" {
			lastAssistant = false
		}
		if row.Type != "assistant" {
			continue
		}
		var message struct {
			ID      string         `json:"id"`
			Content jsontext.Value `json:"content"`
		}
		if json.Unmarshal(row.Message, &message) != nil {
			return snapshot, errors.New("invalid native assistant record")
		}
		lastAssistant = message.ID == assistantID
		if lastAssistant {
			blocks, err := messages.Blocks(message.Content)
			if err != nil {
				return snapshot, err
			}
			collected = append(collected, blocks...)
			if jsonutil.Equal(collected, content) {
				snapshot.Length, snapshot.Digest = total, hex.EncodeToString(h.Sum(nil))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return snapshot, err
	}
	if snapshot.Length == 0 || !lastAssistant || !jsonutil.Equal(collected, content) {
		return snapshot, errors.New("native completion boundary not found")
	}
	return snapshot, nil
}

func (s *Store) Verify(row Checkpoint) error {
	root, err := nativeProjects()
	if err != nil {
		return err
	}
	snapshot := row.Snapshot
	if snapshot.Project == "" || snapshot.Project == "." || snapshot.Project == ".." || filepath.Base(snapshot.Project) != snapshot.Project || snapshot.Length < 1 {
		return errors.New("invalid native snapshot")
	}
	path := filepath.Join(root, snapshot.Project, row.NativeID+".jsonl")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("native transcript unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	h := hmac.New(sha256.New, s.key)
	if _, err := io.CopyN(h, file, snapshot.Length); err != nil || hex.EncodeToString(h.Sum(nil)) != snapshot.Digest {
		return errors.New("native transcript changed")
	}
	// CLI may append accounting metadata on exit. Conversation changes after
	// the completed prefix invalidate recovery; metadata alone does not.
	scanner := transcriptLines(file)
	for scanner.Scan() {
		var tail transcriptRow
		if json.Unmarshal(scanner.Bytes(), &tail) != nil || tail.Type == "user" || tail.Type == "assistant" || tail.Subtype == "compact_boundary" {
			return errors.New("native transcript advanced beyond checkpoint")
		}
	}
	return scanner.Err()
}
