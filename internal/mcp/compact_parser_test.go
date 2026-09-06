package mcp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// ParseCompact — the inverse (test-only per architecture review finding #11).
// The compact parser ships only in test binaries to validate that the compact
// renderer is lossless in round-trip tests; production code never calls it.
// ---------------------------------------------------------------------------

// ParseCompact parses a compact rendering back into a Board. The grammar is
// strict: a malformed line returns an error with the line number.
//
// ParseCompact uses the zero time as the reference for `age` and `lease … <remaining>`,
// so a parsed Board round-trips with byte parity only when the renderer uses
// the same reference. Callers that need full preservation (round-trip tests,
// callers re-rendering with the same wall clock) should use ParseCompactAt.
//
// The parser recovers the column Kind from the grammar: a `count/wip`
// segment means active, a bare count means backlog, the trailing `Done …`
// segment is the done column. An active column without a WIP limit (the
// default `Review` column) parses as backlog — that lossiness is accepted
// because Kind is renderer-side state that no consuming agent reads from
// the compact line. The Done column is also recovered from the trailing
// segment when its `## Done` section appears with task lines.
func ParseCompact(s string) (*service.Board, error) {
	return ParseCompactAt(s, time.Time{})
}

// ParseCompactAt is ParseCompact with an explicit reference time. The
// reference is used to anchor parsed leases (the expiry is `now + remaining`)
// and parsed ages (`ColumnEnteredAt = now - age`). Same reference at both
// ends of a render → parse → render cycle produces identical bytes.
func ParseCompactAt(s string, now time.Time) (*service.Board, error) {
	s = strings.TrimRight(s, "\r\n")
	if s == "" {
		return nil, fmt.Errorf("empty compact input")
	}
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty compact input")
	}
	if lines[0] != versionHeader() {
		return nil, fmt.Errorf("line 1: missing or wrong version header %q, want %q", lines[0], versionHeader())
	}

	var board service.Board
	var currentProj *service.BoardProject
	var currentCol *service.BoardColumn

	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			currentCol = nil
			continue
		}
		switch {
		case strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, "## "):
			proj, err := parseProjectHeader(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			board.Projects = append(board.Projects, *proj)
			currentProj = &board.Projects[len(board.Projects)-1]
			currentCol = nil
		case strings.HasPrefix(line, "## "):
			if currentProj == nil {
				return nil, fmt.Errorf("line %d: column section before any project header", i+1)
			}
			name := strings.TrimPrefix(line, "## ")
			matched := findColumnByName(currentProj, name)
			if matched == nil {
				if name != "Done" {
					return nil, fmt.Errorf("line %d: column %q has no matching segment in the project header", i+1, name)
				}
				// The Done column is not in the header segments; it lives in
				// DoneTotal/DoneShown. Materialise it on demand so task lines
				// have a place to land.
				currentProj.Columns = append(currentProj.Columns, service.BoardColumn{
					Name:  "Done",
					Kind:  domain.KindDone,
					Count: currentProj.DoneShown,
				})
				matched = &currentProj.Columns[len(currentProj.Columns)-1]
			}
			currentCol = matched
		case strings.HasPrefix(line, "- "):
			if currentCol == nil {
				return nil, fmt.Errorf("line %d: task line outside any column section", i+1)
			}
			tv, err := parseTaskLine(line, now)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			currentCol.Tasks = append(currentCol.Tasks, *tv)
		default:
			return nil, fmt.Errorf("line %d: unrecognised prefix in %q", i+1, line)
		}
	}

	return &board, nil
}

func findColumnByName(p *service.BoardProject, name string) *service.BoardColumn {
	for i := range p.Columns {
		if p.Columns[i].Name == name {
			return &p.Columns[i]
		}
	}
	return nil
}

var projectHeaderRe = regexp.MustCompile(`^# (.+)$`)

