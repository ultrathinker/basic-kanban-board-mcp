package view

import (
	"fmt"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// SampleProjects is the project list used by uipreview and tests. The keys
// match real-world PROJECT conventions: BMB is the placeholder project
// every dogfooding doc uses; KAN is a "vanilla project" so the overview
// page has more than one row.
var SampleProjects = []ProjectSummary{
	{Key: "BMB", Name: "BeeMemoryBank", Focus: "BMB-14"},
	{Key: "KAN", Name: "Kanban UI", Focus: ""},
	{Key: "OPS", Name: "Ops & Infra", Focus: "OPS-3"},
}

// SampleBoardModel returns the BMB board at a moment that looks plausible.
// It is the canonical fixture referenced from tests and the uipreview
// server.
func SampleBoardModel() (BoardModel, []domain.Column, []domain.TaskView) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	columns := sampleColumns()
	tasks := sampleTasks(now, columns)
	project := SampleProjects[0]
	focus := &FocusCard{Key: "BMB-14", Title: "Fix WAL checkpoint race", Type: domain.TypeBug}
	return BuildBoard(project, focus, columns, tasks, true), columns, tasks
}

// SampleBoardModelFor returns a BoardModel for the named project key.
func SampleBoardModelFor(key string) (BoardModel, []domain.Column, []domain.TaskView) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	columns := sampleColumns()
	tasks := sampleTasks(now, columns)
	project := ProjectSummary{Key: key, Name: key}
	return BuildBoard(project, nil, columns, tasks, true), columns, tasks
}

// sampleColumns is the board shape every board fixture shares. "Ready" is
// deliberately empty and deliberately has no WIP limit: a template that only
// renders against populated, limited columns is a template nobody has
// tested — the WIP badge crash that shipped in the first cut was exactly
// that gap.
func sampleColumns() []domain.Column {
	return []domain.Column{
		{ID: "c-backlog", Name: "Backlog", Kind: domain.KindBacklog},
		{ID: "c-ready", Name: "Ready", Kind: domain.KindActive},
		{ID: "c-doing", Name: "Doing", Kind: domain.KindActive, WIPLimit: intPtr(3)},
		{ID: "c-review", Name: "Review", Kind: domain.KindActive},
		{ID: "c-done", Name: "Done", Kind: domain.KindDone},
	}
}

// SampleEmptyBoardModel is a project that exists but has nothing in it: the
// first thing a new install shows, and the state most likely to divide by
// zero or range over nil.
func SampleEmptyBoardModel() BoardModel {
	return BuildBoard(
		ProjectSummary{Key: "NEW", Name: "Fresh project"},
		nil,
		sampleColumns(),
		nil,
		false,
	)
}

// SampleColumnlessBoardModel is the degenerate board: a project with no
// columns at all. project_upsert can create one, so the template must not
// render a bare, unexplained void.
func SampleColumnlessBoardModel() BoardModel {
	return BoardModel{Project: ProjectSummary{Key: "VOID", Name: "No columns yet"}}
}

// SampleMinimalDrawerModel is a task with nothing optional set: no body, no
// acceptance, no subtasks, no notes, no history, no links, no lease, no
// estimate, no assignee, read-only. Every `{{if}}` in the drawer takes its
// other branch here.
func SampleMinimalDrawerModel() DrawerModel {
	return DrawerModel{
		Project: ProjectSummary{Key: "BMB", Name: "BeeMemoryBank"},
		Task: DrawerTask{
			Key:       "BMB-42",
			Title:     "Bare task with nothing filled in",
			Type:      domain.TypeTask,
			Priority:  domain.PriorityNone,
			Version:   1,
			CreatedAt: "2026-09-06T09:00:00Z",
			CreatedBy: "alex",
			UpdatedAt: "2026-09-06T09:00:00Z",
			UpdatedBy: "alex",
		},
		CanEdit: false,
	}
}

// SampleEmptyActivityModel is the feed before anything has happened.
func SampleEmptyActivityModel() ActivityModel {
	return ActivityModel{Project: SampleProjects[0]}
}

