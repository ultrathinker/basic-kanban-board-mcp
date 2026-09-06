// Package view holds the view-models and template helpers used by the web
// templates. The package is intentionally independent of internal/store and
// internal/service: today it is fed by fixtures (so the UI can be developed
// and reviewed without a database); later it is fed by service calls.
//
// Anything that ends up in a template lives here. Templates bind to
// `Page.Model`, which is an interface — concrete models (`BoardModel`,
// `DrawerModel`, ...) are plain values so html/template can render them
// without reflection surprises.
package view

import (
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
)

// Page is the value the layout template renders against. Every page sets
// `Title`, `Nav` (which topbar item is active), `Theme` and one of the
// concrete Model values.
type Page struct {
	Title       string
	Subtitle    string
	Nav         string // "board" | "activity" | "overview" | "admin" | "login" | "agent-setup"
	Theme       string // "light" | "dark"
	CSRFToken   string
	CurrentUser string // actor = token name, never accepted as a parameter
	BaseURL     string
	Flash       string // optional one-line message rendered above the body

	// Model is the page-specific payload. Concrete types live in this package.
	Model any
}

// Layout is the chrome shared by every page (topbar, footer, toast region).
type Layout struct {
	Projects      []ProjectSummary
	CurrentKey    string
	LoggedInActor string
}

// ProjectSummary is the dropdown entry in the project switcher.
type ProjectSummary struct {
	Key   string
	Name  string
	Focus string
}

// BoardModel is what the board page renders.
type BoardModel struct {
	Project  ProjectSummary
	Focus    *FocusCard      // nil if no focus is set
	Columns  []ColumnView
	DoneTotal int
	DoneShown int
	HideDone  bool
}

// FocusCard is the "Now: KEY — title" banner.
type FocusCard struct {
	Key   string
	Title string
	Type  domain.Type
}

// ColumnView is one column on the board. Tasks are sorted by Rank ASC.
type ColumnView struct {
	Name   string
	Kind   domain.Kind
	WIP    *int // nil when no limit
	Count  int
	Tasks  []TaskCard
	Hidden bool // done column hidden when board says so
}

// TaskCard is the card rendered for one task on the board.
type TaskCard struct {
	Key         string
	Title       string
	Type        domain.Type
	Priority    domain.Priority
	PriorityCSS string // prio-none | prio-low | ...
	Estimate    *EstimateView
	Tags        []string
	Assignee    string // empty when unassigned
	Lease       *LeaseView
	BlockedBy   []string // open blockers (sorted, keys)
	Blocked     bool
	SubDone     int
	SubTotal    int
	Version     int
	HasBody     bool // whether to show "edit" hint; full body lives in drawer
	Age         string
	Updated     time.Time
	URL         string // /t/{KEY}
}

// EstimateView is the pill: "2h", "30m".
type EstimateView struct {
	N     float64
	Unit  string
	Label string
}

// LeaseView is the badge: remaining time + actor + state.
type LeaseView struct {
	Actor    string
	State    string // "live" | "expired" | "mine"
	Remain   string // "43m", "2h", "expired"
	IsMine   bool
}

// DrawerModel is the /t/{KEY} page (rendered inside the topbar layout).
type DrawerModel struct {
	Project   ProjectSummary
	Task      DrawerTask
	Blockers  []TaskRef
	Blocking  []TaskRef
	Subtasks  []TaskRef
	Notes     []DrawerNote
	History   []DrawerHistoryEntry
	Acceptance []AcceptanceView
	MarkdownHTML template.HTML
	CanEdit   bool
	IsAdmin   bool
}

// DrawerTask is the full task payload the drawer binds to.
type DrawerTask struct {
	Key       string
	Title     string
	Body      string
	Type      domain.Type
	Priority  domain.Priority
	Estimate  *EstimateView
	Tags      []string
	Assignee  string
	Version   int
	Lease     *LeaseView
	Due       string
	CreatedAt string
	CreatedBy string
	UpdatedAt string
	UpdatedBy string
}

// TaskRef is a small reference used in lists (blockers, subtasks, ...).
type TaskRef struct {
	Key    string
	Title  string
	Type   domain.Type
	URL    string
}

