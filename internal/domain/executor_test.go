package domain

import (
	"strings"
	"testing"
	"time"
)

func TestScopes_ExecutorPlacement(t *testing.T) {
	for _, tc := range []struct {
		have                      Scopes
		read, executor, write, ok bool // ok = OwnCardsOnly
	}{
		{Scopes{ScopeRead}, true, false, false, false},
		{Scopes{ScopeExecutor}, true, true, false, true},
		{Scopes{ScopeWrite}, true, true, true, false},
		{Scopes{ScopeAdmin}, true, true, true, false},
		{Scopes{ScopeExecutor, ScopeWrite}, true, true, true, false},
		{Scopes{}, false, false, false, false},
	} {
		if got := tc.have.Has(ScopeRead); got != tc.read {
			t.Errorf("%v Has(read) = %v", tc.have, got)
		}
		if got := tc.have.Has(ScopeExecutor); got != tc.executor {
			t.Errorf("%v Has(executor) = %v", tc.have, got)
		}
		if got := tc.have.Has(ScopeWrite); got != tc.write {
			t.Errorf("%v Has(write) = %v — an executor key must never pass a write check", tc.have, got)
		}
		if got := tc.have.OwnCardsOnly(); got != tc.ok {
			t.Errorf("%v OwnCardsOnly() = %v", tc.have, got)
		}
	}
	if !ScopeExecutor.Valid() {
		t.Error("executor is not a valid scope")
	}
}

func TestToken_ActiveAt(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(time.Second), now.Add(-time.Second)
	for _, tc := range []struct {
		label string
		tok   Token
		want  bool
	}{
		{"no expiry", Token{}, true},
		{"expires later", Token{ExpiresAt: &later}, true},
		{"expires exactly now", Token{ExpiresAt: &now}, false},
		{"expired", Token{ExpiresAt: &earlier}, false},
		{"revoked, not expired", Token{RevokedAt: &earlier, ExpiresAt: &later}, false},
	} {
		tok := tc.tok
		if got := tok.ActiveAt(now); got != tc.want {
			t.Errorf("%s: ActiveAt = %v, want %v", tc.label, got, tc.want)
		}
	}
	var nilTok *Token
	if nilTok.ActiveAt(now) {
		t.Error("a nil token is active")
	}
}

func TestExecutorKeyTTL(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
		ok      bool
	}{
		{0, 24 * time.Hour, true},
		{300, 5 * time.Minute, true},
		{299, 0, false},
		{7 * 24 * 3600, 7 * 24 * time.Hour, true},
		{7*24*3600 + 1, 0, false},
		{-1, 0, false},
	} {
		got, err := ExecutorKeyTTL(tc.seconds)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ExecutorKeyTTL(%d) = %v, %v; want %v ok=%v", tc.seconds, got, err, tc.want, tc.ok)
		}
		if err != nil && AsError(err).Code != CodeValidation {
			t.Errorf("ExecutorKeyTTL(%d) code %s, want validation", tc.seconds, AsError(err).Code)
		}
	}
}

func TestExecutorMayMove(t *testing.T) {
	backlog := Column{ID: "b", Name: "Backlog", Kind: KindBacklog}
	doing := Column{ID: "d", Name: "Doing", Kind: KindActive}
	review := Column{ID: "r", Name: "Review", Kind: KindActive}
	waiting := Column{ID: "w", Name: "Waiting", Kind: KindWaiting}
	done := Column{ID: "x", Name: "Done", Kind: KindDone}
	for _, tc := range []struct {
		from, to Column
		ok       bool
	}{
		{backlog, doing, true},
		{doing, review, true},
		{review, waiting, true},
		{review, backlog, true},
		{review, done, false},
		{backlog, done, false},
		{done, doing, false},
		{done, backlog, false},
		{done, done, true}, // re-ranking inside Done moves nothing across the boundary
	} {
		err := ExecutorMayMove("exec-1", "BMB-1", tc.from, tc.to)
		if (err == nil) != tc.ok {
			t.Errorf("%s -> %s: %v, want ok=%v", tc.from.Name, tc.to.Name, err, tc.ok)
		}
		if err != nil && AsError(err).Code != CodeForbidden {
			t.Errorf("%s -> %s: code %s, want forbidden", tc.from.Name, tc.to.Name, AsError(err).Code)
		}
	}
}

func TestExecutorOwns(t *testing.T) {
	mine, other, folded := "exec-1", "exec-2", "Exec-1"
	for _, tc := range []struct {
		assignee *string
		ok       bool
	}{
		{&mine, true},
		{&other, false},
		{&folded, false},
		{nil, false},
	} {
		err := ExecutorOwns("exec-1", &Task{Key: "BMB-1", Assignee: tc.assignee})
		if (err == nil) != tc.ok {
			t.Errorf("assignee %v: %v, want ok=%v", tc.assignee, err, tc.ok)
		}
	}
}

func TestValidateTokenName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"claude-exec-1", true},
		{"", false},
		{"two words", false},
		{"tab\tname", false},
		{string(make([]byte, MaxAssigneeLen+1)), false},
	} {
		if err := ValidateTokenName("name", tc.name); (err == nil) != tc.ok {
			t.Errorf("ValidateTokenName(%q) = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// KANB-68: the marker of a participant without a key must be recognised,
// and nothing that could be a real digest may pass for it.
func TestHasNoSecret(t *testing.T) {
	real := make([]byte, 32)
	copy(real, "participant-without-key:")
	cases := []struct {
		label string
		tok   *Token
		want  bool
	}{
		{"marker", &Token{Hash: NoSecretHash("id-1")}, true},
		{"a 32-byte hash that starts like the marker", &Token{Hash: real}, false},
		{"an ordinary hash", &Token{Hash: []byte("0123456789abcdef0123456789abcdef")}, false},
		{"another prefix", &Token{Hash: []byte("hash-id-1")}, false},
		{"nil token", nil, false},
	}
	for _, tc := range cases {
		if got := tc.tok.HasNoSecret(); got != tc.want {
			t.Errorf("%s: HasNoSecret = %v, want %v", tc.label, got, tc.want)
		}
	}
	if string(NoSecretHash("a")) == string(NoSecretHash("b")) {
		t.Error("two rows share one marker; the hash column is UNIQUE")
	}
}

func TestMergeProjectKeys(t *testing.T) {
	cases := []struct {
		have, more    []string
		merged, added string
	}{
		{[]string{"BMB"}, []string{"OPS"}, "BMB,OPS", "OPS"},
		{[]string{"BMB", "OPS"}, []string{"ops", "BMB"}, "BMB,OPS", ""},
		{nil, []string{"BMB"}, "BMB", "BMB"},
		{[]string{"BMB"}, nil, "BMB", ""},
	}
	for _, tc := range cases {
		merged, added := MergeProjectKeys(tc.have, tc.more)
		if strings.Join(merged, ",") != tc.merged || strings.Join(added, ",") != tc.added {
			t.Errorf("MergeProjectKeys(%v, %v) = %v, %v; want %s, %s", tc.have, tc.more, merged, added, tc.merged, tc.added)
		}
	}
}