// SampleEmptyOverviewModel is the overview with no projects — what a fresh
// install renders at "/".
func SampleEmptyOverviewModel() OverviewModel {
	return OverviewModel{}
}

// SampleEmptyAdminModel is /admin with no tokens and no projects.
func SampleEmptyAdminModel() AdminModel {
	return AdminModel{
		ExportURL:     "/admin/export",
		BackupURL:     "/admin/backup",
		TokensPager:   Pager{Page: 1, PageCount: 1, ParamName: "tok_page", BasePath: "/admin"},
		ProjectsPager: Pager{Page: 1, PageCount: 1, ParamName: "proj_page", BasePath: "/admin"},
	}
}

// SamplePage wraps a model in the Page envelope the layout binds to. Every
// field the chrome reads is populated: Layout (the project switcher), CSRF
// and CSRFToken (every state-changing form), CurrentUser (the topbar). A
// page rendered without those is not the page the server renders, so tests
// that skip them do not prove anything — which is how a missing Page.Layout
// and Page.CSRF reached production the first time.
func SamplePage(title, nav string, model any) Page {
	return Page{
		Title:       title,
		Nav:         nav,
		Theme:       "light",
		CSRFToken:   "csrf-fixture-token",
		CSRF:        "csrf-fixture-token",
		CurrentUser: "alex",
		BaseURL:     "http://127.0.0.1:8080",
		Layout: Layout{
			Projects:      SampleProjects,
			CurrentKey:    "BMB",
			LoggedInActor: "alex",
			IsAdmin:       true,
			Prompts:       StandardPrompts(),
		},
		Model: model,
	}
}

// SampleAnonymousPage is the chrome an unauthenticated visitor sees: no
// current user, no project switcher, no sign-out form.
func SampleAnonymousPage(title, nav string, model any) Page {
	return Page{
		Title:     title,
		Nav:       nav,
		Theme:     "light",
		CSRFToken: "csrf-fixture-token",
		CSRF:      "csrf-fixture-token",
		BaseURL:   "http://127.0.0.1:8080",
		Model:     model,
	}
}