// DrawerNote is one row in the notes timeline.
type DrawerNote struct {
	ID        string
	Author    string
	Body      string
	CreatedAt string // pre-formatted
	RelTime   string // "2m ago"
}

// DrawerHistoryEntry is one row in the history panel.
type DrawerHistoryEntry struct {
	TS      string
	RelTime string
	Actor   string
	Verb    string
	Detail  string
}

// AcceptanceView is one row of the acceptance checklist.
type AcceptanceView struct {
	Index int
	Text  string
	Done  bool
}

// ActivityModel is /p/{KEY}/activity — the live feed.
type ActivityModel struct {
	Project ProjectSummary
	Events  []ActivityEvent
}

// ActivityEvent is one row in the feed.
type ActivityEvent struct {
	ID       int64
	Actor    string
	Verb     string // human-friendly: "started", "moved to Review"
	Target   string // task key
	TargetTitle string
	RelTime  string
	TS       time.Time
	IsNew    bool // whether to apply enter animation
}

// OverviewModel is the / page — all projects, counts, focus per project.
type OverviewModel struct {
	Projects []OverviewProject
	Total    OverviewTotals
}

// OverviewProject is one card on the overview page.
type OverviewProject struct {
	Key         string
	Name        string
	Description string
	Focus       string
	Active      int // tasks in active columns
	Backlog     int
	Done        int
	URL         string
}

// OverviewTotals is the roll-up shown above the project grid.
type OverviewTotals struct {
	Projects   int
	Active     int
	Backlog    int
	Done       int
}

// LoginModel is /login.
type LoginModel struct {
	Error   string
	Redirect string
}

// FirstRunModel is the first-run landing page (same chrome as login but a
// different copy + snippet for the bootstrap token).
type FirstRunModel struct {
	Token    string
	BaseURL  string
	Projects []ProjectSummary
}

// AgentSetupModel is /agent-setup — copy-paste MCP config blocks.
type AgentSetupModel struct {
	BaseURL   string
	TokenName string
	Snippets  []AgentSnippet
}

// AgentSnippet is one of the per-client config blocks (Claude Code, Codex,
// Cursor, generic).
type AgentSnippet struct {
	ID          string // "claude" | "codex" | "cursor" | "generic"
	Title       string
	Description string
	Config      string // the JSON (or TOML) snippet
	Format      string // "json" | "toml" | "text"
}

// AdminModel is /admin.
type AdminModel struct {
	Tokens       []AdminToken
	Projects     []AdminProject
	ExportURL    string
	BackupURL    string
}

// AdminToken is one row in the tokens table.
type AdminToken struct {
	Name      string
	Scope     string
	ProjectKeys string
	CreatedAt string
	LastUsed  string
	Active    bool
}

// AdminProject is one row in the projects table.
type AdminProject struct {
	Key         string
	Name        string
	Columns     int
	ActiveTasks int
	Version     int
}

// BuildBoard constructs the BoardModel from a project + columns + tasks. It
// is the single place where the fixtures and (later) the real service meet
// the templates, so the templates can be exercised today and against live
// data later with no change to the HTML.
func BuildBoard(project ProjectSummary, focus *FocusCard, columns []domain.Column, tasks []domain.TaskView, hideDone bool) BoardModel {
	byCol := map[string][]domain.TaskView{}
	for _, t := range tasks {
		if t.ArchivedAt != nil {
			continue
		}
		byCol[t.ColumnID] = append(byCol[t.ColumnID], t)
	}
	out := BoardModel{
		Project: project,
		Focus:   focus,
		HideDone: hideDone,
	}
	for _, c := range columns {
		tasks := byCol[c.ID]
		cv := ColumnView{
			Name:  c.Name,
			Kind:  c.Kind,
			WIP:   c.WIPLimit,
			Count: len(tasks),
			Hidden: hideDone && c.Kind == domain.KindDone,
		}
		for _, t := range tasks {
			cv.Tasks = append(cv.Tasks, viewCard(project.Key, t))
		}
		out.Columns = append(out.Columns, cv)
		if c.Kind == domain.KindDone {
			out.DoneTotal = len(tasks)
		}
	}
	return out
}

