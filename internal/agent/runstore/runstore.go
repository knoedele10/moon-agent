// Package runstore persists a small append-only journal for long-running
// agent runs. It deliberately uses JSONL files instead of the application DB:
// a crashed process can still be inspected before the database is available.
package runstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusRunning Status = "running"
	StatusWaiting Status = "waiting"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusAborted Status = "aborted"
)

// Event is one durable journal record. Payload is intentionally opaque to the
// store so callers can evolve their trace schema without a migration.
type Event struct {
	At      time.Time       `json:"at"`
	RunID   string          `json:"run_id"`
	Session string          `json:"session_id,omitempty"`
	Type    string          `json:"type"`
	Status  Status          `json:"status,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Summary struct {
	RunID     string    `json:"run_id"`
	SessionID string    `json:"session_id,omitempty"`
	Status    Status    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Path      string    `json:"path"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func New(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("runstore directory cannot be empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve runstore directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create runstore directory: %w", err)
	}
	return &Store{dir: abs}, nil
}

func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *Store) Start(runID, sessionID string, payload any) error {
	return s.append(Event{RunID: runID, Session: sessionID, Type: "run_started", Status: StatusRunning, Payload: mustJSON(payload)})
}

func (s *Store) Append(runID, sessionID, eventType string, status Status, payload any) error {
	return s.append(Event{RunID: runID, Session: sessionID, Type: eventType, Status: status, Payload: mustJSON(payload)})
}

func (s *Store) Finish(runID, sessionID string, status Status, payload any) error {
	if status == "" {
		status = StatusDone
	}
	return s.Append(runID, sessionID, "run_finished", status, payload)
}

func (s *Store) append(ev Event) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return errors.New("runstore is not initialized")
	}
	ev.RunID = strings.TrimSpace(ev.RunID)
	if ev.RunID == "" {
		return errors.New("run id cannot be empty")
	}
	if strings.TrimSpace(ev.Type) == "" {
		return errors.New("event type cannot be empty")
	}
	ev.Type = strings.TrimSpace(ev.Type)
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal run event: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(ev.RunID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open run journal: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write run journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync run journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close run journal: %w", err)
	}
	return nil
}

func (s *Store) Read(runID string) ([]Event, error) {
	if s == nil {
		return nil, errors.New("runstore is not initialized")
	}
	f, err := os.Open(s.path(runID))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("decode run journal %s: %w", runID, err)
		}
		out = append(out, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read run journal %s: %w", runID, err)
	}
	return out, nil
}

// List returns the latest state for every journal in deterministic order.
func (s *Store) List() ([]Summary, error) {
	if s == nil {
		return nil, errors.New("runstore is not initialized")
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		fileRunID := strings.TrimSuffix(entry.Name(), ".jsonl")
		events, err := s.Read(fileRunID)
		if err != nil || len(events) == 0 {
			continue
		}
		first, last := events[0], events[len(events)-1]
		runID := first.RunID
		if runID == "" {
			runID = fileRunID
		}
		summary := Summary{RunID: runID, SessionID: last.Session, Status: last.Status, StartedAt: first.At, UpdatedAt: last.At, Path: s.path(fileRunID)}
		if summary.Status == "" {
			summary.Status = StatusRunning
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	return out, nil
}

func (s *Store) Recoverable() ([]Summary, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, summary := range all {
		if summary.Status == StatusRunning || summary.Status == StatusWaiting {
			out = append(out, summary)
		}
	}
	return out, nil
}

func (s *Store) path(runID string) string {
	clean := strings.TrimSpace(runID)
	var b strings.Builder
	for _, r := range clean {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return filepath.Join(s.dir, b.String()+".jsonl")
}

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"error":"payload_not_serializable"}`)
	}
	return b
}
