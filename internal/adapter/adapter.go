// Package adapter implements the durable delivery sidecar for one coding CLI.
package adapter

import (
	"context"
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

// DeliveryState records the durable boundary between the adapter and a CLI.
type DeliveryState string

const (
	// DeliveryPending has not crossed into a CLI process.
	DeliveryPending DeliveryState = "pending"
	// DeliveryAttempting was durably handed to a target session before the CLI call.
	DeliveryAttempting DeliveryState = "attempting"
	// DeliveryConfirmed was acknowledged by the CLI invocation.
	DeliveryConfirmed DeliveryState = "confirmed"
)

// FeedMessage is the small feed projection the adapter needs to deliver work.
type FeedMessage struct {
	ID               string `json:"id"`
	CreatedAt        string `json:"created_at"`
	Author           string `json:"author"`
	Kind             string `json:"kind"`
	Recipient        string `json:"recipient,omitempty"`
	ResolvedExecutor string `json:"resolved_executor,omitempty"`
	Body             string `json:"body"`
}

// FeedPage is the cursor-paged response from board_get(view:"messages").
type FeedPage struct {
	Messages   []FeedMessage `json:"messages"`
	NextCursor string        `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
}

// QueuedMessage is a received feed entry and its durable delivery state.
type QueuedMessage struct {
	FeedMessage
	TargetSession string        `json:"target_session"`
	Delivery      DeliveryState `json:"delivery"`
	AttemptedAt   *time.Time    `json:"attempted_at,omitempty"`
	ConfirmedAt   *time.Time    `json:"confirmed_at,omitempty"`
}

// State is intentionally local-only. It has no board secrets or process control.
type State struct {
	Version  int             `json:"version"`
	BoardURL string          `json:"board_url"`
	Project  string          `json:"project"`
	Identity string          `json:"identity"`
	Cursor   string          `json:"cursor"`
	Queue    []QueuedMessage `json:"queue"`
	Workers  []Worker        `json:"workers,omitempty"`
	Journal  []JournalEntry  `json:"journal,omitempty"`
}

// Worker is local orchestration context, never a board entity. It contains no
// token secret and grants no process authority outside this adapter.
type Worker struct {
	WorkerID    string     `json:"worker_id"`
	Role        string     `json:"role"`
	Provider    string     `json:"provider"`
	SessionID   string     `json:"session_id,omitempty"`
	WorkDir     string     `json:"work_dir"`
	CurrentTask string     `json:"current_task,omitempty"`
	Checkpoint  Checkpoint `json:"checkpoint"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Checkpoint is the bounded handoff record used when a CLI cannot resume.
type Checkpoint struct {
	Done       string `json:"done,omitempty"`
	Version    string `json:"version,omitempty"`
	Unresolved string `json:"unresolved,omitempty"`
	NextStep   string `json:"next_step,omitempty"`
}

// JournalEntry makes resume fallbacks explicit instead of hiding them in an
// inferred worker status.
type JournalEntry struct {
	At       time.Time `json:"at"`
	WorkerID string    `json:"worker_id"`
	Kind     string    `json:"kind"`
	Detail   string    `json:"detail"`
}

// Assignment tells a caller whether the same CLI session can be continued.
type Assignment struct {
	Worker   Worker
	Resume   bool
	Prompt   string
	Fallback string
}

// Store owns an adapter state file and makes every state transition durable.
type Store struct {
	mu    sync.Mutex
	path  string
	state State
}

// OpenStore opens or initializes durable adapter state at path.
func OpenStore(path, boardURL, project, identity string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("adapter state path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create adapter state directory: %w", err)
	}
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.state = State{Version: 1, BoardURL: boardURL, Project: project, Identity: identity, Queue: []QueuedMessage{}}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read adapter state: %w", err)
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		return nil, fmt.Errorf("decode adapter state: %w", err)
	}
	if s.state.Version != 1 {
		return nil, fmt.Errorf("unsupported adapter state version %d", s.state.Version)
	}
	if s.state.BoardURL != boardURL || s.state.Project != project || s.state.Identity != identity {
		return nil, errors.New("adapter state belongs to a different board, project, or identity")
	}
	return s, nil
}