// viewCard turns a TaskView into the card value the board template binds to.
func viewCard(projectKey string, t domain.TaskView) TaskCard {
	c := TaskCard{
		Key:         t.Key,
		Title:       t.Title,
		Type:        t.Type,
		Priority:    t.Priority,
		PriorityCSS: priorityCSS(t.Priority),
		Tags:        t.Tags,
		Assignee:    assignOf(t.Assignee),
		BlockedBy:   t.BlockedBy,
		Blocked:     len(t.BlockedBy) > 0,
		SubDone:     t.SubDone,
		SubTotal:    t.SubTotal,
		Version:     t.Version,
		HasBody:     t.Body != "",
		Age:         formatAge(t.ColumnEnteredAt, time.Now().UTC()),
		Updated:     t.UpdatedAt,
		URL:         "/t/" + url.PathEscape(t.Key),
	}
	if t.Estimate != nil {
		c.Estimate = &EstimateView{N: *t.Estimate, Unit: "h", Label: formatEstimate(*t.Estimate, "h")}
	}
	if t.ClaimedBy != nil {
		// Lease liveness is computed against the server clock here. The store
		// also passes LeaseRemain pre-computed; if it is set we trust it,
		// otherwise we recompute from ClaimExpiresAt.
		var remain time.Duration
		live := false
		if t.LeaseRemain != nil {
			remain = *t.LeaseRemain
			live = remain > 0
		} else if t.ClaimExpiresAt != nil {
			remain = time.Until(*t.ClaimExpiresAt)
			live = remain > 0
		}
		lv := &LeaseView{Actor: *t.ClaimedBy, IsMine: live}
		if live {
			lv.State = "live"
			lv.Remain = formatDuration(remain)
		} else {
			lv.State = "expired"
			lv.Remain = "expired"
		}
		c.Lease = lv
	}
	return c
}

func assignOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func priorityCSS(p domain.Priority) string {
	switch p {
	case domain.PriorityLow:
		return "prio-low"
	case domain.PriorityMedium:
		return "prio-medium"
	case domain.PriorityHigh:
		return "prio-high"
	case domain.PriorityCritical:
		return "prio-critical"
	default:
		return "prio-none"
	}
}

// priorityLabel returns the canonical name shown to humans.
func priorityLabel(p domain.Priority) string {
	if !p.Valid() {
		return "none"
	}
	return p.String()
}

// formatEstimate renders an estimate as e.g. "2h", "30m" (no unit conversion
// here — the project unit is assumed, the compact format is the same as the
// compact grammar: integer + unit).
func formatEstimate(n float64, unit string) string {
	if n == float64(int(n)) {
		return fmt.Sprintf("%d%s", int(n), unit)
	}
	return fmt.Sprintf("%.1f%s", n, unit)
}

// formatDuration renders a duration as the compact grammar: 12m, 3h, 2d.
// Anything under a minute collapses to "expired" so a lease that crossed
// zero mid-render does not show "0m".
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "expired"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// formatAge renders the time since column entry the same way.
func formatAge(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return formatDuration(d)
}

// relTime returns a short relative-time string ("2m ago", "3h ago", "5d ago").
func relTime(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// relTimeFuture is for due dates.
func relTimeFuture(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := t.Sub(now)
	if d < 0 {
		return "overdue"
	}
	if d < time.Hour {
		return fmt.Sprintf("in %dm", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("in %dh", int(d/time.Hour))
	}
	return fmt.Sprintf("in %dd", int(d/(24*time.Hour)))
}

// truncate shortens a string for compact display.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// joinTags joins tags for the compact grammar style: "#a #b #c".
func joinTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, "#"+t)
	}
	return strings.Join(out, " ")
}

// commaKeys joins keys for the compact grammar style.
func commaKeys(keys []string) string {
	return strings.Join(keys, ",")
}

// priorityName is the canonical name used by compact/UI. Exported so the
// MCP package can use it from a single source of truth if it ever needs to.
func PriorityName(p domain.Priority) string { return priorityLabel(p) }
