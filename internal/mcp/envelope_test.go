package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// TestNewErrorEnvelope_PerCode is the table-driven check that the wire
// envelope preserves every domain field for every domain.Code. It also
// pins `current` to the conflict code only — dropping it on, say, a
// blocked error would silently break the merge-without-reread contract,
// and the omission test here is the only thing that would notice.
func TestNewErrorEnvelope_PerCode(t *testing.T) {
	t.Parallel()
	type tc struct {
		code           domain.Code
		make           func() *domain.Error
		wantField      bool // whether the envelope should carry Field
		wantCurrent    bool // whether the envelope should carry Current
		wantCurrentVal any  // exact value to compare when wantCurrent
	}
	cases := []tc{
		{
			code: domain.CodeNotFound,
			make: func() *domain.Error {
				return domain.NotFound("task", "BMB-42")
			},
		},
		{
			code: domain.CodeValidation,
			make: func() *domain.Error {
				return domain.Invalid("priority", `"foo" is not a valid priority`, "Use one of: none, low, medium, high, critical.")
			},
			wantField: true,
		},
		{
			code: domain.CodeConflict,
			make: func() *domain.Error {
				cur := &domain.TaskView{ProjectKey: "BMB"}
				return domain.Conflict(cur, 3, 4)
			},
			wantCurrent:    true,
			wantCurrentVal: &domain.TaskView{ProjectKey: "BMB"},
		},
		{
			code: domain.CodeBlocked,
			make: func() *domain.Error {
				return domain.Blocked("BMB-14", []string{"BMB-9", "BMB-12"})
			},
		},
		{
			code: domain.CodeWIPExceeded,
			make: func() *domain.Error {
				return domain.WIPExceeded("Doing", 3)
			},
		},
		{
			code: domain.CodeClaimed,
			make: func() *domain.Error {
				return domain.Claimed("BMB-14", "claude@rog")
			},
		},
		{
			code: domain.CodeForbidden,
			make: func() *domain.Error {
				return domain.Forbidden("admin scope required", "Use an admin token.")
			},
		},
		{
			code: domain.CodeCycle,
			make: func() *domain.Error {
				return domain.Cycle([]string{"BMB-1", "BMB-2", "BMB-3", "BMB-1"})
			},
		},
		{
			code: domain.CodeRateLimited,
			make: func() *domain.Error {
				return &domain.Error{
					Code:        domain.CodeRateLimited,
					Message:     "600 req/min exceeded",
					Remediation: "Wait a moment and retry; batch your writes to stay under the limit.",
				}
			},
		},
		{
			code: domain.CodePayloadTooLarge,
			make: func() *domain.Error {
				return &domain.Error{
					Code:        domain.CodePayloadTooLarge,
					Message:     "body is 1.5 MB, limit is 1 MB",
					Remediation: "Split the batch or trim the body field.",
				}
			},
		},
		{
			code: domain.CodeIdempotencyMismatch,
			make: func() *domain.Error {
				return &domain.Error{
					Code:        domain.CodeIdempotencyMismatch,
					Message:     "idempotency key already used with a different request",
					Remediation: "Use a fresh idempotency_key for each distinct request.",
				}
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(string(c.code), func(t *testing.T) {
			t.Parallel()
			derr := c.make()
			if derr == nil {
				t.Fatalf("nil error from make")
			}
			env := newErrorEnvelope(derr)
			if env == nil {
				t.Fatalf("envelope is nil")
			}
			if env.Code != string(c.code) {
				t.Errorf("code = %q, want %q", env.Code, c.code)
			}
			if env.Message != derr.Message {
				t.Errorf("message = %q, want %q", env.Message, derr.Message)
			}
			if env.Remediation != derr.Remediation {
				t.Errorf("remediation = %q, want %q", env.Remediation, derr.Remediation)
			}
			if c.wantField && env.Field == "" {
				t.Errorf("Field should be set for %s", c.code)
			}
			if !c.wantField && env.Field != "" {
				t.Errorf("Field should be empty for %s, got %q", c.code, env.Field)
			}
			if c.wantCurrent {
				if env.Current == nil {
					t.Errorf("Current should be set for %s", c.code)
				}
				// Compare via JSON round-trip: addresses differ, contents must not.
				got, _ := json.Marshal(env.Current)
				want, _ := json.Marshal(c.wantCurrentVal)
				if string(got) != string(want) {
					t.Errorf("current payload mismatch:\n got: %s\nwant: %s", got, want)
				}
			}
			if !c.wantCurrent && env.Current != nil {
				t.Errorf("Current should be empty for %s, got %v", c.code, env.Current)
			}
		})
	}
}

// TestNewErrorEnvelope_Nil is a small guard so the call sites in this
// package can hand the envelope into an omitempty field without nil checks.
func TestNewErrorEnvelope_Nil(t *testing.T) {
	t.Parallel()
	if got := newErrorEnvelope(nil); got != nil {
		t.Fatalf("newErrorEnvelope(nil) = %+v, want nil", got)
	}
}

// TestAsDomainError_DomainAndOther pins both branches of asDomainError:
// *domain.Error passes through, anything else wraps as a validation error
// with a remediation (per AGENTS.md "fail loud").
func TestAsDomainError_DomainAndOther(t *testing.T) {
	t.Parallel()

	t.Run("domain_passes_through", func(t *testing.T) {
		t.Parallel()
		orig := domain.NotFound("task", "BMB-1")
		got := asDomainError(orig)
		if got != orig {
			t.Errorf("got %p, want original %p", got, orig)
		}
	})

	t.Run("non_domain_wraps_validation", func(t *testing.T) {
		t.Parallel()
		got := asDomainError(errService)
		if got == nil {
			t.Fatalf("got nil")
		}
		if got.Code != domain.CodeValidation {
			t.Errorf("code = %s, want %s", got.Code, domain.CodeValidation)
		}
		if !strings.Contains(got.Message, "kaboom") {
			t.Errorf("message %q does not contain the underlying cause", got.Message)
		}
		if got.Remediation == "" {
			t.Errorf("remediation is empty; callers need a hint")
		}
	})

	t.Run("nil_returns_nil", func(t *testing.T) {
		t.Parallel()
		if got := asDomainError(nil); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
}

// TestErrorEnvelope_JSONMatchesContract checks the wire shape that goes
// onto the JSON-RPC socket. Field names must be the canonical ones used by
// every MCP client (PLAN §6 envelope): code / message / remediation /
// current / field.
func TestErrorEnvelope_JSONMatchesContract(t *testing.T) {
	t.Parallel()
	cur := map[string]any{"key": "BMB-14", "version": 7}
	derr := domain.Conflict(cur, 3, 7)
	env := newErrorEnvelope(derr)
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"code", "message", "remediation", "current"} {
		if _, ok := got[key]; !ok {
			t.Errorf("wire form missing %q: %s", key, b)
		}
	}
	if got["code"] != "conflict" {
		t.Errorf("code = %v, want \"conflict\"", got["code"])
	}
}
