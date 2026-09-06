package store

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// A not-found error is the store's answer to "I asked for X and it is not
// here", so it has exactly one job beyond the code: name X. These tests exist
// because it once did not. scanProject and scanToken are handed a *sql.Row and
// have no idea which key the caller queried with, so they filled the slot with
// a literal "?" — and that placeholder travelled all the way to an MCP client
// as `project "?" not found`. An agent creating a batch across several
// projects could not tell which key was wrong.
//
// The rule these pin: whoever still holds the identifier builds the error.

// notFoundMessage runs fn inside a read transaction and returns the message of
// the *domain.Error it produced, failing the test if it produced anything else.
func notFoundMessage(t *testing.T, s Store, fn func(tx Tx) error) string {
	t.Helper()
	var msg string
	if err := s.Read(context.Background(), func(tx Tx) error {
		err := fn(tx)
		de := domain.AsError(err)
		if de == nil {
			t.Fatalf("want *domain.Error, got %v", err)
		}
		if de.Code != domain.CodeNotFound {
			t.Fatalf("code = %q, want %q (message %q)", de.Code, domain.CodeNotFound, de.Message)
		}
		msg = de.Message
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
	return msg
}

func TestNotFound_NamesTheIdentifierAsked(t *testing.T) {
	ts := openTestStore(t)
	seedProject(t, ts)

	cases := []struct {
		name string
		want string
		fn   func(tx Tx) error
	}{
		{
			// The exact defect from the live run: task_create against a
			// missing project resolved the project by key first.
			name: "project by key",
			want: "NOPE",
			fn: func(tx Tx) error {
				_, err := ts.Projects().GetByKey(tx, "NOPE")
				return err
			},
		},
		{
			name: "project by id",
			want: "11111111-2222-3333-4444-555555555555",
			fn: func(tx Tx) error {
				_, err := ts.Projects().GetByID(tx, "11111111-2222-3333-4444-555555555555")
				return err
			},
		},
		{
			name: "task by key",
			want: "BMB-404",
			fn: func(tx Tx) error {
				_, err := ts.Tasks().GetByKey(tx, "BMB-404")
				return err
			},
		},
		{
			name: "column by name",
			want: "Nowhere",
			fn: func(tx Tx) error {
				_, err := ts.Columns().GetByName(tx, uuid.NewString(), "Nowhere")
				return err
			},
		},
		{
			name: "token by name",
			want: "ghost-token",
			fn: func(tx Tx) error {
				_, err := ts.Tokens().GetByName(tx, "ghost-token")
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := notFoundMessage(t, ts, tc.fn)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not name %q", msg, tc.want)
			}
			if strings.Contains(msg, `"?"`) {
				t.Errorf("message %q still carries the %q placeholder", msg, "?")
			}
			if strings.Contains(msg, `""`) {
				t.Errorf("message %q names an empty identifier, which is no better than %q", msg, "?")
			}
		})
	}
}

// TestNotFound_TokenByHashNamesTheLookupNotTheSecret is the one lookup whose
// identifier must not be echoed: it is the caller's credential. The message
// still has to say what was not found, so it names the lookup instead — but it
// may never contain the hash.
func TestNotFound_TokenByHashNamesTheLookupNotTheSecret(t *testing.T) {
	ts := openTestStore(t)
	sum := sha256.Sum256([]byte("no-such-secret"))
	hash := sum[:]

	msg := notFoundMessage(t, ts, func(tx Tx) error {
		_, err := ts.Tokens().GetByHash(tx, hash)
		return err
	})
	if strings.Contains(msg, `"?"`) {
		t.Errorf("message %q still carries the %q placeholder", msg, "?")
	}
	if !strings.Contains(msg, "token") {
		t.Errorf("message %q does not say what was not found", msg)
	}
	for _, leak := range []string{string(hash), "no-such-secret"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message %q leaks the credential", msg)
		}
	}
}
