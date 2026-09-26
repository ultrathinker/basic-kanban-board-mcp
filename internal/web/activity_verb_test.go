package web

import (
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// KANB-68: one event type reports three acts on the participant list; the
// activity feed must not call a renewal or a secretless participant a newly
// issued key.
func TestActivityVerb_ExecutorKeyEventsSayWhatHappened(t *testing.T) {
	cases := []struct {
		payload map[string]any
		want    string
	}{
		{map[string]any{"name": "exec-1"}, "issued an executor key for"},
		{map[string]any{"name": "exec-1", "reissued": true}, "issued an executor key for"},
		{map[string]any{"name": "exec-1", "renewed": true}, "renewed an executor key for"},
		{map[string]any{"name": "owner", "participant_only": true}, "added a participant without a key to"},
		{nil, "issued an executor key for"},
	}
	for _, tc := range cases {
		got := activityVerb(domain.Event{Type: domain.EventExecutorKeyIssued, Payload: tc.payload})
		if got != tc.want {
			t.Errorf("payload %v: verb %q, want %q", tc.payload, got, tc.want)
		}
	}
	if got := activityVerb(domain.Event{Type: domain.EventTaskCreated, Payload: map[string]any{"renewed": true}}); got != "created" {
		t.Errorf("another event type picked up the refinement: %q", got)
	}
}
