package mcp

import (
	"context"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opBoardGet = "board_get"

// boardFilterIn is the wire form of PLAN §6.1's `filter` object.
type boardFilterIn struct {
	Columns      []string `json:"columns,omitempty" jsonschema:"column names to include (OR)"`
	Types        []string `json:"types,omitempty" jsonschema:"task types to include (OR)"`
	PriorityMin  string   `json:"priority_min,omitempty" jsonschema:"minimum priority name, inclusive"`
	Tags         []string `json:"tags,omitempty" jsonschema:"tags to include (OR)"`
	Assignee     string   `json:"assignee,omitempty" jsonschema:"exact assignee match"`
	Claimed      string   `json:"claimed,omitempty" jsonschema:"claim state filter"`
	Blocked      *bool    `json:"blocked,omitempty" jsonschema:"true = has an open blocker, false = has none"`
	Q            string   `json:"q,omitempty" jsonschema:"title/body substring search"`
	UpdatedSince string   `json:"updated_since,omitempty" jsonschema:"RFC3339 timestamp; only tasks updated at or after it"`
}

type boardGetInput struct {
	Project   string         `json:"project,omitempty" jsonschema:"project key; omitted = every accessible project; REQUIRED for view:messages"`
	View      string         `json:"view,omitempty" jsonschema:"tasks = full board, summary = counts only, messages = the communication feed read forward from the beginning; default depends on whether project is set"`
	DoneLimit int            `json:"done_limit,omitempty" jsonschema:"how many done tasks to include, most recently done first"`
	Filter    *boardFilterIn `json:"filter,omitempty"`
	Include   []string       `json:"include,omitempty" jsonschema:"widen what is returned: per-task fields, and description for the project's own description (omitted by default)"`
	Format    string         `json:"format,omitempty" jsonschema:"compact = the token-cheap text grammar, json = the same board as compact JSON text"`
	// After pages the messages view FORWARD: it is the opaque next_cursor a
	// previous page returned. Without it the feed starts at the OLDEST
	// message, never at "now" — a consumer must not miss commands written
	// before it started (KANB-45).
	After string `json:"after,omitempty" jsonschema:"messages view only: the next_cursor of a previous page; omit to read the feed from the beginning"`
	Limit int    `json:"limit,omitempty" jsonschema:"messages view only: page size"`
}

type boardColumnOut struct {
	Name  string      `json:"name"`
	Kind  domain.Kind `json:"kind"`
	Count int         `json:"count"`
	Tasks []taskOut   `json:"tasks,omitempty"`
}

// participantOut is one actor who may take part in the project's
// communication (KANB-44): the stable tokens.id plus the display name. The
// secret never travels — there is no field for it.
type participantOut struct {
	TokenID string `json:"token_id"`
	Name    string `json:"name"`
}

func participantOuts(ps []service.Participant) []participantOut {
	if len(ps) == 0 {
		return nil
	}
	out := make([]participantOut, len(ps))
	for i, p := range ps {
		out[i] = participantOut{TokenID: p.TokenID, Name: p.Name}
	}
	return out
}

func participantOutPtr(p *service.Participant) *participantOut {
	if p == nil {
		return nil
	}
	return &participantOut{TokenID: p.TokenID, Name: p.Name}
}

// boardProjectOut publishes the project's whole configuration, not just its
// identity. `version` in particular closes a loop that was open: project_upsert
// (mode:"update") requires if_version and this is the only tool that reads a
// project, so without it an administrator could not reconfigure an existing
// project through the tools at all. The settings come with it so a caller
// can send a modified copy of what it read instead of guessing at the values
// it is about to overwrite.
type boardProjectOut struct {
	Key                 string `json:"key"`
	Name                string `json:"name"`
	Description         string `json:"description,omitempty"`
	Version             int    `json:"version" jsonschema:"the project configuration version; send it back as project_upsert's if_version"`
	FocusKey            string `json:"focus_key,omitempty"`
	EstimateUnit        string `json:"estimate_unit" jsonschema:"the unit every estimate on this project is expressed in, e.g. h or d"`
	EnforceDependencies bool   `json:"enforce_dependencies"`
	StrictDone          bool   `json:"strict_done"`
	ClaimTTLSeconds     int    `json:"claim_ttl_seconds"`
	Archived            bool   `json:"archived,omitempty"`
	// Coordinator names the appointed coordinator (nil/absent when none),
	// Participants everyone who MAY participate: every active token with
	// access to the project. Access is permission, not presence (KANB-44).
	// Pass coordinator as project_upsert's settings.coordinator (tokens.id).
	Coordinator  *participantOut  `json:"coordinator,omitempty"`
	Participants []participantOut `json:"participants,omitempty" jsonschema:"derived from tokens with access to this project; may participate, not is-present"`
	Columns      []boardColumnOut `json:"columns"`
	DoneTotal    int              `json:"done_total"`
	DoneShown    int              `json:"done_shown"`
}

type boardGetData struct {
	Projects []boardProjectOut `json:"projects"`
}

type boardGetOutput struct {
	OK   bool          `json:"ok"`
	Op   string        `json:"op"`
	Data *boardGetData `json:"data,omitempty"`
	// Messages carries the view:"messages" payload. It is a sibling of Data,
	// not inside it, so the two shapes cannot be confused: a feed page is
	// never a board projection.
	Messages *boardMessagesData `json:"messages,omitempty"`
	Meta     *toolMeta          `json:"meta,omitempty"`
	Error    *errorEnvelope     `json:"error,omitempty"`
}

func boardGetTool() *gomcp.Tool {
	s := schemaFor[boardGetInput]()
	setEnum(prop(s, "view"), string(service.ViewTasks), string(service.ViewSummary), string(service.ViewMessages))
	setDefault(prop(s, "done_limit"), 0)
	setMin(prop(s, "done_limit"), 0)
	setMax(prop(s, "done_limit"), float64(domain.MaxDoneLimit))
	inc := prop(s, "include")
	setEnum(inc.Items, append(append([]string{}, includeEnumValues...), includeDescription)...)
	setDefault(inc, []string{})
	setEnum(prop(s, "format"), "compact", "json")
	setDefault(prop(s, "format"), "compact")
	setDefault(prop(s, "limit"), 50)
	setMin(prop(s, "limit"), 1)
	setMax(prop(s, "limit"), 100)

	filter := prop(s, "filter")
	setEnum(prop(filter, "types"), typeNames()...)
	setEnum(prop(filter, "priority_min"), priorityNames()...)
	setEnum(prop(filter, "claimed"), "any", "mine", "unclaimed", "other")
	setMaxItems(prop(filter, "tags"), domain.MaxTags)
	setMaxLen(prop(filter, "tags").Items, domain.MaxTagLen)

	return &gomcp.Tool{
		Name: opBoardGet,
		Description: "Read the board. Compact text by default — about 1,000 tokens for 30 active tasks, roughly 90% smaller than the same board as JSON, so it is cheap enough to call at the start of every session. `format:\"json\"` returns the same board as JSON text instead. The project's description — its rulebook — is NOT returned unless asked for: add `include:[\"description\"]` once per session, or again after your context was compacted.\n" +
			"`view:\"messages\"` reads one project's communication feed forward through history instead of the board: `messages[]` in chronological order (each with `id`, `created_at`, `author`, `author_token_id` — the authorized source, which the caller never chooses — `kind`, `recipient`/`recipient_name`, `resolved_executor`/`resolved_executor_name`, `reply_to`, `body`, and the `task_keys` created by accepting that command), plus `next_cursor` and `has_more`. The board itself and task bodies are NOT part of that response. Chat history is never pruned, so a cursor never goes stale; a cursor from another project, a cursor naming a message that does not exist, or an unparseable cursor is refused with an actionable error rather than a silent empty page.\n" +
			"Feed participants and the project's coordinator are published by `view:\"summary\"` as `projects[].participants[]` and `projects[].coordinator` — token ids and display names only, never secrets.",
		InputSchema: s,
	}
}

// includeDescription widens board_get's PROJECT with its description. It is
// not a task field, so it lives outside includeEnumValues: task_get and
// task_next have no project to widen.
const includeDescription = "description"

// splitDescriptionInclude separates board_get's project-level include from
// the per-task ones the service understands.
func splitDescriptionInclude(vals []string) (bool, []string) {
	want := false
	rest := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == includeDescription {
			want = true
			continue
		}
		rest = append(rest, v)
	}
	return want, rest
}

