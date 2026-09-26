package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// KANB-61: the attention line, include:["progress"], task_get's epic summary.
// The service decides who is idle and by how much (internal/service
// activity_test.go); these tests pin how the decision reaches the wire.
// ---------------------------------------------------------------------------

func attentionBoard() *service.Board {
	pct := 40
	return &service.Board{Projects: []service.BoardProject{{
		Key: "BMB", Name: "BeeMemoryBank", Version: 3, EstimateUnit: "h",
		Columns: []service.BoardColumn{
			mkColumn("Doing", domain.KindActive, 2,
				mkTask("BMB-12", withEnteredAt(fixedNow.Add(-26*time.Hour))),
				mkTask("BMB-15", withEnteredAt(fixedNow.Add(-5*time.Hour))),
			),
			mkColumn("Backlog", domain.KindBacklog, 1, mkTask("BMB-20")),
		},
		Attention: &service.Attention{Count: 7, Sample: []service.IdleTask{
			{Key: "BMB-12", Idle: 26 * time.Hour},
			{Key: "BMB-15", Idle: 5 * time.Hour},
			{Key: "BMB-9", Idle: 3 * time.Hour},
			{Key: "BMB-3", Idle: 2*time.Hour + 30*time.Minute},
			{Key: "BMB-4", Idle: 61 * time.Minute},
		}},
		Activity: map[string]service.TaskActivity{
			"BMB-12": {AcceptanceDone: 2, AcceptanceTotal: 5, Percent: &pct, LastActivityAt: fixedNow.Add(-26 * time.Hour), Idle: 26 * time.Hour},
			"BMB-15": {LastActivityAt: fixedNow.Add(-5 * time.Hour), Idle: 5 * time.Hour},
		},
	}}}
}

func TestRender_AttentionLineUnderTheHeader(t *testing.T) {
	b := attentionBoard()
	b.Projects[0].Description = "rules"
	lines := strings.Split(Render(b, fixedNow), "\n")
	if !strings.HasPrefix(lines[1], "# BMB ") {
		t.Fatalf("line 2 is not the project header: %q", lines[1])
	}
	want := "attention 7: BMB-12 idle 1d, BMB-15 idle 5h, BMB-9 idle 3h, BMB-3 idle 2h, BMB-4 idle 1h, and 2 more"
	if lines[2] != want {
		t.Errorf("line 3 = %q\nwant      %q", lines[2], want)
	}
	if lines[3] != "description:" {
		t.Errorf("the description must follow the attention line, line 4 = %q", lines[3])
	}

	b.Projects[0].Attention = &service.Attention{Count: 1, Sample: []service.IdleTask{{Key: "BMB-12", Idle: 90 * time.Minute}}}
	if got := strings.Split(Render(b, fixedNow), "\n")[2]; got != "attention 1: BMB-12 idle 1h" {
		t.Errorf("a complete sample must not say \"and 0 more\": %q", got)
	}

	b.Projects[0].Attention = nil
	if out := Render(b, fixedNow); strings.Contains(out, "attention") {
		t.Errorf("no idle card, yet the rendering mentions attention:\n%s", out)
	}
}

func TestRender_ProgressSegments(t *testing.T) {
	out := Render(attentionBoard(), fixedNow)
	for _, want := range []string{
		"- BMB-12 [task] Sample task BMB-12 · v1 · age 1d · acc 2/5 · pct 40 · idle 1d\n",
		// No criteria and nobody assessed: acc and pct are omitted, idle is not.
		"- BMB-15 [task] Sample task BMB-15 · v1 · age 5h · idle 5h\n",
		// No activity entry (a backlog card): the line is unchanged.
		"- BMB-20 [task] Sample task BMB-20 · v1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing line %q in:\n%s", want, out)
		}
	}
}

// The parser is the executable form of the grammar: it must read back
// everything the renderer writes, byte for byte.
func TestCompactRoundTrip_AttentionAndProgress(t *testing.T) {
	want := attentionBoard()
	rendered := Render(want, fixedNow)
	parsed, err := ParseCompactAt(rendered, fixedNow)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, rendered)
	}
	a := parsed.Projects[0].Attention
	if a == nil || a.Count != 7 || len(a.Sample) != 5 || a.Sample[0] != (service.IdleTask{Key: "BMB-12", Idle: 24 * time.Hour}) {
		t.Fatalf("parsed attention = %+v", a)
	}
	act := parsed.Projects[0].Activity["BMB-12"]
	if act.AcceptanceDone != 2 || act.AcceptanceTotal != 5 || act.Percent == nil || *act.Percent != 40 || act.Idle != 24*time.Hour {
		t.Errorf("parsed activity for BMB-12 = %+v", act)
	}
	// The grammar does not carry a column's kind (ParseCompact's doc), and
	// `age` is written for active columns only; restore it so the second
	// render compares the progress segments rather than that known loss.
	parsed.Projects[0].Columns[0].Kind = domain.KindActive
	if again := Render(parsed, fixedNow); again != rendered {
		t.Errorf("render(parse(x)) != x\nfirst:\n%s\nsecond:\n%s", rendered, again)
	}
}

func TestParseCompact_AttentionErrors(t *testing.T) {
	head := versionHeader() + "\n# BMB B · focus none · Doing 1 · Done 0 (hidden) · v1\n"
	cases := map[string]string{
		"count disagrees with the items": head + "attention 3: BMB-1 idle 2h\n",
		"and 0 more":                     head + "attention 1: BMB-1 idle 2h, and 0 more\n",
		"malformed item":                 head + "attention 1: BMB-1 2h\n",
		"not under the header":           head + "## Doing\nattention 1: BMB-1 idle 2h\n",
		"idle missing after acc":         head + "## Doing\n- BMB-1 [task] T · v1 · acc 1/2\n",
		"acc with a zero total":          head + "## Doing\n- BMB-1 [task] T · v1 · acc 0/0 · idle 1h\n",
	}
	for name, in := range cases {
		if _, err := ParseCompact(in); err == nil {
			t.Errorf("%s: parsed without error:\n%s", name, in)
		}
	}
}