// SampleDrawerModel builds a drawer model for BMB-14.
func SampleDrawerModel() DrawerModel {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-2 * time.Hour)
	colEnter := now.Add(-2 * time.Hour)
	leaseExp := now.Add(43 * time.Minute)
	t := domain.TaskView{
		Task: domain.Task{
			ID:              "t-14",
			Key:             "BMB-14",
			Title:           "Fix WAL checkpoint race",
			Body:            "## Problem\n\n`wal_checkpoint(TRUNCATE)` can be preempted by another writer, leaving the WAL growing.\n\n## Steps\n\n1. Add a `BEGIN IMMEDIATE` wrapper around `wal_checkpoint`.\n2. Retry up to three times when the SQLITE_BUSY is returned.\n3. Log every retry.\n\n## Acceptance\n\n- [ ] no WAL growth over 24h\n- [ ] no SQLITE_BUSY leaked to callers",
			Type:            domain.TypeBug,
			Priority:        domain.PriorityHigh,
			Tags:            []string{"sync", "storage"},
			Assignee:        ptrString("alex"),
			ClaimedBy:       ptrString("claude@rog"),
			ClaimedAt:       ptrTime(startedAt),
			ClaimExpiresAt:  ptrTime(leaseExp),
			ColumnEnteredAt: colEnter,
			StartedAt:       ptrTime(startedAt),
			Acceptance: []domain.AcceptanceItem{
				{Text: "no WAL growth over 24h", Done: true},
				{Text: "no SQLITE_BUSY leaked to callers", Done: false},
				{Text: "exponential backoff in the retry path", Done: false},
			},
			Version: 7,
		},
		ProjectKey: "BMB",
		ColumnName: "Doing",
		ColumnKind: domain.KindActive,
		BlockedBy:  []string{"BMB-9"},
		Blocks:     []string{"BMB-17"},
		SubDone:    1,
		SubTotal:   3,
		Ready:      false,
		Notes: []domain.Note{
			{ID: "n1", Author: "claude@rog", Body: "Reproduced against the bundled test database. SQLITE_BUSY returned after 1.7s on a cold WAL.", CreatedAt: now.Add(-90 * time.Minute)},
			{ID: "n2", Author: "alex", Body: "Add a unit test that hammers the checkpoint path.", CreatedAt: now.Add(-25 * time.Minute)},
		},
	}
	remain := leaseExp.Sub(now)
	t.LeaseRemain = &remain

	d := DrawerModel{
		Project: SampleProjects[0],
		Task: DrawerTask{
			Key:       t.Key,
			Title:     t.Title,
			Body:      t.Body,
			Type:      t.Type,
			Priority:  t.Priority,
			Tags:      t.Tags,
			Assignee:  ptrStringValue(t.Assignee),
			Version:   t.Version,
			Lease:     cardLease(t),
			Due:       "overdue",
			Overdue:   true,
			CreatedAt: now.Add(-72 * time.Hour).Format(time.RFC3339),
			CreatedBy: "alex",
			UpdatedAt: now.Add(-12 * time.Minute).Format(time.RFC3339),
			UpdatedBy: "claude@rog",
		},
		MarkdownHTML: markdownSafe(t.Body),
		CanEdit:      true,
	}
	if est := ptrFloat(2.0); est != nil {
		d.Task.Estimate = &EstimateView{N: *est, Unit: "h", Label: formatEstimate(*est, "h")}
	}
	for _, b := range t.BlockedBy {
		d.Blockers = append(d.Blockers, TaskRef{Key: b, Title: "Sync tombstones from remote", Type: domain.TypeBug, URL: taskURL(b)})
	}
	for _, b := range t.Blocks {
		d.Blocking = append(d.Blocking, TaskRef{Key: b, Title: "Encrypted FTS index", Type: domain.TypeFeat, URL: taskURL(b)})
	}
	d.Subtasks = []TaskRef{
		{Key: "BMB-14.1", Title: "Write failing test", Type: domain.TypeTask, URL: "#"},
		{Key: "BMB-14.2", Title: "Add retry path", Type: domain.TypeTask, URL: "#"},
		{Key: "BMB-14.3", Title: "Document the policy", Type: domain.TypeDoc, URL: "#"},
	}
	for _, n := range t.Notes {
		d.Notes = append(d.Notes, DrawerNote{
			ID: n.ID, Author: n.Author, Body: n.Body,
			CreatedAt: n.CreatedAt.Format("2006-01-02 15:04"),
			RelTime:   relTime(n.CreatedAt, now),
		})
	}
	d.History = []DrawerHistoryEntry{
		{TS: now.Add(-12 * time.Minute).Format(time.RFC3339), RelTime: relTime(now.Add(-12*time.Minute), now), Actor: "claude@rog", Verb: "started", Detail: "Doing"},
		{TS: now.Add(-25 * time.Minute).Format(time.RFC3339), RelTime: relTime(now.Add(-25*time.Minute), now), Actor: "claude@rog", Verb: "claimed", Detail: ""},
		{TS: now.Add(-72 * time.Hour).Format(time.RFC3339), RelTime: relTime(now.Add(-72*time.Hour), now), Actor: "alex", Verb: "created", Detail: ""},
	}
	for i, a := range t.Acceptance {
		d.Acceptance = append(d.Acceptance, AcceptanceView{Index: i, Text: a.Text, Done: a.Done})
	}
	return d
}

