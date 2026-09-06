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
	Name     string `json:"name" jsonschema:"unique within the project"`
	Kind     string `json:"kind"`
	WIPLimit *int   `json:"wip_limit,omitempty" jsonschema:"meaningful only for kind:active"`
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
}

type projectUpsertInput struct {
	Mode          string             `json:"mode" jsonschema:"REQUIRED, no default. \"create\" makes a new project (name is then required too); \"update\" edits an existing one (if_version is then required too). There is deliberately no upsert-by-guess: a mistyped key would silently fork the board into a second project."`
	Key           string             `json:"key" jsonschema:"project key, case-insensitive"`
	Name          string             `json:"name,omitempty" jsonschema:"display name; required when mode:create"`
	Description   *string            `json:"description,omitempty"`
	IfVersion     *int               `json:"if_version,omitempty" jsonschema:"required when mode:update — the project version your last read returned"`
	Columns       []columnSpecIn     `json:"columns,omitempty" jsonschema:"the full desired column list, in order, when provided"`
	RemoveColumns []removeColumnIn   `json:"remove_columns,omitempty" jsonschema:"required for every existing column absent from columns"`
	Settings      *projectSettingsIn `json:"settings,omitempty"`
	Archived      *bool              `json:"archived,omitempty"`
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
	cols := prop(s, "columns")
	setMaxItems(cols, domain.MaxColumnsPerPrj)
	item := cols.Items
	setMaxLen(prop(item, "name"), domain.MaxColumnNameLen)
	setEnum(prop(item, "kind"), string(domain.KindBacklog), string(domain.KindActive), string(domain.KindDone))
	rmCols := prop(s, "remove_columns")
	setMaxItems(rmCols, domain.MaxColumnsPerPrj)
	setMaxLen(prop(rmCols.Items, "name"), domain.MaxColumnNameLen)

	return &gomcp.Tool{
		Name:        opProjectUpsert,
		Description: projectUpsertDescription,
		InputSchema: s,
	}
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
	"Not sure which one applies? Call board_get with no `project` first: every project you can reach comes back with its key and version."

// defaultColumnSummary renders domain.DefaultColumns the way the tool
// description quotes them, e.g. `Backlog, Doing (WIP 3), Review, Done`.
func defaultColumnSummary() string {
	parts := make([]string, 0, len(domain.DefaultColumns))
	for _, c := range domain.DefaultColumns {
		s := c.Name
		if c.WIPLimit != nil {
			s += fmt.Sprintf(" (WIP %d)", *c.WIPLimit)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func columnSpecsToService(cols []columnSpecIn) []service.ColumnSpec {
	if len(cols) == 0 {
		return nil
	}
	out := make([]service.ColumnSpec, len(cols))
	for i, c := range cols {
		out[i] = service.ColumnSpec{Name: c.Name, Kind: domain.Kind(c.Kind), WIPLimit: c.WIPLimit}
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
	}
}

func registerProjectUpsert(s *gomcp.Server, svc service.Service) {
	tool := projectUpsertTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in projectUpsertInput) (*gomcp.CallToolResult, projectUpsertOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opProjectUpsert, aerr), projectUpsertOutput{OK: false, Op: opProjectUpsert, Error: newErrorEnvelope(aerr)}, nil
		}

		res, err := svc.ProjectUpsert(ctx, actor, service.ProjectUpsertInput{
			Mode:          service.UpsertMode(in.Mode),
			Key:           domain.NormalizeProjectKey(in.Key),
			Name:          in.Name,
			Description:   in.Description,
			IfVersion:     in.IfVersion,
			Columns:       columnSpecsToService(in.Columns),
			RemoveColumns: removeColumnsToService(in.RemoveColumns),
			Settings:      settingsToService(in.Settings),
			Archived:      in.Archived,
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
