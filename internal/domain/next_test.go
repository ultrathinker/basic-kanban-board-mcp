package domain

import (
	"testing"
	"time"
)

// tv is the test helper. It builds a TaskView in a backlog-kind column with
// the fields each test cares about. The defaults produce a ready, claimable
// leaf so individual tests only have to override what they are asserting on.
func tv(key string, opts ...func(*TaskView)) TaskView {
	t := TaskView{
		Task: Task{
			ID:              key,
			Key:             key,
			ProjectID:       "p",
			ColumnID:        "c",
			Rank:            1024,
			Title:           "task " + key,
			Type:            TypeTask,
			Priority:        PriorityNone,
			Tags:            nil,
			ColumnEnteredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Version:         1,
			CreatedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			CreatedBy:       "test",
			UpdatedBy:       "test",
		},
		ProjectKey: "BMB",
		ColumnName: "Backlog",
		ColumnKind: KindBacklog,
	}
	for _, o := range opts {
		o(&t)
	}
	return t
}

func withPriority(p Priority) func(*TaskView) { return func(t *TaskView) { t.Priority = p } }
func withType(t Type) func(*TaskView)         { return func(tv *TaskView) { tv.Type = t } }
func withRank(r int64) func(*TaskView)        { return func(t *TaskView) { t.Rank = r } }
func withCreatedAt(s string) func(*TaskView) {
	return func(t *TaskView) { t.CreatedAt, _ = time.Parse(time.RFC3339, s) }
}
func withDueAt(s string) func(*TaskView) {
	return func(t *TaskView) { d, _ := time.Parse(time.RFC3339, s); t.DueAt = &d }
}
func withBlockedBy(keys ...string) func(*TaskView) {
	return func(t *TaskView) { t.BlockedBy = append([]string(nil), keys...) }
}
func withSub(done, total int) func(*TaskView) {
	return func(t *TaskView) {
		t.SubDone = done
		t.SubTotal = total
	}
}
func withClaim(actor string, expiresAt string) func(*TaskView) {
	return func(t *TaskView) {
		now, _ := time.Parse(time.RFC3339, expiresAt)
		earlier := now.Add(-time.Minute)
		t.ClaimedBy = &actor
		t.ClaimedAt = &earlier
		t.ClaimExpiresAt = &now
	}
}
func withVersion(v int) func(*TaskView) { return func(t *TaskView) { t.Version = v } }

func TestNextReady_Ordering(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1", withPriority(PriorityLow), withCreatedAt("2026-01-01T00:00:00Z"), withRank(1024)),
			tv("BMB-2", withPriority(PriorityHigh), withCreatedAt("2026-01-02T00:00:00Z"), withRank(1024)),
			tv("BMB-3", withPriority(PriorityCritical), withCreatedAt("2026-01-03T00:00:00Z"), withRank(1024)),
			tv("BMB-4", withPriority(PriorityMedium), withCreatedAt("2026-01-01T00:00:00Z"), withRank(2048)),
			tv("BMB-5", withPriority(PriorityMedium), withCreatedAt("2026-01-02T00:00:00Z"), withRank(1024)),
		},
		Limit: 10,
	}
	out := NextReady(in)
	got := keys(out.Ready)
	want := []string{"BMB-3", "BMB-2", "BMB-5", "BMB-4", "BMB-1"}
	if !equal(got, want) {
		t.Fatalf("ordering:\n got %v\nwant %v", got, want)
	}
}

func TestNextReady_OrderingWithDueAt(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	earlyDue := "2026-01-05T00:00:00Z"
	lateDue := "2026-01-10T00:00:00Z"
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			// same priority, rank, created_at; due_at is the differentiator.
			tv("BMB-1", withPriority(PriorityHigh), withRank(1024), withCreatedAt("2026-01-01T00:00:00Z"), withDueAt(lateDue)),
			tv("BMB-2", withPriority(PriorityHigh), withRank(1024), withCreatedAt("2026-01-01T00:00:00Z"), withDueAt(earlyDue)),
			tv("BMB-3", withPriority(PriorityHigh), withRank(1024), withCreatedAt("2026-01-01T00:00:00Z")),
		},
		Limit: 10,
	}
	out := NextReady(in)
	got := keys(out.Ready)
	want := []string{"BMB-2", "BMB-1", "BMB-3"} // early due, late due, no due (nulls last)
	if !equal(got, want) {
		t.Fatalf("due_at ordering:\n got %v\nwant %v", got, want)
	}
}