func TestBoardGet_AttentionAndProgressOnTheWire(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	svc.DefaultBoardGet = attentionBoard()

	text := resultText(callToolRaw(t, cs, "board_get", map[string]any{"project": "BMB", "include": []any{"progress"}}))
	if !strings.Contains(text, "\nattention 7: BMB-12 idle 1d,") || !strings.Contains(text, "acc 2/5 · pct 40 · idle 1d") {
		t.Errorf("compact board_get lost the attention line or the progress segments:\n%s", text)
	}
	if !svc.LastBoardGet.Include.Has(service.IncludeProgress) {
		t.Errorf("include progress did not reach the service: %v", svc.LastBoardGet.Include)
	}

	_, env := callTool(t, cs, "board_get", map[string]any{"project": "BMB", "format": "json", "include": []any{"progress"}})
	var got struct {
		Data struct {
			Projects []struct {
				Attention *struct {
					Count  int `json:"count"`
					Sample []struct {
						Key         string `json:"key"`
						IdleSeconds int    `json:"idle_seconds"`
					} `json:"sample"`
				} `json:"attention"`
				Columns []struct {
					Tasks []struct {
						Key      string         `json:"key"`
						Progress map[string]any `json:"progress"`
					} `json:"tasks"`
				} `json:"columns"`
			} `json:"projects"`
		} `json:"data"`
	}
	raw, _ := json.Marshal(env)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	p := got.Data.Projects[0]
	if p.Attention == nil || p.Attention.Count != 7 || len(p.Attention.Sample) != 5 ||
		p.Attention.Sample[0].Key != "BMB-12" || p.Attention.Sample[0].IdleSeconds != 26*3600 {
		t.Errorf("json attention = %+v", p.Attention)
	}
	prog := p.Columns[0].Tasks[0].Progress
	if prog["acceptance_done"] != 2.0 || prog["acceptance_total"] != 5.0 || prog["percent"] != 40.0 || prog["idle_seconds"] != 26*3600.0 {
		t.Errorf("json progress for BMB-12 = %v", prog)
	}
	if _, has := p.Columns[0].Tasks[1].Progress["percent"]; has {
		t.Errorf("an unassessed card must omit percent, not report 0: %v", p.Columns[0].Tasks[1].Progress)
	}
	if p.Columns[1].Tasks[0].Progress != nil {
		t.Errorf("a card without an activity entry carries progress: %v", p.Columns[1].Tasks[0].Progress)
	}

	b := attentionBoard()
	b.Projects[0].Attention, b.Projects[0].Activity = nil, nil
	svc.DefaultBoardGet = b
	res := resultText(callToolRaw(t, cs, "board_get", map[string]any{"project": "BMB", "format": "json"}))
	if strings.Contains(res, `"attention"`) || strings.Contains(res, `"progress"`) {
		t.Errorf("a board with nothing idle must not grow: %s", res)
	}
}

func TestTaskGet_SubtasksOnTheWire(t *testing.T) {
	t.Parallel()
	cs, svc := roundtripServer(t, NewServer)
	parent, leaf := fixedTask("BMB-1"), fixedTask("BMB-2")
	parent.SubDone, parent.SubTotal = 4, 7
	svc.DefaultTaskGet = &service.TaskGetResult{
		Tasks:    []domain.TaskView{parent, leaf},
		Subtasks: map[string]service.SubtaskKinds{"BMB-1": {Backlog: 2, Active: 1, Waiting: 0, Done: 4}},
	}
	text := resultText(callToolRaw(t, cs, "task_get", map[string]any{"keys": []any{"BMB-1", "BMB-2"}}))
	var got struct {
		Data struct {
			Items []struct {
				Task struct {
					Subtasks map[string]int `json:"subtasks"`
				} `json:"task"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	want := map[string]int{"backlog": 2, "active": 1, "waiting": 0, "done": 4}
	sub := got.Data.Items[0].Task.Subtasks
	if len(sub) != len(want) {
		t.Fatalf("parent subtasks = %v, want %v", sub, want)
	}
	for k, v := range want {
		if n, ok := sub[k]; !ok || n != v {
			t.Errorf("parent subtasks[%s] = %d (present %v), want %d", k, n, ok, v)
		}
	}
	if got.Data.Items[1].Task.Subtasks != nil {
		t.Errorf("a leaf task carries subtasks: %v", got.Data.Items[1].Task.Subtasks)
	}
}

// "progress" is a board_get include; task_get has no board to annotate.
func TestIncludeProgress_OnlyOnBoardGet(t *testing.T) {
	enumOf := func(schema any) []string {
		raw, _ := json.Marshal(schema)
		var s struct {
			Properties struct {
				Include struct {
					Items struct {
						Enum []string `json:"enum"`
					} `json:"items"`
				} `json:"include"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("decode schema: %v", err)
		}
		return s.Properties.Include.Items.Enum
	}
	has := func(vals []string, v string) bool {
		for _, x := range vals {
			if x == v {
				return true
			}
		}
		return false
	}
	if !has(enumOf(boardGetTool().InputSchema), "progress") {
		t.Error("board_get's include enum lacks progress")
	}
	if has(enumOf(taskGetTool().InputSchema), "progress") {
		t.Error("task_get publishes include progress")
	}
}
