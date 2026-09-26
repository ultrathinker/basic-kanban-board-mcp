package mcp

import (
	"context"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opProjectUpsert = "project_upsert"

type columnSpecIn struct {
	Name string `json:"name" jsonschema:"unique within the project"`
	Kind string `json:"kind"`
}

type removeColumnIn struct {
	Name        string `json:"name"`
	MoveTasksTo string `json:"move_tasks_to" jsonschema:"destination column for any task currently in the removed column"`
}

type projectSettingsIn struct {
	EstimateUnit        *string `json:"estimate_unit,omitempty" jsonschema:"e.g. \"h\" or \"d\""`
	EnforceDependencies *bool   `json:"enforce_dependencies,omitempty"`
	StrictDone          *bool   `json:"strict_done,omitempty"`
	ClaimTTLSeconds     *int    `json:"claim_ttl_seconds,omitempty"`
	// IdleAfterSeconds is the attention-line threshold (KANB-67). Its bounds
	// are published on the schema from the same domain constants the service
	// enforces, so the range is learned from tools/list, not from a refusal.
	IdleAfterSeconds *int `json:"idle_after_seconds,omitempty" jsonschema:"how long a card in an active column may go without movement before board_get's attention line names it; 0 clears it back to claim_ttl_seconds; absence leaves it unchanged"`
	// Coordinator is the tokens.id of the project's coordinator, or an empty
	// string to clear the appointment (KANB-44). The id — never the display
	// name — is the contract: it survives secret rotation, a name does not.
	Coordinator *string `json:"coordinator,omitempty" jsonschema:"tokens.id of the coordinator (read participants from board_get), or an empty string to clear the appointment; absence leaves it unchanged"`
}

type projectUpsertInput struct {
	Mode        string  `json:"mode" jsonschema:"REQUIRED, no default. \"create\" makes a new project (name is then required too); \"update\" edits an existing one (if_version is then required too). There is deliberately no upsert-by-guess: a mistyped key would silently fork the board into a second project."`
	Key         string  `json:"key" jsonschema:"project key, case-insensitive"`
	Name        string  `json:"name,omitempty" jsonschema:"display name; required when mode:create"`
	Description *string `json:"description,omitempty"`
	// DescriptionAppend mirrors task_update's body_append: same field
	// naming pattern, same mutual exclusion with the full-replacement
	// field, same "not both" wording.
	DescriptionAppend *string            `json:"description_append,omitempty" jsonschema:"append this text to the description instead of replacing it; mutually exclusive with description"`
	IfVersion         *int               `json:"if_version,omitempty" jsonschema:"required when mode:update — the project version your last read returned"`
	Columns           []columnSpecIn     `json:"columns,omitempty" jsonschema:"the full desired column list, in order, when provided"`
	RemoveColumns     []removeColumnIn   `json:"remove_columns,omitempty" jsonschema:"required for every existing column absent from columns"`
	Settings          *projectSettingsIn `json:"settings,omitempty"`
	Archived          *bool              `json:"archived,omitempty"`
}

type projectUpsertOutput struct {
	OK    bool           `json:"ok"`
	Op    string         `json:"op"`
	Data  *projectOut    `json:"data,omitempty"`
	Meta  *toolMeta      `json:"meta,omitempty"`
	Error *errorEnvelope `json:"error,omitempty"`
}

func projectUpsertTool() *gomcp.Tool {
	s := schemaFor[projectUpsertInput]()
	setEnum(prop(s, "mode"), string(service.UpsertCreate), string(service.UpsertUpdate))
	setMinLen(prop(s, "key"), domain.MinProjectKeyLen)
	setMaxLen(prop(s, "key"), domain.MaxProjectKeyLen)
	// Both description writers carry the same ceiling the service enforces,
	// so an agent learns the bound from tools/list instead of from a
	// rejection — and so the replacement path cannot store a value that
	// would make every later append impossible.
	setMaxLen(prop(s, "description"), domain.MaxBodyBytes)
	setMaxLen(prop(s, "description_append"), domain.MaxBodyBytes)
	cols := prop(s, "columns")
	setMaxItems(cols, domain.MaxColumnsPerPrj)
	item := cols.Items
	setMaxLen(prop(item, "name"), domain.MaxColumnNameLen)
	setEnum(prop(item, "kind"), columnKindNames()...)
	rmCols := prop(s, "remove_columns")
	setMaxItems(rmCols, domain.MaxColumnsPerPrj)
	setMaxLen(prop(rmCols.Items, "name"), domain.MaxColumnNameLen)
	// 0 is the documented "clear", so the published minimum is 0 and the
	// floor above it is stated in the description; a schema cannot express
	// "0 or at least N" without a oneOf that clients render poorly.
	idle := prop(prop(s, "settings"), "idle_after_seconds")
	setMin(idle, 0)
	setMax(idle, domain.IdleAfterMax.Seconds())
	idle.Description += fmt.Sprintf("; otherwise %d (%d minutes) to %d (%d days) — out of range is refused, not clamped",
		int(domain.IdleAfterMin.Seconds()), int(domain.IdleAfterMin.Minutes()),
		int(domain.IdleAfterMax.Seconds()), int(domain.IdleAfterMax.Hours()/24))

	return &gomcp.Tool{
		Name:        opProjectUpsert,
		Description: projectUpsertDescription,
		InputSchema: s,
	}
}

// columnKindNames is the column kind enum published in schemas, built from
// domain.AllKinds so the wire enum cannot drift from the internal one — the
// same discipline messageKindNames applies for chat message kinds. A
// hand-typed list here is exactly the disease this project already caught
// once with its tool count ("nine tools" in prose, twelve for real): add a
// kind to domain.AllKinds and forget this list, and the new kind is valid
// everywhere except the one surface that would let a caller actually set it.
func columnKindNames() []string {
	names := make([]string, len(domain.AllKinds))
	for i, k := range domain.AllKinds {
		names[i] = string(k)
	}
	return names
}

// projectUpsertDescription spells out both calls in full. "upsert" invites
// the caller to omit `mode` and let the server work it out; PLAN §6.9 refuses
// that on purpose, because a mistyped key would then quietly create a second
// project instead of editing the intended one. Since the tool cannot infer
// the intent, the schema has to teach it: an agent should get the call right
// from tools/list alone, without spending a round trip on a rejection.
//
// The default column list is read from domain.DefaultColumns rather than
// typed out here, so it can never drift from what create actually produces.
var projectUpsertDescription = "Create or update one project: identity, columns and settings. " +
	"`mode` is REQUIRED and has no default — despite the name this tool never guesses create-vs-update, " +
	"because a typo in a project key must not silently fork the board into a second project.\n" +
	"Create: {\"mode\":\"create\",\"key\":\"TEST\",\"name\":\"Smoke Test\"} — `name` is required; " +
	"omitting `columns` gives the default set (" + defaultColumnSummary() + ").\n" +
	"Update: {\"mode\":\"update\",\"key\":\"TEST\",\"if_version\":3,\"name\":\"New name\"} — `if_version` is required " +
	"and is the version your last read of the project returned.\n" +
	"`settings.idle_after_seconds` sets how long a card in an active column may sit without movement before board_get's attention line names it " +
	"(e.g. 172800 for a review cycle measured in days); unset or 0, the threshold is `claim_ttl_seconds`. How long a lease lasts and when a card looks abandoned are separate settings.\n" +
	"Not sure which one applies? Call board_get with no `project` first: every project you can reach comes back with its key and version."

// defaultColumnSummary renders domain.DefaultColumns the way the tool
// description quotes them, e.g. `Backlog, Doing, Done`.
func defaultColumnSummary() string {
	parts := make([]string, 0, len(domain.DefaultColumns))
	for _, c := range domain.DefaultColumns {
		parts = append(parts, c.Name)
	}
	return strings.Join(parts, ", ")
}

func columnSpecsToService(cols []columnSpecIn) []service.ColumnSpec {
	if len(cols) == 0 {
		return nil
	}
	out := make([]service.ColumnSpec, len(cols))
	for i, c := range cols {
		out[i] = service.ColumnSpec{Name: c.Name, Kind: domain.Kind(c.Kind)}
	}
	return out
}

func removeColumnsToService(cols []removeColumnIn) []service.RemoveColumn {
	if len(cols) == 0 {
		return nil
	}
	out := make([]service.RemoveColumn, len(cols))
	for i, c := range cols {
		out[i] = service.RemoveColumn{Name: c.Name, MoveTasksTo: c.MoveTasksTo}
	}
	return out
}

func settingsToService(s *projectSettingsIn) *service.ProjectSettings {
	if s == nil {
		return nil
	}
	return &service.ProjectSettings{
		EstimateUnit:        s.EstimateUnit,
		EnforceDependencies: s.EnforceDependencies,
		StrictDone:          s.StrictDone,
		ClaimTTLSeconds:     s.ClaimTTLSeconds,
		IdleAfterSeconds:    s.IdleAfterSeconds,
		Coordinator:         s.Coordinator,
	}
}

func registerProjectUpsert(s *gomcp.Server, svc service.Service) {
	tool := projectUpsertTool()
	addTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in projectUpsertInput) (*gomcp.CallToolResult, projectUpsertOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opProjectUpsert, aerr), projectUpsertOutput{OK: false, Op: opProjectUpsert, Error: newErrorEnvelope(aerr)}, nil
		}

		res, err := svc.ProjectUpsert(ctx, actor, service.ProjectUpsertInput{
			Mode:              service.UpsertMode(in.Mode),
			Key:               domain.NormalizeProjectKey(in.Key),
			Name:              in.Name,
			Description:       in.Description,
			DescriptionAppend: in.DescriptionAppend,
			IfVersion:         in.IfVersion,
			Columns:           columnSpecsToService(in.Columns),
			RemoveColumns:     removeColumnsToService(in.RemoveColumns),
			Settings:          settingsToService(in.Settings),
			Archived:          in.Archived,
		})
		if err != nil {
			derr := asDomainError(err)
			return errorResult(opProjectUpsert, derr), projectUpsertOutput{OK: false, Op: opProjectUpsert, Error: newErrorEnvelope(derr)}, nil
		}

		data := projectOutFrom(res.Project, res.Columns)
		out := projectUpsertOutput{OK: true, Op: opProjectUpsert, Data: &data, Meta: &toolMeta{Count: len(res.Columns)}}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