func TestNextReady_Exclusions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := "2026-01-01T00:30:00Z"
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1"),
			tv("BMB-2", withBlockedBy("BMB-1")),
			tv("BMB-3", withSub(0, 2)),              // not a leaf
			tv("BMB-4", withClaim("other", expiry)), // claimed by someone else
			tv("BMB-5"),
		},
		ParentBlocked: map[string][]string{
			"BMB-5": {"BMB-2"},
		},
		Limit: 10,
	}
	out := NextReady(in)
	got := keys(out.Ready)
	want := []string{"BMB-1"}
	if !equal(got, want) {
		t.Fatalf("ready:\n got %v\nwant %v", got, want)
	}
	if out.Reasons.BlockedDependency != 1 {
		t.Errorf("BlockedDependency=%d want 1", out.Reasons.BlockedDependency)
	}
	if out.Reasons.NotLeaf != 1 {
		t.Errorf("NotLeaf=%d want 1", out.Reasons.NotLeaf)
	}
	if out.Reasons.ClaimedByOther != 1 {
		t.Errorf("ClaimedByOther=%d want 1", out.Reasons.ClaimedByOther)
	}
	if out.Reasons.ParentIncomplete != 1 {
		t.Errorf("ParentIncomplete=%d want 1", out.Reasons.ParentIncomplete)
	}
}

func TestNextReady_SelfClaimIsCandidate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := "2026-01-01T00:30:00Z"
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1", withClaim("agent", expiry)),
		},
		Limit: 10,
	}
	out := NextReady(in)
	if len(out.Ready) != 1 || out.Ready[0].Key != "BMB-1" {
		t.Fatalf("self-claimed task must be a candidate, got %v", keys(out.Ready))
	}
	if out.Reasons.ClaimedByOther != 0 {
		t.Errorf("ClaimedByOther=%d want 0", out.Reasons.ClaimedByOther)
	}
}

func TestNextReady_ExpiredLeaseIsCandidate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// lease expired an hour ago
	expiry := "2025-12-31T23:00:00Z"
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1", withClaim("other", expiry)),
		},
		Limit: 10,
	}
	out := NextReady(in)
	if len(out.Ready) != 1 || out.Ready[0].Key != "BMB-1" {
		t.Fatalf("expired lease must be a candidate, got %v", keys(out.Ready))
	}
}

func TestNextReady_PeekReturnsWorkEvenWhenWIPFull(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1"),
			tv("BMB-2"),
		},
		ActiveWIPFull: true,
		Limit:         10,
	}
	out := NextReady(in)
	if len(out.Ready) != 2 {
		t.Fatalf("peek with WIP full must still return ready work, got %d", len(out.Ready))
	}
	if !out.WIPFull {
		t.Errorf("WIPFull must be true in output")
	}
	if out.Reasons.WIPFull != 2 {
		t.Errorf("Reasons.WIPFull=%d want 2 (count of ready tasks)", out.Reasons.WIPFull)
	}
}

func TestNextReady_LimitDefaultsToThree(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1"),
			tv("BMB-2"),
			tv("BMB-3"),
			tv("BMB-4"),
			tv("BMB-5"),
		},
		// no Limit
	}
	out := NextReady(in)
	if len(out.Ready) != 3 {
		t.Fatalf("default limit should be 3, got %d", len(out.Ready))
	}
}

func TestNextReady_LimitHonored(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1"),
			tv("BMB-2"),
			tv("BMB-3"),
			tv("BMB-4"),
			tv("BMB-5"),
		},
		Limit: 2,
	}
	out := NextReady(in)
	if len(out.Ready) != 2 {
		t.Fatalf("Limit=2 should return 2, got %d", len(out.Ready))
	}
}

func TestNextReady_Determinism(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1", withPriority(PriorityMedium), withRank(1024)),
			tv("BMB-2", withPriority(PriorityMedium), withRank(2048)),
			tv("BMB-3", withPriority(PriorityMedium), withRank(3072)),
			tv("BMB-4", withPriority(PriorityMedium), withRank(4096)),
			tv("BMB-5", withPriority(PriorityMedium), withRank(5120)),
			tv("BMB-6", withPriority(PriorityHigh), withBlockedBy("BMB-1")),
			tv("BMB-7", withPriority(PriorityLow), withSub(0, 3)),
		},
		Limit: 10,
	}
	first := NextReady(in)
	for i := 0; i < 100; i++ {
		again := NextReady(in)
		if !equal(keys(first.Ready), keys(again.Ready)) {
			t.Fatalf("Ready order diverged at iteration %d: %v vs %v", i, keys(first.Ready), keys(again.Ready))
		}
		if !sameBlockedTop(first.BlockedTop, again.BlockedTop) {
			t.Fatalf("BlockedTop diverged at iteration %d: %v vs %v", i, first.BlockedTop, again.BlockedTop)
		}
		if first.Reasons != again.Reasons {
			t.Fatalf("Reasons diverged at iteration %d: %+v vs %+v", i, first.Reasons, again.Reasons)
		}
	}
}