func parseProjectHeader(line string) (*service.BoardProject, error) {
	m := projectHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return nil, fmt.Errorf("not a project header")
	}
	body := m[1]
	parts := strings.Split(body, " · ")
	if len(parts) < 3 {
		return nil, fmt.Errorf("project header too short: %q", line)
	}

	keyName := parts[0]
	sp := strings.IndexByte(keyName, ' ')
	if sp <= 0 {
		return nil, fmt.Errorf("project header must start with KEY and a name, got %q", keyName)
	}
	proj := &service.BoardProject{
		Key:  keyName[:sp],
		Name: keyName[sp+1:],
	}

	if !strings.HasPrefix(parts[1], "focus ") {
		return nil, fmt.Errorf("missing focus segment: %q", parts[1])
	}
	focusKey := strings.TrimPrefix(parts[1], "focus ")
	if focusKey != "none" {
		if _, _, err := domain.ParseTaskKey(focusKey); err != nil {
			return nil, fmt.Errorf("focus key %q is not a task key", focusKey)
		}
		proj.FocusKey = focusKey
	}

	rest := parts[2:]
	// The trailing `v<version>` segment carries what project_upsert needs as
	// if_version. It sits after the Done segment so that everything between
	// focus and Done stays column segments, as it always was.
	if last := rest[len(rest)-1]; projectVersionRe.MatchString(last) {
		proj.Version = atoiSafe(strings.TrimPrefix(last, "v"))
		rest = rest[:len(rest)-1]
	}
	doneIdx := -1
	for i, p := range rest {
		if strings.HasPrefix(p, "Done ") {
			doneIdx = i
			break
		}
	}
	if doneIdx == -1 {
		return nil, fmt.Errorf("project header has no Done segment")
	}
	for i := 0; i < doneIdx; i++ {
		col, err := parseColumnSegment(rest[i])
		if err != nil {
			return nil, fmt.Errorf("column segment %q: %w", rest[i], err)
		}
		proj.Columns = append(proj.Columns, *col)
	}
	if err := parseDoneSegment(rest[doneIdx], proj); err != nil {
		return nil, fmt.Errorf("Done segment %q: %w", rest[doneIdx], err)
	}
	return proj, nil
}

var (
	columnSegmentRe  = regexp.MustCompile(`^(.+) (\d+)(?:/(\d+))?$`)
	doneSegmentRe    = regexp.MustCompile(`^Done (\d+) \((?:(\d+) shown|hidden)\)$`)
	projectVersionRe = regexp.MustCompile(`^v\d+$`)
)

func parseColumnSegment(s string) (*service.BoardColumn, error) {
	m := columnSegmentRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("malformed column segment")
	}
	col := &service.BoardColumn{
		Name:  m[1],
		Count: atoiSafe(m[2]),
	}
	if m[3] != "" {
		w := atoiSafe(m[3])
		col.WIPLimit = &w
		col.Kind = domain.KindActive
	} else {
		// Default kind; the Done column is special-cased at the section header.
		col.Kind = domain.KindBacklog
	}
	return col, nil
}

func parseDoneSegment(s string, p *service.BoardProject) error {
	m := doneSegmentRe.FindStringSubmatch(s)
	if m == nil {
		return fmt.Errorf("malformed")
	}
	p.DoneTotal = atoiSafe(m[1])
	if m[2] != "" {
		p.DoneShown = atoiSafe(m[2])
	}
	return nil
}

// taskKeyRe matches the leading KEY on a task line, including the trailing
// space, so the next character after the match is the bracket or the title.
var taskKeyRe = regexp.MustCompile(`^([A-Z][A-Z0-9]{1,7}-\d+) (.+)$`)

