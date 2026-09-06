package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// Architecture review finding #21: "included but bounded" had no representation.
//
// The defect was that one set of booleans did two jobs. `include` selected
// which fields to return, and the service read the same flags as permission to
// return them WHOLE, truncating only when they were absent — while the MCP
// converter dropped any field whose flag was absent. So "bounded body" was
// unreachable: the only two answers were the entire 64 KiB body or nothing,
// and since MCP clients materialise schema defaults, every real client asked
// for the entire one.
//
// The fix separates the axes. `include` still selects fields; the new `detail`
// chooses how much of each; and the service states the outcome in
// NextResult.Projection, which is what the MCP layer now renders from. These
// tests pin that contract and re-measure the wire cost the earlier baseline
// recorded, so the two numbers can be read side by side.
// ---------------------------------------------------------------------------

// realisticCandidates builds n engineering cards of the shape the original
// baseline used: a 2 KiB markdown body and a full 50-item acceptance list.
func realisticCandidates(n int) []domain.TaskView {
	body := strings.Repeat("## Implementation notes\n- Ensure atomic lease acquisition.\n- Validate dependency wait graph.\n", 20)
	if len(body) > 2048 {
		body = body[:2048]
	}
	acceptance := make([]domain.AcceptanceItem, domain.MaxAcceptance)
	for i := range acceptance {
		acceptance[i] = domain.AcceptanceItem{
			Text: fmt.Sprintf("AC-%02d: Given an authenticated agent with read scope, verify response projection behaves.", i+1),
		}
	}
	out := make([]domain.TaskView, n)
	for i := range out {
		tv := fixedTask(fmt.Sprintf("BMB-%d", i+1))
		tv.Title = fmt.Sprintf("Candidate task %d for sprint delivery", i+1)
		tv.Body = body
		tv.Acceptance = acceptance
		out[i] = tv
	}
	return out
}

// shapedNextResult runs the REAL projection rule over the fixture, so what the
// test measures is what the service would actually have produced. Copying the
// clipping into the test would measure the copy, and the copy would drift.
func shapedNextResult(detail service.NextDetail, n int) *service.NextResult {
	proj := service.NewNextProjection(
		service.Includes{service.IncludeBody, service.IncludeAcceptance}, detail)
	tasks := realisticCandidates(n)
	for i := range tasks {
		proj.Apply(&tasks[i])
	}
	return &service.NextResult{Tasks: tasks, Projection: proj}
}

// TestTaskNext_PinsMCPForwarding verifies the exact input fields that MCP
// forwards to service.Service.TaskNext when defaults or explicit parameters
// are supplied.
func TestTaskNext_PinsMCPForwarding(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	// Case 1: Default call with empty arguments.
	svc.DefaultTaskNext = shapedNextResult(service.NextDetailSummary, 1)
	_, sc := callTool(t, cs, "task_next", map[string]any{})
	expectOK(t, sc, "task_next")

	if got := svc.LastTaskNext.Action; got != service.NextPeek {
		t.Errorf("default action = %q, want %q", got, service.NextPeek)
	}
	if got := svc.LastTaskNext.Limit; got != 3 {
		t.Errorf("default limit = %d, want 3", got)
	}
	if got := svc.LastTaskNext.ProjectKey; got != "" {
		t.Errorf("default project = %q, want empty (all accessible projects)", got)
	}
	// MCP forwards IncludeBody and IncludeAcceptance by default.
	wantDefaultIncludes := service.Includes{service.IncludeBody, service.IncludeAcceptance}
	if len(svc.LastTaskNext.Include) != len(wantDefaultIncludes) ||
		!svc.LastTaskNext.Include.Has(service.IncludeBody) ||
		!svc.LastTaskNext.Include.Has(service.IncludeAcceptance) {
		t.Errorf("default include = %v, want %v", svc.LastTaskNext.Include, wantDefaultIncludes)
	}
	// The client materialises the schema default, so `detail` arrives
	// explicitly even though the call omitted it — the same mechanism that
	// made removing the `include` default a non-fix. Either way the answer is
	// summary: the service treats "" and "summary" identically, so a client
	// that does NOT materialise defaults gets the same bounded response.
	if got := svc.LastTaskNext.Detail; got != service.NextDetailSummary && got != "" {
		t.Errorf("default detail = %q, want summary (or empty, which means summary)", got)
	}

	// Case 2: Explicit call with non-default options.
	svc.DefaultTaskNext = shapedNextResult(service.NextDetailFull, 1)
	_, sc2 := callTool(t, cs, "task_next", map[string]any{
		"action":  "claim",
		"project": "BMB",
		"limit":   10,
		"include": []any{"notes", "links"},
		"detail":  "full",
	})
	expectOK(t, sc2, "task_next")

	if got := svc.LastTaskNext.Action; got != service.NextClaim {
		t.Errorf("explicit action = %q, want %q", got, service.NextClaim)
	}
	if got := svc.LastTaskNext.Limit; got != 10 {
		t.Errorf("explicit limit = %d, want 10", got)
	}
	if got := svc.LastTaskNext.ProjectKey; got != "BMB" {
		t.Errorf("explicit project = %q, want BMB", got)
	}
	if got := svc.LastTaskNext.Detail; got != service.NextDetailFull {
		t.Errorf("explicit detail = %q, want %q", got, service.NextDetailFull)
	}
	wantExplicitIncludes := service.Includes{service.IncludeNotes, service.IncludeLinks}
	if len(svc.LastTaskNext.Include) != len(wantExplicitIncludes) ||
		!svc.LastTaskNext.Include.Has(service.IncludeNotes) ||
		!svc.LastTaskNext.Include.Has(service.IncludeLinks) {
		t.Errorf("explicit include = %v, want %v", svc.LastTaskNext.Include, wantExplicitIncludes)
	}
}