// includeEnumValues is the full include enum PLAN §6 defines for board_get
// and task_get. task_next publishes a narrower subset (no "metadata").
var includeEnumValues = []string{
	string(service.IncludeBody),
	string(service.IncludeAcceptance),
	string(service.IncludeNotes),
	string(service.IncludeLinks),
	string(service.IncludeMetadata),
}

func boardFilterToService(f *boardFilterIn) (service.BoardFilter, *domain.Error) {
	if f == nil {
		return service.BoardFilter{}, nil
	}
	out := service.BoardFilter{
		Columns: f.Columns,
		Tags:    f.Tags,
		Claimed: f.Claimed,
		Blocked: f.Blocked,
		Query:   f.Q,
	}
	if f.Assignee != "" {
		a := f.Assignee
		out.Assignee = &a
	}
	if len(f.Types) > 0 {
		out.Types = make([]domain.Type, len(f.Types))
		for i, t := range f.Types {
			out.Types[i] = domain.Type(t)
		}
	}
	if f.PriorityMin != "" {
		p, derr := parsePriorityName("filter.priority_min", f.PriorityMin)
		if derr != nil {
			return service.BoardFilter{}, derr
		}
		out.PriorityMin = &p
	}
	if f.UpdatedSince != "" {
		t, derr := parseRFC3339("filter.updated_since", f.UpdatedSince)
		if derr != nil {
			return service.BoardFilter{}, derr
		}
		out.UpdatedSince = &t
	}
	return out, nil
}

