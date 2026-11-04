package session

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CompactSnapshot requires the exact emitted commit UUID and its following
// official summary. It never treats an ordinary assistant answer as a summary.
func (s *Store) CompactSnapshot(ctx context.Context, nativeID, boundary string, limit int64) (string, NativeSnapshot, error) {
	if uuid.Validate(nativeID) != nil || uuid.Validate(boundary) != nil {
		return "", NativeSnapshot{}, errors.New("invalid compaction identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		body, snapshot, err := s.compactSnapshot(nativeID, boundary, limit)
		if err == nil {
			return body, snapshot, nil
		}
		select {
		case <-ctx.Done():
			return "", NativeSnapshot{}, err
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *Store) compactSnapshot(nativeID, boundary string, limit int64) (string, NativeSnapshot, error) {
	root, err := nativeProjects()
	if err != nil {
		return "", NativeSnapshot{}, err
	}
	projects, err := os.ReadDir(root)
	if err != nil {
		return "", NativeSnapshot{}, err
	}
	var path, project string
	for _, dir := range projects {
		if !dir.IsDir() || dir.Type()&os.ModeSymlink != 0 {
			continue
		}
		candidate := filepath.Join(root, dir.Name(), nativeID+".jsonl")
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return "", NativeSnapshot{}, errors.New("invalid native transcript")
		}
		if path != "" {
			return "", NativeSnapshot{}, errors.New("ambiguous native transcript")
		}
		path, project = candidate, dir.Name()
	}
	if path == "" {
		return "", NativeSnapshot{}, errors.New("native transcript missing")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", NativeSnapshot{}, err
	}
	defer file.Close()
	h := hmac.New(sha256.New, s.key)
	scanner := transcriptLines(file)
	var total int64
	var body string
	found := false
	for scanner.Scan() {
		line := scanner.Bytes()
		total += int64(len(line))
		_, _ = h.Write(line)
		var row struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			UUID    string `json:"uuid"`
			Summary bool   `json:"isCompactSummary"`
			Message struct {
				Content jsontext.Value `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &row) != nil {
			return "", NativeSnapshot{}, errors.New("invalid native transcript")
		}
		if row.Type == "system" && row.Subtype == "compact_boundary" {
			found, body = row.UUID == boundary, ""
		}
		if found && row.Type == "user" && row.Summary {
			text, ok := Text(row.Message.Content)
			if !ok || int64(len(text)) > limit {
				return "", NativeSnapshot{}, errors.New("invalid native summary")
			}
			body, err = NativeSummaryBody(text, path)
			if err != nil {
				return "", NativeSnapshot{}, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", NativeSnapshot{}, err
	}
	if body == "" {
		return "", NativeSnapshot{}, errors.New("native summary not committed")
	}
	if err := file.Sync(); err != nil {
		return "", NativeSnapshot{}, err
	}
	return body, NativeSnapshot{Project: project, Length: total, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}

func NativeSummaryBody(text, file string) (string, error) {
	marker := SummaryHeader + "\n\n"
	at := strings.Index(text, marker)
	if at < 0 || at > 2048 {
		return "", errors.New("invalid native summary wrapper")
	}
	body := StripSummaryTools(text[at+len(marker):])
	tail := "\n\nIf you need specific details from before compaction (like exact code snippets, error messages, or content you generated), read the full transcript at: "
	if end := strings.LastIndex(body, tail); end >= 0 {
		reference := body[end+len(tail):]
		if reference != file && !strings.HasPrefix(reference, file+"\n") {
			return "", errors.New("native summary transcript reference differs")
		}
		body = body[:end]
	} else {
		const resume = "\nContinue the conversation from where it left off without asking the user any further questions."
		if end := strings.LastIndex(body, resume); end >= 0 && strings.HasSuffix(body, "Pick up the last task as if the break never happened.") {
			body = body[:end]
		}
		const kept = "\n\nThe messages after this summary are the most recent messages from before compaction, kept verbatim. The summary was written without seeing them, so something it says has not happened yet may already have happened in them."
		body = strings.TrimSuffix(body, kept)
	}
	if !strings.HasPrefix(body, "Summary:\n") || strings.TrimSpace(strings.TrimPrefix(body, "Summary:\n")) == "" {
		return "", errors.New("native summary body missing")
	}
	return strings.TrimSpace(strings.TrimPrefix(body, "Summary:\n")), nil
}
