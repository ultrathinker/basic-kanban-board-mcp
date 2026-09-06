package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// ---------------------------------------------------------------------------
// Idempotency request hashing and response storage
//
// task_create is the only tool that uses (token_id, idempotency_key) as a
// retry safety net: a client that crashed mid-call replays with the same
// key and gets the original response, while a client that re-uses a key
// with different content gets a CodeIdempotencyMismatch error rather than
// silently overwriting the original audit record.
//
// The hash covers the content fields an LLM actually means to be
// "the same request" — everything except Ref (a client-chosen handle, not
// a content signal) and IdempotencyKey (the slot, not the content).
// ---------------------------------------------------------------------------

// idemContent is the deterministic projection of NewTask used to compare
// replay requests. Fields are listed in a fixed order so two requests
// that differ only in map iteration order hash the same.
type idemContent struct {
	ProjectKey string          `json:"project"`
	Title      string          `json:"title"`
	Body       string          `json:"body"`
	Type       domain.Type     `json:"type"`
	Priority   domain.Priority `json:"priority"`
	Estimate   *float64        `json:"estimate,omitempty"`
	Tags       []string        `json:"tags"`
	Assignee   *string         `json:"assignee,omitempty"`
	Column     string          `json:"column"`
	Parent     string          `json:"parent"`
	BlockedBy  []string        `json:"blocked_by"`
	Acceptance []string        `json:"acceptance"`
	DueAt      *time.Time      `json:"due_at,omitempty"`
	Metadata   map[string]any  `json:"metadata"`
}

// hashNewTask projects n into idemContent, encodes it as canonical JSON
// and returns a sha256 hex digest. The canonical form sorts the metadata
// keys (handled by sortedMetadata) and the slice fields, so two requests
// with semantically-equal inputs hash identically regardless of the order
// the caller used.
func hashNewTask(n NewTask) (string, error) {
	idem := idemContent{
		ProjectKey: domain.NormalizeProjectKey(n.ProjectKey),
		Title:      n.Title,
		Body:       n.Body,
		Type:       n.Type,
		Priority:   n.Priority,
		Estimate:   n.Estimate,
		Assignee:   n.Assignee,
		Column:     n.Column,
		Parent:     n.Parent,
		Metadata:   sortedMetadata(n.Metadata),
	}
	if n.Tags != nil {
		idem.Tags = append([]string(nil), n.Tags...)
		sort.Strings(idem.Tags)
	}
	if n.BlockedBy != nil {
		idem.BlockedBy = append([]string(nil), n.BlockedBy...)
		sort.Strings(idem.BlockedBy)
	}
	if n.Acceptance != nil {
		idem.Acceptance = append([]string(nil), n.Acceptance...)
		sort.Strings(idem.Acceptance)
	}
	if n.DueAt != nil {
		t := n.DueAt.UTC()
		idem.DueAt = &t
	}
	enc, err := json.Marshal(idem)
	if err != nil {
		return "", fmt.Errorf("idempotency: hash new task: %w", err)
	}
	sum := sha256.Sum256(enc)
	return hex.EncodeToString(sum[:]), nil
}

// sortedMetadata returns a copy of m with keys sorted, so map iteration
// order never affects the hash. A nil or empty input produces a non-nil
// empty map so the absence of metadata hashes the same regardless of
// whether the caller passed nil or an empty map.
func sortedMetadata(m map[string]any) map[string]any {
	if len(m) == 0 {
		return map[string]any{}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(m))
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}

// encodeTaskView serializes a TaskView for the idempotency store. The
// stored bytes are the source of truth for replay: anything the caller
// would have seen in the original response must round-trip through this.
func encodeTaskView(tv domain.TaskView) ([]byte, error) {
	enc, err := json.Marshal(tv)
	if err != nil {
		return nil, fmt.Errorf("idempotency: encode task view: %w", err)
	}
	return enc, nil
}

// decodeTaskView is the inverse of encodeTaskView. Used by TaskCreate's
// replay path to materialise a stored response back into the result.
func decodeTaskView(b []byte) (domain.TaskView, error) {
	var tv domain.TaskView
	if err := json.Unmarshal(b, &tv); err != nil {
		return domain.TaskView{}, fmt.Errorf("idempotency: decode task view: %w", err)
	}
	return tv, nil
}