func registerBoardGet(s *gomcp.Server, svc service.Service) {
	tool := boardGetTool()
	addTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in boardGetInput) (*gomcp.CallToolResult, boardGetOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opBoardGet, aerr), boardGetOutput{OK: false, Op: opBoardGet, Error: newErrorEnvelope(aerr)}, nil
		}

		filter, ferr := boardFilterToService(in.Filter)
		if ferr != nil {
			return errorResult(opBoardGet, ferr), boardGetOutput{OK: false, Op: opBoardGet, Error: newErrorEnvelope(ferr)}, nil
		}

		view := service.BoardView(in.View)
		if view == "" {
			if in.Project == "" {
				view = service.ViewSummary
			} else {
				view = service.ViewTasks
			}
		}

		// view:"messages" is a different payload, not a board projection:
		// it reads the feed forward and returns messages + cursor only — the
		// board, its columns and task bodies are deliberately absent
		// (KANB-45). It stays inside board_get so the tool count does not
		// grow.
		if view == service.ViewMessages {
			if strings.TrimSpace(in.Project) == "" {
				derr := domain.Invalid("project", "project is required for view:messages",
					"Pass the project key whose feed you are reading, e.g. {\"project\":\"KANB\",\"view\":\"messages\"}.")
				return errorResult(opBoardGet, derr), boardGetOutput{OK: false, Op: opBoardGet, Error: newErrorEnvelope(derr)}, nil
			}
			key, kerr := domain.ValidateProjectKey(in.Project)
			if kerr != nil {
				derr := domain.AsError(kerr)
				return errorResult(opBoardGet, derr), boardGetOutput{OK: false, Op: opBoardGet, Error: newErrorEnvelope(derr)}, nil
			}
			res, err := svc.ChatFeed(ctx, actor, service.ChatFeedInput{
				ProjectKey: key,
				After:      in.After,
				Limit:      in.Limit,
			})
			if err != nil {
				derr := asDomainError(err)
				return errorResult(opBoardGet, derr), boardGetOutput{OK: false, Op: opBoardGet, Error: newErrorEnvelope(derr)}, nil
			}
			md := &boardMessagesData{
				Project:    key,
				Messages:   feedMessagesOut(res),
				NextCursor: res.NextCursor,
				HasMore:    res.HasMore,
			}
			mout := boardGetOutput{OK: true, Op: opBoardGet, Messages: md, Meta: &toolMeta{Count: len(md.Messages)}}
			text := renderMessagesFeed(md)
			if in.Format == "json" {
				text = jsonText(mout)
			}
			return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: text}}}, mout, nil
		}

		// "description" widens the PROJECT, not the tasks, so it is split off
		// before the per-task includes reach the service.
		wantDescription, taskIncludes := splitDescriptionInclude(in.Include)
		board, err := svc.BoardGet(ctx, actor, service.BoardGetInput{
			ProjectKey: in.Project,
			View:       view,
			DoneLimit:  in.DoneLimit,
			Filter:     filter,
			Include:    toIncludes(taskIncludes),
		})
		if err != nil {
			derr := asDomainError(err)
			return errorResult(opBoardGet, derr), boardGetOutput{OK: false, Op: opBoardGet, Error: newErrorEnvelope(derr)}, nil
		}

		// board_get returns whatever `include` selected, whole: it has no
		// bounded tier, so the projection is the include set as-is.
		proj := service.FullProjection(toIncludes(taskIncludes))
		// The description is thousands of characters that do not change
		// between two reads, and it used to ride along with every call — even
		// one that matched no task at all (KANB-58). Zeroing it here drops it
		// from both renderings at once.
		if !wantDescription {
			// Work on a copy: the board belongs to the service, which may hand
			// the same value to another caller (the test fakes do exactly
			// that, and a guard caught the first version of this mutating it).
			shown := *board
			shown.Projects = append([]service.BoardProject(nil), board.Projects...)
			for i := range shown.Projects {
				shown.Projects[i].Description = ""
			}
			board = &shown
		}
		data := boardGetData{Projects: make([]boardProjectOut, len(board.Projects))}
		total := 0
		for i := range board.Projects {
			p := &board.Projects[i]
			cols := make([]boardColumnOut, len(p.Columns))
			for j := range p.Columns {
				c := &p.Columns[j]
				cols[j] = boardColumnOut{
					Name:  c.Name,
					Kind:  c.Kind,
					Count: c.Count,
					Tasks: taskViewOutList(c.Tasks, proj),
				}
				total += len(c.Tasks)
			}
			data.Projects[i] = boardProjectOut{
				Key:                 p.Key,
				Name:                p.Name,
				Description:         p.Description,
				Version:             p.Version,
				FocusKey:            p.FocusKey,
				EstimateUnit:        p.EstimateUnit,
				EnforceDependencies: p.EnforceDependencies,
				StrictDone:          p.StrictDone,
				ClaimTTLSeconds:     p.ClaimTTLSeconds,
				Archived:            p.Archived,
				Coordinator:         participantOutPtr(p.Coordinator),
				Participants:        participantOuts(p.Participants),
				Columns:             cols,
				DoneTotal:           p.DoneTotal,
				DoneShown:           p.DoneShown,
			}
		}

		out := boardGetOutput{OK: true, Op: opBoardGet, Data: &data, Meta: &toolMeta{Count: total}}

		var text string
		if in.Format == "json" {
			text = jsonText(out)
		} else {
			text = Render(board, renderNow())
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: text}}}, out, nil
	})
}