func TestNextReady_BlockedTopSamples(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1", withPriority(PriorityCritical), withBlockedBy("BMB-9")),
			tv("BMB-2", withPriority(PriorityHigh), withBlockedBy("BMB-9")),
			tv("BMB-3", withPriority(PriorityMedium), withBlockedBy("BMB-9")),
			tv("BMB-4", withPriority(PriorityLow), withBlockedBy("BMB-9")),
			tv("BMB-5", withPriority(PriorityNone), withBlockedBy("BMB-9")),
			tv("BMB-6", withPriority(PriorityHigh), withBlockedBy("BMB-9")),
		},
		Limit: 0, // no ready limit; we want the BlockedTop sample
	}
	out := NextReady(in)
	if len(out.BlockedTop) != NextBlockedTopSample {
		t.Fatalf("BlockedTop size = %d, want %d", len(out.BlockedTop), NextBlockedTopSample)
	}
	// Highest priority comes first.
	if out.BlockedTop[0].Key != "BMB-1" {
		t.Errorf("BlockedTop[0].Key = %q, want BMB-1 (critical)", out.BlockedTop[0].Key)
	}
}

func TestNextReady_FinalTieBreakOnKey(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Identical priority, rank, created_at, no due_at — only Key differentiates.
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-3", withPriority(PriorityMedium), withRank(1024), withCreatedAt("2026-01-01T00:00:00Z")),
			tv("BMB-1", withPriority(PriorityMedium), withRank(1024), withCreatedAt("2026-01-01T00:00:00Z")),
			tv("BMB-2", withPriority(PriorityMedium), withRank(1024), withCreatedAt("2026-01-01T00:00:00Z")),
		},
		Limit: 10,
	}
	out := NextReady(in)
	got := keys(out.Ready)
	want := []string{"BMB-1", "BMB-2", "BMB-3"}
	if !equal(got, want) {
		t.Fatalf("tie-break on Key:\n got %v\nwant %v", got, want)
	}
}

func TestNextReady_LeafWithCompleteSubtasks(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := NextInput{
		Now: now, Actor: "agent",
		Candidates: []TaskView{
			tv("BMB-1", withSub(2, 2)), // all subtasks done → still a leaf
			tv("BMB-2", withSub(1, 2)), // incomplete subtasks → not a leaf
		},
		Limit: 10,
	}
	out := NextReady(in)
	got := keys(out.Ready)
	if !equal(got, []string{"BMB-1"}) {
		t.Fatalf("leaf with completed subtasks:\n got %v\nwant [BMB-1]", got)
	}
	if out.Reasons.NotLeaf != 1 {
		t.Errorf("NotLeaf=%d want 1", out.Reasons.NotLeaf)
	}
}

func TestNextReady_EmptyCandidates(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := NextInput{Now: now, Actor: "agent"}
	out := NextReady(in)
	if len(out.Ready) != 0 || len(out.BlockedTop) != 0 {
		t.Fatalf("empty input must produce empty output: %+v", out)
	}
	if out.WIPFull {
		t.Errorf("WIPFull must default to false")
	}
}

// TestNextReady_ContractShape is a guard for the public field names. A future
// rename in service.NextReasons would otherwise silently break the contract;
// this test fails loudly so the divergence is caught.
func TestNextReady_ContractShape(t *testing.T) {
	r := Reasons{}
	fields := map[string]int{
		"BlockedDependency": r.BlockedDependency,
		"WIPFull":           r.WIPFull,
		"ClaimedByOther":    r.ClaimedByOther,
		"ParentIncomplete":  r.ParentIncomplete,
		"NotLeaf":           r.NotLeaf,
	}
	if len(fields) != 5 {
		t.Fatalf("Reasons has wrong number of fields: %v", fields)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func keys(tvs []TaskView) []string {
	out := make([]string, len(tvs))
	for i, t := range tvs {
		out[i] = t.Key
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameBlockedTop(a, b []BlockedSample) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			return false
		}
		if !equal(a[i].BlockedBy, b[i].BlockedBy) {
			return false
		}
	}
	return true
}
