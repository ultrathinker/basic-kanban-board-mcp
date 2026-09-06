package mcp

import (
	"context"

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
	Project   string         `json:"project,omitempty" jsonschema:"project key; omitted = every accessible project"`
	View      string         `json:"view,omitempty" jsonschema:"tasks = full board, summary = counts only; default depends on whether project is set"`
	DoneLimit int            `json:"done_limit,omitempty" jsonschema:"how many done tasks to include, most recently done first"`
	Filter    *boardFilterIn `json:"filter,omitempty"`
	Include   []string       `json:"include,omitempty" jsonschema:"widen the per-task fields returned"`
	Format    string         `json:"format,omitempty" jsonschema:"compact = the token-cheap text grammar, json = pretty JSON text (structuredContent is always JSON either way)"`
}

type boardColumnOut struct {
	Name     string      `json:"name"`
	Kind     domain.Kind `json:"kind"`
	WIPLimit *int        `json:"wip_limit,omitempty"`
	Count    int         `json:"count"`
	Tasks    []taskOut   `json:"tasks,omitempty"`
}

// boardProjectOut publishes the project's whole configuration, not just its
// identity. `version` in particular closes a loop that was open: project_upsert
// (mode:"update") requires if_version and this is the only tool that reads a
// project, so without it an administrator could not reconfigure an existing
// project through the nine tools at all. The settings come with it so a caller
// can send a modified copy of what it read instead of guessing at the values
// it is about to overwrite.
type boardProjectOut struct {
	Key                 string           `json:"key"`
	Name                string           `json:"name"`
	Description         string           `json:"description,omitempty"`
	Version             int              `json:"version" jsonschema:"the project configuration version; send it back as project_upsert's if_version"`
	FocusKey            string           `json:"focus_key,omitempty"`
	EstimateUnit        string           `json:"estimate_unit" jsonschema:"the unit every estimate on this project is expressed in, e.g. h or d"`
	EnforceDependencies bool             `json:"enforce_dependencies"`
	StrictDone          bool             `json:"strict_done"`
	ClaimTTLSeconds     int              `json:"claim_ttl_seconds"`
	Archived            bool             `json:"archived,omitempty"`
	Columns             []boardColumnOut `json:"columns"`
	DoneTotal           int              `json:"done_total"`
	DoneShown           int              `json:"done_shown"`
}

type boardGetData struct {
	Projects []boardProjectOut `json:"projects"`
}

type boardGetOutput struct {
	OK    bool           `json:"ok"`
	Op    string         `json:"op"`
	Data  *boardGetData  `json:"data,omitempty"`
	Meta  *toolMeta      `json:"meta,omitempty"`
	Error *errorEnvelope `json:"error,omitempty"`
}

func boardGetTool() *gomcp.Tool {
	s := schemaFor[boardGetInput]()
	setEnum(prop(s, "view"), string(service.ViewTasks), string(service.ViewSummary))
	setDefault(prop(s, "done_limit"), 0)
	setMin(prop(s, "done_limit"), 0)
	setMax(prop(s, "done_limit"), float64(domain.MaxDoneLimit))
	inc := prop(s, "include")
	setEnum(inc.Items, includeEnumValues...)
	setDefault(inc, []string{})
	setEnum(prop(s, "format"), "compact", "json")
	setDefault(prop(s, "format"), "compact")

	filter := prop(s, "filter")
	setEnum(prop(filter, "types"), typeNames()...)
	setEnum(prop(filter, "priority_min"), priorityNames()...)
	setEnum(prop(filter, "claimed"), "any", "mine", "unclaimed", "other")
	setMaxItems(prop(filter, "tags"), domain.MaxTags)
	setMaxLen(prop(filter, "tags").Items, domain.MaxTagLen)

	return &gomcp.Tool{
		Name:        opBoardGet,
		Description: "Read the board. Compact text by default — about 1,000 tokens for 30 active tasks, roughly 90% smaller than the same board as indented JSON, so it is cheap enough to call at the start of every session. structuredContent is always full JSON.",
		InputSchema: s,
	}
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
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in boardGetInput) (*gomcp.CallToolResult, boardGetOutput, error) {
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

		board, err := svc.BoardGet(ctx, actor, service.BoardGetInput{
			ProjectKey: in.Project,
			View:       view,
			DoneLimit:  in.DoneLimit,
			Filter:     filter,
			Include:    toIncludes(in.Include),
		})
		if err != nil {
			derr := asDomainError(err)
			return errorResult(opBoardGet, derr), boardGetOutput{OK: false, Op: opBoardGet, Error: newErrorEnvelope(derr)}, nil
		}

		// board_get returns whatever `include` selected, whole: it has no
		// bounded tier, so the projection is the include set as-is.
		proj := service.FullProjection(toIncludes(in.Include))
		data := boardGetData{Projects: make([]boardProjectOut, len(board.Projects))}
		total := 0
		for i := range board.Projects {
			p := &board.Projects[i]
			cols := make([]boardColumnOut, len(p.Columns))
			for j := range p.Columns {
				c := &p.Columns[j]
				cols[j] = boardColumnOut{
					Name:     c.Name,
					Kind:     c.Kind,
					WIPLimit: c.WIPLimit,
					Count:    c.Count,
					Tasks:    taskViewOutList(c.Tasks, proj),
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