// TestTaskNext_RendersFromServiceProjection is the regression test for the
// defect itself: the MCP layer must render what the service says it returned,
// not what the request implied. Under the old code a bounded body was
// impossible — the converter dropped `body` whenever the include flag that
// would have meant "full" was absent.
func TestTaskNext_RendersFromServiceProjection(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	svc.DefaultTaskNext = shapedNextResult(service.NextDetailSummary, 1)
	_, sc := callTool(t, cs, "task_next", map[string]any{"project": "BMB"})
	expectOK(t, sc, "task_next")

	first := sc["data"].(map[string]any)["tasks"].([]any)[0].(map[string]any)

	body, ok := first["body"].(string)
	if !ok {
		t.Fatalf("bounded body was dropped entirely; got %v", first["body"])
	}
	if len(body) > service.NextSummaryBodyBytes+32 {
		t.Errorf("summary body = %d bytes, want ~%d", len(body), service.NextSummaryBodyBytes)
	}
	if !strings.Contains(body, "chars") {
		t.Errorf("clipped body does not say how much is missing: %q", body)
	}

	acc := first["acceptance"].([]any)
	if len(acc) != service.NextSummaryAcceptanceItems {
		t.Errorf("summary acceptance = %d items, want %d", len(acc), service.NextSummaryAcceptanceItems)
	}
	// A clipped list must say what it was clipped from.
	if got := first["acceptance_total"]; got != float64(domain.MaxAcceptance) {
		t.Errorf("acceptance_total = %v, want %d", got, domain.MaxAcceptance)
	}

	meta := sc["meta"].(map[string]any)["projection"].(map[string]any)
	if meta["body"] != string(service.DetailBounded) ||
		meta["body_limit_bytes"] != float64(service.NextSummaryBodyBytes) ||
		meta["acceptance"] != string(service.DetailBounded) ||
		meta["acceptance_limit"] != float64(service.NextSummaryAcceptanceItems) {
		t.Errorf("meta.projection does not describe the bounds applied: %v", meta)
	}
}

// TestTaskNext_FullDetailWidensButStaysBounded pins the other tier. `full` is
// the PLAN §6.2 ceiling, not "everything": task_next never hands back a whole
// 64 KiB body, because that is what task_get is for.
func TestTaskNext_FullDetailWidensButStaysBounded(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	svc.DefaultTaskNext = shapedNextResult(service.NextDetailFull, 1)
	_, sc := callTool(t, cs, "task_next", map[string]any{"project": "BMB", "detail": "full"})
	expectOK(t, sc, "task_next")

	first := sc["data"].(map[string]any)["tasks"].([]any)[0].(map[string]any)
	if got := len(first["acceptance"].([]any)); got != domain.NextAcceptanceItems {
		t.Errorf("full acceptance = %d items, want %d", got, domain.NextAcceptanceItems)
	}
	if got := len(first["body"].(string)); got > domain.NextBodyTruncate+32 {
		t.Errorf("full body = %d bytes, want at most ~%d", got, domain.NextBodyTruncate)
	}

	meta := sc["meta"].(map[string]any)["projection"].(map[string]any)
	if meta["body_limit_bytes"] != float64(domain.NextBodyTruncate) {
		t.Errorf("meta.projection.body_limit_bytes = %v, want %d", meta["body_limit_bytes"], domain.NextBodyTruncate)
	}
}

// TestTaskNext_UnselectedFieldsAreAbsent keeps the other half of the contract
// honest: separating detail from selection must not turn selection off.
func TestTaskNext_UnselectedFieldsAreAbsent(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	proj := service.NewNextProjection(service.Includes{service.IncludeAcceptance}, service.NextDetailSummary)
	tasks := realisticCandidates(1)
	proj.Apply(&tasks[0])
	svc.DefaultTaskNext = &service.NextResult{Tasks: tasks, Projection: proj}

	_, sc := callTool(t, cs, "task_next", map[string]any{"project": "BMB", "include": []any{"acceptance"}})
	expectOK(t, sc, "task_next")

	first := sc["data"].(map[string]any)["tasks"].([]any)[0].(map[string]any)
	if _, present := first["body"]; present {
		t.Errorf("body present although it was not selected: %v", first["body"])
	}
	if _, present := first["acceptance"]; !present {
		t.Errorf("acceptance missing although it was selected")
	}
}

