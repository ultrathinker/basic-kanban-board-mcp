// Package mcp: the compact board rendering.
//
// Render is the compact board grammar from PLAN §6.1.
// The grammar is line-oriented so it round-trips through both eyes and
// LLMs: every line is a fixed prefix (`#`, `##`, `-`), every suffix is
// labelled (`est`, `@assignee`, `lease`, `sub`, `blocked-by`, `#tag`, `v`,
// `age`), and every separator is ` · ` so the lexer never has to guess.
//
// `v<version>` is always present on a task line. That is the whole point of
// the cheap read: an agent doing read-modify-write from board_get must not
// need a follow-up task_get to learn the version it must echo back in
// if_version. Removing it would silently break the optimistic-concurrency
// contract for every client.
//
// Per architecture review finding #11, the reverse parser (ParseCompact)
// is test scaffolding for round-trip validation and lives in
// compact_parser_test.go so production binaries do not carry an uncalled parser.
package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const versionHeaderFmt = "compact_version=%d"

// versionHeader is what the first line of every rendering must say.
func versionHeader() string { return fmt.Sprintf(versionHeaderFmt, domain.CompactVersion) }

// Render renders a Board to the compact grammar.
//
// `now` is the reference time for `age` and `lease … <remaining>`. The
// renderer is pure given a fixed `now`: same inputs, same bytes.
//
// The estimate unit comes from each BoardProject. It used to arrive in a side
// map because the board carried no unit of its own, and every production
// caller passed nil — so a project configured in days silently rendered as
// hours. The unit is now on the project and the side map is gone.
func Render(b *service.Board, now time.Time) string {
	var sb strings.Builder
	sb.WriteString(versionHeader())
	sb.WriteByte('\n')
	for i := range b.Projects {
		renderProject(&sb, &b.Projects[i], now)
	}
	return sb.String()
}

// RenderAt is a convenience for `Render(b, time.Now())` — fine for
// the UI, not fine for tests, which must pass an explicit `now`.
func RenderAt(b *service.Board) string { return Render(b, time.Now()) }

func renderProject(sb *strings.Builder, p *service.BoardProject, now time.Time) {
	fmt.Fprintf(sb, "# %s %s · ", p.Key, p.Name)
	if p.FocusKey == "" {
		sb.WriteString("focus none")
	} else {
		fmt.Fprintf(sb, "focus %s", p.FocusKey)
	}
	for i := range p.Columns {
		col := &p.Columns[i]
		// Done is rendered as its own trailing segment, not as a column
		// header segment. The column may still appear in Columns when
		// DoneShown > 0 so task lines have a BoardColumn to attach to.
		if col.Kind == domain.KindDone {
			continue
		}
		sb.WriteString(" · ")
		writeColumnHeader(sb, col)
	}
	if p.DoneShown > 0 {
		fmt.Fprintf(sb, " · Done %d (%d shown)", p.DoneTotal, p.DoneShown)
	} else {
		fmt.Fprintf(sb, " · Done %d (hidden)", p.DoneTotal)
	}
	// `v<version>` closes the project header for the same reason it closes
	// every task line: project_upsert(mode:"update") requires if_version, and
	// compact is the DEFAULT board_get format — without it here, the cheapest
	// read cannot feed the write that follows it, and an admin needs a tool
	// that does not exist. Four bytes per project, once, is the whole cost.
	// It goes last so a reader scanning for the `Done …` segment, and any
	// parser that stops there, is unaffected by its arrival.
	fmt.Fprintf(sb, " · v%d\n", p.Version)

	for i := range p.Columns {
		col := &p.Columns[i]
		// Hide the Done section when nothing is being shown; the header still
		// carries the total and the (hidden) marker.
		if col.Kind == domain.KindDone && p.DoneShown == 0 {
			continue
		}
		// A section with no task lines under it says nothing the header segment
		// did not already say, and in `view:"summary"` — where no column
		// carries tasks by definition — it said it for every column at once,
		// so the cheapest read in the product opened with four empty headings.
		// Worse, an empty heading is ambiguous: it reads the same whether the
		// column is empty, the view omitted tasks, or a filter matched none.
		// The header segment (`Backlog 6`) is the unambiguous answer.
		if len(col.Tasks) == 0 {
			continue
		}
		fmt.Fprintf(sb, "## %s\n", col.Name)
		for j := range col.Tasks {
			renderTask(sb, &col.Tasks[j], col.Kind, now, p.EstimateUnit)
		}
	}
}