// SampleActivityModel returns a synthetic activity feed for BMB.
func SampleActivityModel() ActivityModel {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	events := []ActivityEvent{
		{ID: 1, Actor: "claude@rog", Verb: "started", Target: "BMB-14", TargetTitle: "Fix WAL checkpoint race", TS: now.Add(-2 * time.Minute)},
		{ID: 2, Actor: "claude@rog", Verb: "claimed", Target: "BMB-14", TargetTitle: "Fix WAL checkpoint race", TS: now.Add(-3 * time.Minute)},
		{ID: 3, Actor: "codex@desk", Verb: "moved", Target: "BMB-12", TargetTitle: "Squash migrations 41-44", TS: now.Add(-10 * time.Minute), RelTime: "10m ago"},
		{ID: 4, Actor: "codex@desk", Verb: "claimed", Target: "BMB-12", TargetTitle: "Squash migrations 41-44", TS: now.Add(-11 * time.Minute)},
		{ID: 5, Actor: "alex", Verb: "commented on", Target: "BMB-14", TargetTitle: "Fix WAL checkpoint race", TS: now.Add(-25 * time.Minute)},
		{ID: 6, Actor: "alex", Verb: "added acceptance item to", Target: "BMB-14", TargetTitle: "Fix WAL checkpoint race", TS: now.Add(-30 * time.Minute)},
		{ID: 7, Actor: "alex", Verb: "linked", Target: "BMB-14", TargetTitle: "Fix WAL checkpoint race", TS: now.Add(-31 * time.Minute), RelTime: "31m ago"},
		{ID: 8, Actor: "alex", Verb: "created", Target: "BMB-18", TargetTitle: "Sync loses tombstones", TS: now.Add(-50 * time.Minute)},
		{ID: 9, Actor: "alex", Verb: "moved", Target: "BMB-18", TargetTitle: "Sync loses tombstones", TS: now.Add(-51 * time.Minute)},
	}
	for i := range events {
		if events[i].RelTime == "" {
			events[i].RelTime = relTime(events[i].TS, now)
		}
	}
	// Mark the most recent 3 as new so the launch GIF shows the animation.
	if len(events) >= 3 {
		events[0].IsNew = true
		events[1].IsNew = true
		events[2].IsNew = true
	}
	return ActivityModel{Project: SampleProjects[0], Events: events}
}

// SampleOverviewModel returns the / page.
func SampleOverviewModel() OverviewModel {
	totals := OverviewTotals{Projects: 3, Active: 7, Backlog: 41, Done: 240}
	return OverviewModel{
		Total: totals,
		Projects: []OverviewProject{
			{Key: "BMB", Name: "BeeMemoryBank", Description: "Self-hosted kanban for agents.", Focus: "BMB-14", Active: 4, Backlog: 18, Done: 95, URL: boardURL("BMB")},
			{Key: "KAN", Name: "Kanban UI", Description: "Tailwind + htmx + Alpine dashboard.", Focus: "KAN-3", Active: 2, Backlog: 14, Done: 88, URL: boardURL("KAN")},
			{Key: "OPS", Name: "Ops & Infra", Description: "Deploy, backup, monitoring.", Focus: "OPS-3", Active: 1, Backlog: 9, Done: 57, URL: boardURL("OPS")},
		},
	}
}

// SampleLoginModel returns the /login page (error=empty unless set).
func SampleLoginModel(err, redirect string) LoginModel {
	return LoginModel{Error: err, Redirect: redirect}
}

// SampleFirstRunModel returns the first-run page with a bootstrap token.
func SampleFirstRunModel(token string) FirstRunModel {
	return FirstRunModel{Token: token, BaseURL: "http://127.0.0.1:8080", Projects: SampleProjects}
}

// SampleAgentSetup returns the /agent-setup page.
func SampleAgentSetup(baseURL, tokenName string) AgentSetupModel {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	cfg := func(name string) string {
		return fmt.Sprintf(`{
  "mcpServers": {
    "kanban": {
      "type": "http",
      "url": %q,
      "headers": {
        "Authorization": "Bearer ${KANBAN_TOKEN}"
      }
    }
  }
}`, baseURL+"/mcp")
	}
	return AgentSetupModel{
		BaseURL:   baseURL,
		TokenName: tokenName,
		Snippets: []AgentSnippet{
			{
				ID: "claude", Title: "Claude Code",
				Description: "Drop into ~/.claude.json or paste into the project .mcp.json.",
				Config:      cfg("claude"), Format: "json",
			},
			{
				ID: "codex", Title: "Codex CLI",
				Description: "Add to ~/.codex/config.toml under [mcp_servers.kanban].",
				Config: fmt.Sprintf(`[mcp_servers.kanban]
type = "http"
url = %q
headers = { "Authorization" = "Bearer ${KANBAN_TOKEN}" }`, baseURL+"/mcp"),
				Format: "toml",
			},
			{
				ID: "cursor", Title: "Cursor",
				Description: "Cursor → Settings → MCP → Add new global MCP server.",
				Config:      cfg("cursor"),
				Format:      "json",
			},
			{
				ID: "generic", Title: "Generic (streamable-HTTP)",
				Description: "Any MCP client that speaks streamable-HTTP over bearer auth.",
				Config:      fmt.Sprintf("POST %s/mcp\nAuthorization: Bearer ${KANBAN_TOKEN}\nContent-Type: application/json", baseURL),
				Format:      "text",
			},
		},
	}
}