// Snapshot returns a detached copy suitable for display and tests.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

// Receive persists a complete feed page and its cursor together before any
// caller may deliver it. A repeated page cannot duplicate a session target.
func (s *Store) Receive(page FeedPage, targetSession string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, msg := range page.Messages {
		if !addressedTo(msg, s.state.Identity) || queued(s.state.Queue, msg.ID, targetSession) {
			continue
		}
		s.state.Queue = append(s.state.Queue, QueuedMessage{FeedMessage: msg, TargetSession: targetSession, Delivery: DeliveryPending})
	}
	s.state.Cursor = page.NextCursor
	return s.saveLocked()
}

// NextDeliverable returns the highest-priority pending message. Incomplete
// attempts deliberately never return here: retrying them could duplicate work.
func (s *Store) NextDeliverable() *QueuedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := pendingIndexes(s.state.Queue)
	if len(items) == 0 {
		return nil
	}
	sort.SliceStable(items, func(i, j int) bool { return stopFirst(s.state.Queue[items[i]], s.state.Queue[items[j]]) })
	q := s.state.Queue[items[0]]
	return &q
}

// BeginDelivery records the external-process boundary before any CLI call.
func (s *Store) BeginDelivery(messageID, sessionID string, now time.Time) (QueuedMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Queue {
		q := &s.state.Queue[i]
		if q.ID != messageID || q.TargetSession != sessionID {
			continue
		}
		if q.Delivery != DeliveryPending {
			return QueuedMessage{}, fmt.Errorf("message %s is %s, not pending", messageID, q.Delivery)
		}
		t := now.UTC()
		q.Delivery = DeliveryAttempting
		q.AttemptedAt = &t
		if err := s.saveLocked(); err != nil {
			return QueuedMessage{}, err
		}
		return *q, nil
	}
	return QueuedMessage{}, fmt.Errorf("pending message %s for session %s not found", messageID, sessionID)
}

// ConfirmDelivery records the CLI's actual acknowledgement.
func (s *Store) ConfirmDelivery(messageID, sessionID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Queue {
		q := &s.state.Queue[i]
		if q.ID != messageID || q.TargetSession != sessionID {
			continue
		}
		if q.Delivery != DeliveryAttempting {
			return fmt.Errorf("message %s is %s, not attempting", messageID, q.Delivery)
		}
		t := now.UTC()
		q.Delivery = DeliveryConfirmed
		q.ConfirmedAt = &t
		return s.saveLocked()
	}
	return fmt.Errorf("attempting message %s for session %s not found", messageID, sessionID)
}

// ResetAttempt makes a human-approved retry possible. It is never called by polling.
func (s *Store) ResetAttempt(messageID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Queue {
		q := &s.state.Queue[i]
		if q.ID == messageID && q.TargetSession == sessionID {
			if q.Delivery != DeliveryAttempting {
				return fmt.Errorf("message %s is %s, not awaiting a decision", messageID, q.Delivery)
			}
			q.Delivery, q.AttemptedAt = DeliveryPending, nil
			return s.saveLocked()
		}
	}
	return fmt.Errorf("message %s for session %s not found", messageID, sessionID)
}

// UpsertWorker records a worker's durable environment association. SessionID
// may be empty only for a worker that has not yet started a CLI session.
func (s *Store) UpsertWorker(worker Worker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(worker.WorkerID) == "" || strings.TrimSpace(worker.Role) == "" || strings.TrimSpace(worker.Provider) == "" || strings.TrimSpace(worker.WorkDir) == "" {
		return errors.New("worker id, role, provider, and work directory are required")
	}
	worker.UpdatedAt = time.Now().UTC()
	for i := range s.state.Workers {
		if s.state.Workers[i].WorkerID == worker.WorkerID {
			if worker.SessionID == "" {
				worker.SessionID = s.state.Workers[i].SessionID
			}
			if worker.CurrentTask == "" {
				worker.CurrentTask = s.state.Workers[i].CurrentTask
			}
			if worker.Checkpoint == (Checkpoint{}) {
				worker.Checkpoint = s.state.Workers[i].Checkpoint
			}
			s.state.Workers[i] = worker
			return s.saveLocked()
		}
	}
	s.state.Workers = append(s.state.Workers, worker)
	return s.saveLocked()
}