func writeColumnHeader(sb *strings.Builder, col *service.BoardColumn) {
	if col.WIPLimit != nil {
		fmt.Fprintf(sb, "%s %d/%d", col.Name, col.Count, *col.WIPLimit)
		return
	}
	fmt.Fprintf(sb, "%s %d", col.Name, col.Count)
}

func renderTask(sb *strings.Builder, tv *domain.TaskView, kind domain.Kind, now time.Time, unit string) {
	if unit == "" {
		unit = "h"
	}
	fmt.Fprintf(sb, "- %s ", tv.Key)
	if tv.Priority == domain.PriorityNone {
		fmt.Fprintf(sb, "[%s] ", tv.Type)
	} else {
		fmt.Fprintf(sb, "[%s %s] ", tv.Priority.String(), tv.Type)
	}

	// The title may contain the field separator; replace it with a neutral
	// dash so the line stays lexable. domain.NormalizeTitle already collapses
	// internal whitespace and strips newlines.
	title := strings.ReplaceAll(domain.NormalizeTitle(tv.Title), " · ", " - ")
	sb.WriteString(title)

	var parts []string
	if tv.Estimate != nil {
		parts = append(parts, "est "+formatEstimate(*tv.Estimate, unit))
	}
	if tv.Assignee != nil && *tv.Assignee != "" {
		parts = append(parts, "@"+*tv.Assignee)
	}
	if tv.ClaimedBy != nil {
		var rem *time.Duration
		if tv.LeaseRemain != nil && *tv.LeaseRemain > 0 {
			d := *tv.LeaseRemain
			rem = &d
		} else {
			rem = domain.LeaseRemaining(&tv.Task, now)
		}
		if rem != nil && *rem > 0 {
			parts = append(parts, fmt.Sprintf("lease %s %s", *tv.ClaimedBy, formatDuration(*rem)))
		} else {
			parts = append(parts, fmt.Sprintf("lease %s expired", *tv.ClaimedBy))
		}
	}
	if tv.SubTotal > 0 {
		parts = append(parts, fmt.Sprintf("sub %d/%d", tv.SubDone, tv.SubTotal))
	}
	if len(tv.BlockedBy) > 0 {
		parts = append(parts, "blocked-by "+strings.Join(tv.BlockedBy, ","))
	}
	if len(tv.Tags) > 0 {
		tags := append([]string(nil), tv.Tags...)
		sort.Strings(tags)
		out := make([]string, len(tags))
		for i, t := range tags {
			out[i] = "#" + t
		}
		parts = append(parts, strings.Join(out, " "))
	}
	parts = append(parts, fmt.Sprintf("v%d", tv.Version))
	if kind == domain.KindActive && !tv.ColumnEnteredAt.IsZero() {
		age := now.Sub(tv.ColumnEnteredAt)
		if age < 0 {
			age = 0
		}
		parts = append(parts, "age "+formatDuration(age))
	}

	if len(parts) > 0 {
		sb.WriteString(" · ")
		sb.WriteString(strings.Join(parts, " · "))
	}
	sb.WriteByte('\n')
}

// formatEstimate prints a number without trailing zeros so 2h stays "2h" and
// 2.5h is "2.5h". %g is the right tool for that.
func formatEstimate(n float64, unit string) string { return fmt.Sprintf("%g%s", n, unit) }

// formatDuration renders a positive duration as the largest single unit, no
// decimals: 45s, 12m, 3h, 2d. Negative durations are clamped to 0s — only
// the caller can pass them, and only by mistake.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