// SampleAdminModel returns the /admin page.
func SampleAdminModel() AdminModel {
	return AdminModel{
		ExportURL:     "/admin/export",
		BackupURL:     "/admin/backup",
		TokensPager:   Pager{Page: 1, PageCount: 1, Total: 4, ParamName: "tok_page", BasePath: "/admin"},
		ProjectsPager: Pager{Page: 1, PageCount: 1, Total: 3, ParamName: "proj_page", BasePath: "/admin"},
		Tokens: []AdminToken{
			{Name: "admin", Scope: "admin", ProjectKeys: "*", CreatedAt: "2026-09-01 09:00", LastUsed: "2026-09-06 11:55", Active: true},
			{Name: "claude@rog", Scope: "write", ProjectKeys: "BMB", CreatedAt: "2026-09-03 22:14", LastUsed: "2026-09-06 11:30", Active: true},
			{Name: "codex@desk", Scope: "read", ProjectKeys: "BMB,KAN", CreatedAt: "2026-09-04 18:00", LastUsed: "2026-09-06 09:21", Active: true},
			{Name: "oldci", Scope: "write", ProjectKeys: "*", CreatedAt: "2026-08-12 12:30", LastUsed: "2026-08-29 16:11", Active: false},
		},
		Projects: []AdminProject{
			{Key: "BMB", Name: "BeeMemoryBank", Columns: 4, ActiveTasks: 4, Version: 14},
			{Key: "KAN", Name: "Kanban UI", Columns: 4, ActiveTasks: 2, Version: 7},
			{Key: "OPS", Name: "Ops & Infra", Columns: 4, ActiveTasks: 1, Version: 3},
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func intPtr(i int) *int { return &i }

func ptrString(s string) *string { return &s }

func ptrTime(t time.Time) *time.Time { return &t }

func ptrFloat(f float64) *float64 { return &f }

func ptrStringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func cardLease(t domain.TaskView) *LeaseView {
	if t.ClaimedBy == nil {
		return nil
	}
	var remain time.Duration
	live := false
	if t.LeaseRemain != nil {
		remain = *t.LeaseRemain
		live = remain > 0
	} else if t.ClaimExpiresAt != nil {
		remain = time.Until(*t.ClaimExpiresAt)
		live = remain > 0
	}
	lv := &LeaseView{Actor: *t.ClaimedBy}
	if live {
		lv.State = "live"
		lv.Remain = formatDuration(remain)
	} else {
		lv.State = "expired"
		lv.Remain = "expired"
	}
	return lv
}

// sampleTasks builds a small fixture spread across columns. Names, priorities
// and tags mirror the language the launch GIF shows in the brief.
func sampleTasks(now time.Time, columns []domain.Column) []domain.TaskView {
	col := func(name string) string {
		for _, c := range columns {
			if c.Name == name {
				return c.ID
			}
		}
		return columns[0].ID
	}
	enter := func(d time.Duration) time.Time { return now.Add(-d) }
	mk := func(
		key, title string, t domain.Type, p domain.Priority,
		column string, est *float64, tags []string, assignee *string,
		claim *string, claimAge, inCol time.Duration, blocked []string, subDone, subTotal int, ver int,
	) domain.TaskView {
		tv := domain.TaskView{
			ProjectKey: "BMB",
			BlockedBy:  blocked,
			SubDone:    subDone,
			SubTotal:   subTotal,
		}
		tv.ID = "task-" + key
		tv.Key = key
		tv.Title = title
		tv.Type = t
		tv.Priority = p
		tv.Estimate = est
		tv.Tags = tags
		tv.Assignee = assignee
		tv.ColumnEnteredAt = enter(inCol)
		tv.UpdatedAt = now.Add(-1 * time.Hour)
		tv.Version = ver
		for _, c := range columns {
			if c.ID == col(column) {
				tv.ColumnName = c.Name
				tv.ColumnKind = c.Kind
				tv.ColumnID = c.ID
				break
			}
		}
		if claim != nil {
			tv.ClaimedBy = claim
			ca := enter(claimAge)
			tv.ClaimedAt = &ca
			ce := enter(claimAge).Add(time.Hour)
			tv.ClaimExpiresAt = &ce
			if tv.ColumnKind == domain.KindActive {
				r := ce.Sub(now)
				tv.LeaseRemain = &r
			} else {
				// Negative duration to indicate "expired".
				r := -time.Hour
				tv.LeaseRemain = &r
			}
		}
		return tv
	}
	out := []domain.TaskView{
		mk("BMB-14", "Fix WAL checkpoint race", domain.TypeBug, domain.PriorityHigh, "Doing", ptrFloat(2),
			[]string{"sync"}, ptrString("alex"), ptrString("claude@rog"), 17*time.Minute, 2*time.Hour,
			[]string{"BMB-9"}, 1, 3, 7),
		mk("BMB-17", "Encrypted FTS index", domain.TypeFeat, domain.PriorityMedium, "Doing", ptrFloat(8),
			[]string{"search"}, nil, nil, 0, 3*24*time.Hour,
			[]string{"BMB-14", "BMB-9"}, 0, 2, 3),
		mk("BMB-21", "Backfill tag counts", domain.TypeChore, domain.PriorityLow, "Doing", ptrFloat(1),
			nil, ptrString("codex@desk"), nil, 0, 6*time.Hour,
			nil, 0, 0, 1),
		mk("BMB-12", "Squash migrations 41-44", domain.TypeTask, domain.PriorityMedium, "Review", ptrFloat(1),
			nil, ptrString("alex"), nil, 0, 24*time.Hour,
			nil, 0, 0, 2),
		mk("BMB-9", "Sync tombstones from remote", domain.TypeBug, domain.PriorityHigh, "Review", ptrFloat(4),
			[]string{"sync"}, nil, nil, 0, 36*time.Hour,
			nil, 2, 4, 5),
		mk("BMB-18", "Sync loses tombstones", domain.TypeBug, domain.PriorityCritical, "Backlog", nil,
			[]string{"sync"}, nil, nil, 0, 5*time.Minute,
			nil, 0, 0, 1),
		mk("BMB-19", "Expose /agent-setup", domain.TypeDoc, domain.PriorityLow, "Backlog", ptrFloat(1),
			nil, nil, nil, 0, 6*24*time.Hour,
			nil, 0, 0, 1),
		mk("BMB-20", "CSP-tighten app.js", domain.TypeChore, domain.PriorityMedium, "Backlog", ptrFloat(2),
			nil, nil, nil, 0, 12*time.Hour,
			nil, 0, 0, 1),
		mk("BMB-1", "Initial schema", domain.TypeChore, domain.PriorityNone, "Done", nil,
			nil, nil, nil, 0, 40*24*time.Hour,
			nil, 0, 0, 12),
		mk("BMB-2", "Auth middleware", domain.TypeFeat, domain.PriorityHigh, "Done", nil,
			nil, nil, nil, 0, 30*24*time.Hour,
			nil, 0, 0, 9),
	}
	// Due dates: one comfortably ahead, one already past. Overdue is one of
	// the three states PLAN §9 says must not be missed, and it is rendered
	// as an inverted block rather than a colour — so a fixture has to
	// contain one or nothing ever exercises that branch.
	for i := range out {
		switch out[i].Key {
		case "BMB-18": // critical AND overdue AND blocked: the loudest card
			out[i].DueAt = ptrTime(time.Now().UTC().Add(-26 * time.Hour))
			out[i].BlockedBy = []string{"BMB-9"}
		case "BMB-17":
			out[i].DueAt = ptrTime(time.Now().UTC().Add(72 * time.Hour))
		}
	}
	return out
}