func parseTaskLine(line string, now time.Time) (*domain.TaskView, error) {
	// Caller has already verified the `- ` prefix; strip it for the regex.
	body := strings.TrimPrefix(line, "- ")
	m := taskKeyRe.FindStringSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("missing or malformed task key")
	}
	tv := &domain.TaskView{Task: domain.Task{Key: m[1]}}
	rest := m[2]

	// Optional `[priority type]` (priority omitted when "none").
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return nil, fmt.Errorf("unclosed bracket in %q", rest)
		}
		bracket := rest[1:end]
		inner := strings.TrimSpace(bracket)
		bparts := strings.SplitN(inner, " ", 2)
		switch len(bparts) {
		case 1:
			tv.Type = domain.Type(bparts[0])
			if !tv.Type.Valid() {
				return nil, fmt.Errorf("invalid type %q", bparts[0])
			}
		case 2:
			pri, ok := domain.ParsePriority(bparts[0])
			if !ok {
				return nil, fmt.Errorf("invalid priority %q", bparts[0])
			}
			tv.Priority = pri
			tv.Type = domain.Type(bparts[1])
			if !tv.Type.Valid() {
				return nil, fmt.Errorf("invalid type %q", bparts[1])
			}
		default:
			return nil, fmt.Errorf("malformed bracket %q", bracket)
		}
		rest = strings.TrimPrefix(rest[end+1:], " ")
	}

	// Title + optional suffixes, separated by " · ". The title must come
	// first; it is everything up to the first " · ".
	sep := " · "
	parts := strings.Split(rest, sep)
	if parts[0] == "" {
		return nil, fmt.Errorf("empty title")
	}
	tv.Title = parts[0]
	suffixes := parts[1:]

	if err := parseSuffixes(tv, suffixes, now); err != nil {
		return nil, err
	}
	return tv, nil
}