// PrepareAssignment selects a saved session for related work or logs the
// explicit checkpoint fallback when that session is unavailable.
func (s *Store) PrepareAssignment(workerID, task string, resumeAvailable bool) (Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Workers {
		w := &s.state.Workers[i]
		if w.WorkerID != workerID {
			continue
		}
		w.CurrentTask, w.UpdatedAt = task, time.Now().UTC()
		a := Assignment{Worker: *w}
		if resumeAvailable && w.SessionID != "" {
			a.Resume = true
			a.Prompt = "Continue the saved worker session with the current contract and checkpoint."
		} else {
			a.Fallback = "resume_unavailable_checkpoint_loaded"
			a.Prompt = checkpointPrompt(w.Checkpoint)
			s.state.Journal = append(s.state.Journal, JournalEntry{At: w.UpdatedAt, WorkerID: workerID, Kind: a.Fallback, Detail: "new session must start from checkpoint"})
		}
		if err := s.saveLocked(); err != nil {
			return Assignment{}, err
		}
		a.Worker = *w
		return a, nil
	}
	return Assignment{}, fmt.Errorf("worker %s not found", workerID)
}

// RecordCheckpoint replaces the bounded context handoff after a work stage.
func (s *Store) RecordCheckpoint(workerID string, checkpoint Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Workers {
		w := &s.state.Workers[i]
		if w.WorkerID == workerID {
			w.Checkpoint, w.UpdatedAt = checkpoint, time.Now().UTC()
			return s.saveLocked()
		}
	}
	return fmt.Errorf("worker %s not found", workerID)
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode adapter state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".adapter-state-*")
	if err != nil {
		return fmt.Errorf("create adapter state temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write adapter state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync adapter state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close adapter state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace adapter state: %w", err)
	}
	return nil
}

func cloneState(in State) State {
	b, _ := json.Marshal(in)
	var out State
	_ = json.Unmarshal(b, &out)
	return out
}

func queued(queue []QueuedMessage, id, session string) bool {
	for _, q := range queue {
		if q.ID == id && q.TargetSession == session {
			return true
		}
	}
	return false
}

func addressedTo(m FeedMessage, identity string) bool {
	return m.ResolvedExecutor == identity || m.Recipient == identity || m.Recipient == "all"
}

func pendingIndexes(queue []QueuedMessage) []int {
	indexes := make([]int, 0, len(queue))
	for i, q := range queue {
		if q.Delivery == DeliveryPending {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func stopFirst(a, b QueuedMessage) bool { return isStop(a) && !isStop(b) }

func isStop(m QueuedMessage) bool {
	if m.Kind != "command" {
		return false
	}
	body := strings.Fields(strings.ToLower(strings.TrimSpace(m.Body)))
	return len(body) > 0 && (body[0] == "stop" || body[0] == "halt")
}

func checkpointPrompt(c Checkpoint) string {
	return fmt.Sprintf("Resume is unavailable. Start a fresh session from checkpoint: done=%s; version=%s; unresolved=%s; next=%s", c.Done, c.Version, c.Unresolved, c.NextStep)
}

// Runner delivers a message to a particular CLI session and returns only after
// that CLI reports acceptance. It deliberately has no retry method.
type Runner interface {
	Deliver(context.Context, string, QueuedMessage) error
}

// DeliverNext performs one durable delivery. A failed call remains attempting
// and therefore requires an explicit operator reset before it may be retried.
func DeliverNext(ctx context.Context, store *Store, runner Runner, now func() time.Time) error {
	q := store.NextDeliverable()
	if q == nil {
		return nil
	}
	attempt, err := store.BeginDelivery(q.ID, q.TargetSession, now())
	if err != nil {
		return err
	}
	if err := runner.Deliver(ctx, attempt.TargetSession, attempt); err != nil {
		return fmt.Errorf("deliver %s: %w", attempt.ID, err)
	}
	return store.ConfirmDelivery(attempt.ID, attempt.TargetSession, now())
}