// TestTaskNext_DefaultResponseSize measures the default answer on the wire and
// prints it beside the baseline the previous agent recorded, so the two are
// read together rather than one replacing the other.
//
// Baseline (architecture review finding #21, same fixture, before the fix):
//
//	default, 3 candidates .............. 10,297 bytes  ~2,575 tokens
//	10 candidates ...................... 33,890 bytes  ~8,473 tokens
//	include:[body,acceptance] unbounded,
//	  10 candidates .................... 79,090 bytes ~19,773 tokens
//	the entire 30-task board, compact ... 4,258 bytes  ~1,064 tokens
//
// The gate below is the one that matters: choosing the next task must not cost
// more than reading the whole board, which is the failure the review named.
func TestTaskNext_DefaultResponseSize(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	measure := func(detail service.NextDetail, n int, args map[string]any) int {
		svc.DefaultTaskNext = shapedNextResult(detail, n)
		res, sc := callTool(t, cs, "task_next", args)
		expectOK(t, sc, "task_next")
		return len(res.Content[0].(*gomcp.TextContent).Text)
	}

	base := map[string]any{"project": "BMB", "action": "peek"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	bytes3 := measure(service.NextDetailSummary, 3, with(map[string]any{"limit": 3}))
	bytes10 := measure(service.NextDetailSummary, domain.MaxNextLimit, with(map[string]any{"limit": domain.MaxNextLimit}))
	full10 := measure(service.NextDetailFull, domain.MaxNextLimit,
		with(map[string]any{"limit": domain.MaxNextLimit, "detail": "full"}))

	t.Logf("=== task_next wire size after finding #21 ===")
	t.Logf("default (summary), 3 candidates:  %6d bytes  ~%5d tokens   (baseline 10,297 / ~2,575)",
		bytes3, (bytes3+3)/4)
	t.Logf("default (summary), 10 candidates: %6d bytes  ~%5d tokens   (baseline 33,890 / ~8,473)",
		bytes10, (bytes10+3)/4)
	t.Logf("detail:full, 10 candidates:       %6d bytes  ~%5d tokens   (baseline 79,090 / ~19,773)",
		full10, (full10+3)/4)
	t.Logf("for reference, the whole 30-task board, compact: 4,258 bytes / ~1,064 tokens")

	// The product claim the review broke: asking what to do next must cost
	// less than reading the entire board. The compact 30-task board measures
	// ~1,064 tokens (compact_test.go owns that gate); the default task_next
	// answer has to come in under it.
	const boardTokens = 1064
	if got := (bytes3 + 3) / 4; got >= boardTokens {
		t.Errorf("default task_next costs ~%d tokens, at or above the ~%d tokens of reading the ENTIRE board.\n"+
			"That is the exact failure architecture review finding #21 named. Check what grew in the default projection.",
			got, boardTokens)
	}
	// A regression guard on the widest summary call, not a target: the honest
	// measurement is ~2,930 tokens and the ceiling sits above it with enough
	// headroom for wording, not enough to hide a field returning to the
	// default projection.
	if got := (bytes10 + 3) / 4; got > 3200 {
		t.Errorf("summary task_next at %d candidates costs ~%d tokens, over the 3200 ceiling", domain.MaxNextLimit, got)
	}
}

// TestTaskNext_StructuredContentParity verifies that the JSON text in
// Content[0] and StructuredContent represent the identical parsed data structure.
func TestTaskNext_StructuredContentParity(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)

	tv := fixedTask("BMB-1")
	tv.Body = "Test body"
	tv.Acceptance = []domain.AcceptanceItem{{Text: "Done criterion", Done: true}}
	svc.DefaultTaskNext = &service.NextResult{
		Tasks: []domain.TaskView{tv},
		Projection: service.NewNextProjection(
			service.Includes{service.IncludeBody, service.IncludeAcceptance}, service.NextDetailSummary),
	}

	res, sc := callTool(t, cs, "task_next", map[string]any{"project": "BMB"})
	expectOK(t, sc, "task_next")

	var parsedFromText taskNextOutput
	if err := json.Unmarshal([]byte(res.Content[0].(*gomcp.TextContent).Text), &parsedFromText); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if len(parsedFromText.Data.Tasks) != 1 {
		t.Fatalf("tasks count = %d, want 1", len(parsedFromText.Data.Tasks))
	}
	if *parsedFromText.Data.Tasks[0].Body != "Test body" {
		t.Errorf("parsed body = %v, want Test body", *parsedFromText.Data.Tasks[0].Body)
	}
}