// parseSuffixes walks the suffix list in the fixed order, parsing each
// matching prefix and bailing if anything is malformed. The order matters:
// the renderer emits them in this order, so any deviation is a renderer
// bug, not a parser tolerance.
func parseSuffixes(tv *domain.TaskView, suffixes []string, now time.Time) error {
	idx := 0

	// est <float><unit>
	if idx < len(suffixes) && strings.HasPrefix(suffixes[idx], "est ") {
		body := strings.TrimPrefix(suffixes[idx], "est ")
		n, unit, ok := splitEstimate(body)
		if !ok {
			return fmt.Errorf("malformed estimate %q", suffixes[idx])
		}
		tv.Estimate = &n
		_ = unit // unit is per-project; the renderer side-channel carries it
		idx++
	}

	// @<assignee>
	if idx < len(suffixes) && strings.HasPrefix(suffixes[idx], "@") {
		a := strings.TrimPrefix(suffixes[idx], "@")
		if a == "" {
			return fmt.Errorf("empty assignee")
		}
		tv.Assignee = &a
		idx++
	}

	// lease <actor> <remaining>|expired
	if idx < len(suffixes) && strings.HasPrefix(suffixes[idx], "lease ") {
		body := strings.TrimPrefix(suffixes[idx], "lease ")
		actor, rem, expired, err := parseLease(body)
		if err != nil {
			return fmt.Errorf("malformed lease %q: %w", suffixes[idx], err)
		}
		tv.ClaimedBy = &actor
		if expired {
			// Set the expiry into the past relative to `now` so a re-render
			// with the same reference still says "expired".
			past := now.Add(-time.Second)
			tv.ClaimedAt = &past
			tv.ClaimExpiresAt = &past
		} else {
			// Anchor the lease to the reference time so the renderer sees a
			// live lease with the same remaining duration.
			claimed := now
			exp := now.Add(rem)
			tv.ClaimedAt = &claimed
			tv.ClaimExpiresAt = &exp
			tv.LeaseRemain = &rem
		}
		idx++
	}

	// sub <done>/<total>
	if idx < len(suffixes) && strings.HasPrefix(suffixes[idx], "sub ") {
		body := strings.TrimPrefix(suffixes[idx], "sub ")
		dp := strings.SplitN(body, "/", 2)
		if len(dp) != 2 {
			return fmt.Errorf("malformed sub %q", suffixes[idx])
		}
		tv.SubDone = atoiSafe(dp[0])
		tv.SubTotal = atoiSafe(dp[1])
		idx++
	}

	// blocked-by <KEY>(,<KEY>)*
	if idx < len(suffixes) && strings.HasPrefix(suffixes[idx], "blocked-by ") {
		body := strings.TrimPrefix(suffixes[idx], "blocked-by ")
		keys := strings.Split(body, ",")
		for _, k := range keys {
			if _, _, err := domain.ParseTaskKey(k); err != nil {
				return fmt.Errorf("bad blocker key %q: %w", k, err)
			}
		}
		tv.BlockedBy = append([]string(nil), keys...)
		idx++
	}

	// #tag #tag ... — tags are space-separated within one suffix block, not
	// · -separated like every other suffix. The renderer emits e.g.
	// "#release #sync #tagb" as a single joined suffix; splitting on ` · `
	// would not work, so split the block on whitespace here.
	if idx < len(suffixes) && strings.HasPrefix(suffixes[idx], "#") {
		var tags []string
		for _, raw := range strings.Fields(suffixes[idx]) {
			if !strings.HasPrefix(raw, "#") {
				return fmt.Errorf("malformed tag %q in %q", raw, suffixes[idx])
			}
			tag := strings.TrimPrefix(raw, "#")
			if tag == "" {
				return fmt.Errorf("empty tag in %q", suffixes[idx])
			}
			tags = append(tags, tag)
		}
		idx++
		tv.Tags = tags
	}

	// v<version> is required.
	if idx >= len(suffixes) || !strings.HasPrefix(suffixes[idx], "v") {
		return fmt.Errorf("missing required v<version>")
	}
	body := strings.TrimPrefix(suffixes[idx], "v")
	if !allDigits(body) {
		return fmt.Errorf("malformed version %q", suffixes[idx])
	}
	tv.Version = atoiSafe(body)
	idx++

	// age <duration> (active columns only; not strictly required, but
	// anything after v must be age).
	if idx < len(suffixes) {
		if !strings.HasPrefix(suffixes[idx], "age ") {
			return fmt.Errorf("unexpected trailing suffix %q", suffixes[idx])
		}
		body := strings.TrimPrefix(suffixes[idx], "age ")
		age, err := parseDuration(body)
		if err != nil {
			return fmt.Errorf("malformed age %q: %w", suffixes[idx], err)
		}
		// Anchor ColumnEnteredAt to the reference time so a re-render with
		// the same reference computes the same age.
		entered := now.Add(-age)
		tv.ColumnEnteredAt = entered
		idx++
	}
	if idx != len(suffixes) {
		return fmt.Errorf("trailing content after parsed suffixes")
	}
	return nil
}

var (
	estimateRe  = regexp.MustCompile(`^(\d+(?:\.\d+)?)([a-z]+)$`)
	leaseRe     = regexp.MustCompile(`^(\S+) (\d+[smhd]|expired)$`)
	durationRe  = regexp.MustCompile(`^\d+[smhd]$`)
	allDigitsRe = regexp.MustCompile(`^\d+$`)
)

func splitEstimate(s string) (float64, string, bool) {
	m := estimateRe.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, "", false
	}
	return n, m[2], true
}

func parseLease(s string) (actor string, remaining time.Duration, expired bool, err error) {
	m := leaseRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, false, fmt.Errorf("malformed")
	}
	if m[2] == "expired" {
		return m[1], 0, true, nil
	}
	d, perr := parseDuration(m[2])
	if perr != nil {
		return "", 0, false, perr
	}
	return m[1], d, false, nil
}

func parseDuration(s string) (time.Duration, error) {
	if !durationRe.MatchString(s) {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	n, _ := strconv.Atoi(s[:len(s)-1])
	unit := s[len(s)-1]
	switch unit {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unknown unit %q", unit)
}

func isDuration(s string) bool { return durationRe.MatchString(s) }
func allDigits(s string) bool  { return allDigitsRe.MatchString(s) }
func atoiSafe(s string) int    { n, _ := strconv.Atoi(s); return n }
