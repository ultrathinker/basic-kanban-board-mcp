package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// KANB-60: executor_key_issue is a thin adapter — the rules live in the
// service — so these tests pin the translation: arguments forwarded verbatim,
// the secret carried back exactly once, refusals as the standard envelope.

func TestExecutorKeyIssue_ForwardsAndReturnsSecret(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	expires := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	svc.DefaultExecutorKeyIssue = &service.ExecutorKeyIssueResult{
		TokenID: "tok-1", Name: "Exec-1", ProjectKeys: []string{"BMB", "OPS"},
		ExpiresAt: expires, Secret: "kbn_s3cr3t",
	}

	res, sc := callTool(t, cs, "executor_key_issue", map[string]any{
		"name": "Exec-1", "projects": []string{"bmb", "ops"}, "ttl_seconds": 3600,
	})
	if res.IsError {
		t.Fatalf("executor_key_issue failed: %v", sc)
	}
	expectOK(t, sc, "executor_key_issue")

	in := svc.LastExecutorKeyIssue
	if in.Name != "Exec-1" || in.TTLSeconds != 3600 || len(in.ProjectKeys) != 2 || in.ProjectKeys[0] != "bmb" {
		t.Fatalf("forwarded %+v; the name must reach the service verbatim (it is the identity)", in)
	}
	if svc.LastExecutorKeyIssueActor.Name == "" {
		t.Fatal("no actor forwarded: the issuer's identity comes from its token")
	}
	data := sc["data"].(map[string]any)
	if data["secret"] != "kbn_s3cr3t" || data["token_id"] != "tok-1" || data["expires_at"] != "2026-09-27T12:00:00Z" {
		t.Fatalf("data = %v", data)
	}
	if n := strings.Count(resultText(res), "kbn_s3cr3t"); n != 1 {
		t.Fatalf("the secret appears %d times in the response, want exactly once", n)
	}
}

func TestExecutorKeyIssue_RefusalCarriesNoSecret(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultExecutorKeyIssueEr = domain.Forbidden("writer is not the coordinator of project BMB", "Ask the coordinator.")

	res, sc := callTool(t, cs, "executor_key_issue", map[string]any{"name": "exec-1", "projects": []string{"BMB"}})
	if !res.IsError {
		t.Fatalf("refusal reported as success: %v", sc)
	}
	env, _ := sc["error"].(map[string]any)
	if env["code"] != "forbidden" || env["remediation"] == "" {
		t.Fatalf("error envelope = %v", sc)
	}
	if strings.Contains(resultText(res), "secret") {
		t.Fatalf("a refusal mentions a secret: %s", resultText(res))
	}
}

// Every tool an executor key can reach — or is refused by — says so in its
// own description, so an agent handed a key learns the rules from the tool
// list instead of from a refusal.
func TestExecutorKey_DescribedOnEveryAffectedTool(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	for _, name := range []string{"executor_key_issue", "task_update", "task_next", "task_claim", "task_create", "progress_set"} {
		if d := toolByName(t, cs, name).Description; !strings.Contains(strings.ToLower(d), "executor key") {
			t.Errorf("%s description never mentions the executor key", name)
		}
	}
	if d := toolByName(t, cs, "executor_key_issue").Description; !strings.Contains(d, "ONLY in this response") {
		t.Error("executor_key_issue does not warn that the secret is shown once")
	}
}

func TestExecutorKeyIssue_SchemaBounds(t *testing.T) {
	t.Parallel()
	cs, _ := roundtripServer(t, NewServer)
	published := toolByName(t, cs, "executor_key_issue")
	raw, _ := published.InputSchema.(map[string]any)
	props, _ := raw["properties"].(map[string]any)
	ttl, _ := props["ttl_seconds"].(map[string]any)
	if ttl["maximum"] != domain.ExecutorKeyMaxTTL.Seconds() {
		t.Errorf("ttl_seconds maximum = %v, want %v", ttl["maximum"], domain.ExecutorKeyMaxTTL.Seconds())
	}
	projects, _ := props["projects"].(map[string]any)
	if projects["minItems"] != float64(1) {
		t.Errorf("projects minItems = %v, want 1", projects["minItems"])
	}
	req, _ := raw["required"].([]any)
	var names []string
	for _, r := range req {
		names = append(names, r.(string))
	}
	// projects is required by the service for an issue and a participant,
	// and refused with renew, so the schema can only require the name.
	if strings.Join(names, ",") != "name" {
		t.Errorf("required = %v, want name only", names)
	}
}

// KANB-68: renew and participant_only reach the service verbatim, and their
// answers carry no secret at all — not even an empty "secret" field that
// would read like one was withheld. A participant without a key never
// expires, so it has no expires_at either.
func TestExecutorKeyIssue_RenewAndParticipantOnlyCarryNoSecret(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		label   string
		args    map[string]any
		result  *service.ExecutorKeyIssueResult
		check   func(in service.ExecutorKeyIssueInput) bool
		want    map[string]any
		without []string
	}{
		{
			label:  "renew",
			args:   map[string]any{"name": "exec-1", "renew": true, "ttl_seconds": 7200},
			result: &service.ExecutorKeyIssueResult{TokenID: "tok-1", Name: "exec-1", ProjectKeys: []string{"BMB"}, ExpiresAt: expires, Renewed: true},
			check: func(in service.ExecutorKeyIssueInput) bool {
				return in.Renew && !in.ParticipantOnly && in.TTLSeconds == 7200 && len(in.ProjectKeys) == 0
			},
			want:    map[string]any{"renewed": true, "expires_at": "2026-09-27T12:00:00Z"},
			without: []string{"secret", "participant_only"},
		},
		{
			label: "participant_only",
			args:  map[string]any{"name": "owner", "projects": []string{"BMB", "OPS"}, "participant_only": true},
			result: &service.ExecutorKeyIssueResult{TokenID: "tok-2", Name: "owner", ProjectKeys: []string{"BMB", "OPS"},
				ParticipantOnly: true, AddedProjects: []string{"OPS"}},
			check: func(in service.ExecutorKeyIssueInput) bool {
				return in.ParticipantOnly && !in.Renew && len(in.ProjectKeys) == 2
			},
			want:    map[string]any{"participant_only": true},
			without: []string{"secret", "expires_at", "renewed"},
		},
	}
	for _, tc := range cases {
		cs, svc := roundtripServer(t, NewServer)
		svc.DefaultExecutorKeyIssue = tc.result
		res, sc := callTool(t, cs, "executor_key_issue", tc.args)
		if res.IsError {
			t.Fatalf("%s: %v", tc.label, sc)
		}
		if !tc.check(svc.LastExecutorKeyIssue) {
			t.Fatalf("%s: forwarded %+v", tc.label, svc.LastExecutorKeyIssue)
		}
		data := sc["data"].(map[string]any)
		for k, v := range tc.want {
			if data[k] != v {
				t.Errorf("%s: data[%s] = %v, want %v", tc.label, k, data[k], v)
			}
		}
		for _, k := range tc.without {
			if _, ok := data[k]; ok {
				t.Errorf("%s: data carries %q: %v", tc.label, k, data)
			}
		}
	}
}
