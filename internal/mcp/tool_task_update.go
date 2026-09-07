package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opTaskUpdate = "task_update"

// acceptanceIn mirrors service-side domain.AcceptanceItem at the wire layer.
// It deliberately accepts both a bare JSON string and the {text, done} object
// form: agents that hand-craft task_update payloads otherwise have to learn
// task_create's shape and this tool's shape separately, and the asymmetry
// shows up as silent drops. The string form is sugar for {text, done:false}.
type acceptanceIn struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// UnmarshalJSON accepts a bare JSON string ("buy milk") as the unchecked
// form {text:"buy milk", done:false}. The raw-alias trick avoids the obvious
// infinite recursion that a naive `json.Unmarshal(b, a)` would cause.
func (a *acceptanceIn) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		a.Text, a.Done = s, false
		return nil
	}
	type raw acceptanceIn
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*a = acceptanceIn(r)
	return nil
}

// taskPatchIn is deliberately the richest input struct in the package,
// mirroring service.TaskPatch (PLAN §6.5: "consolidating mutations into
// parameters keeps the tool count low"). Estimate, Assignee, DueAt and
// Parent are three-state (absent / set / explicit null to clear); Go's
// encoding/json collapses "absent" and "explicit null" into the same nil
// pointer, so this struct only carries the "set" value and
// decodeRawPatches below re-parses the raw request JSON to tell the other
// two apart.
type taskPatchIn struct {
	Key       string   `json:"key" jsonschema:"task key, case-insensitive"`
	IfVersion *int     `json:"if_version,omitempty" jsonschema:"required for any replacement-style field below; commutative fields (note, tags_add, tags_remove) may omit it. Read the current version from board_get (every task line and the project header end with v<N>) or from task_get — no separate read is needed."`
	Title     *string  `json:"title,omitempty"`
	Body      *string  `json:"body,omitempty"`
	Type      *string  `json:"type,omitempty"`
	Priority  *string  `json:"priority,omitempty"`
	Estimate  *float64 `json:"estimate,omitempty" jsonschema:"send null to clear"`
	Assignee  *string  `json:"assignee,omitempty" jsonschema:"send null to clear"`
	DueAt     *string  `json:"due_at,omitempty" jsonschema:"RFC3339; send null to clear"`

	Tags       []string `json:"tags,omitempty" jsonschema:"replace the whole tag set; mutually exclusive with tags_add/tags_remove"`
	TagsAdd    []string `json:"tags_add,omitempty"`
	TagsRemove []string `json:"tags_remove,omitempty"`

	Column string  `json:"column,omitempty" jsonschema:"move to this column; validated (dependencies, WIP, strict_done) inside the transaction"`
	Rank   string  `json:"rank,omitempty" jsonschema:"reposition within the destination column"`
	Parent *string `json:"parent,omitempty" jsonschema:"reparent to this task key, or send null to clear; \"parent\" alone is a normal field, not the three-state trick — use JSON null to clear"`

	Acceptance      []acceptanceIn `json:"acceptance,omitempty" jsonschema:"replace the whole checklist; each item is either a plain string (unchecked) or {text, done}; mutually exclusive with acceptance_check/acceptance_add"`
	AcceptanceCheck []int          `json:"acceptance_check,omitempty" jsonschema:"tick items by index; requires if_version"`
	AcceptanceAdd   []string       `json:"acceptance_add,omitempty"`

	Note          string         `json:"note,omitempty" jsonschema:"appended as a note; never bumps version"`
	Focus         *bool          `json:"focus,omitempty" jsonschema:"true sets this task as the project's focus; false clears it only if this task holds it"`
	MetadataMerge map[string]any `json:"metadata_merge,omitempty" jsonschema:"shallow-merged; a null value deletes that key"`

	Force  bool   `json:"force,omitempty" jsonschema:"admin scope only: bypass a blocked/wip_exceeded/strict_done refusal"`
	Reason string `json:"reason,omitempty" jsonschema:"required with force:true; written to the event log"`
}

type taskUpdateInput struct {
	Patches []taskPatchIn `json:"patches"`
	Atomic  bool          `json:"atomic,omitempty" jsonschema:"false (default) = per-item results; true = the whole batch commits or none of it does"`
}

type taskPatchResultOut struct {
	Key   string         `json:"key"`
	OK    bool           `json:"ok"`
	Task  *taskOut       `json:"task,omitempty"`
	Error *errorEnvelope `json:"error,omitempty"`
}

type taskUpdateData struct {
	Items []taskPatchResultOut `json:"items"`
}

type taskUpdateOutput struct {
	OK    bool            `json:"ok"`
	Op    string          `json:"op"`
	Data  *taskUpdateData `json:"data,omitempty"`
	Meta  *toolMeta       `json:"meta,omitempty"`
	Error *errorEnvelope  `json:"error,omitempty"`
}

func taskUpdateTool() *gomcp.Tool {
	s := schemaFor[taskUpdateInput]()
	patches := prop(s, "patches")
	setMinItems(patches, 1)
	setMaxItems(patches, domain.MaxBatchTasks)

	item := patches.Items
	setEnum(prop(item, "type"), typeNames()...)
	setEnum(prop(item, "priority"), priorityNames()...)
	setEnum(prop(item, "rank"), "top", "bottom")
	setMaxLen(prop(item, "title"), domain.MaxTitleLen)
	setMaxLen(prop(item, "body"), domain.MaxBodyBytes)
	setMaxLen(prop(item, "assignee"), domain.MaxAssigneeLen)
	setMaxLen(prop(item, "note"), domain.MaxNoteBytes)
	tags := prop(item, "tags")
	setMaxItems(tags, domain.MaxTags)
	setMaxLen(tags.Items, domain.MaxTagLen)
	tagsAdd := prop(item, "tags_add")
	setMaxItems(tagsAdd, domain.MaxTags)
	setMaxLen(tagsAdd.Items, domain.MaxTagLen)
	tagsRemove := prop(item, "tags_remove")
	setMaxItems(tagsRemove, domain.MaxTags)
	setMaxLen(tagsRemove.Items, domain.MaxTagLen)
	acc := prop(item, "acceptance")
	setMaxItems(acc, domain.MaxAcceptance)
	// The bare-string form ("item is an unchecked line of text") is sugar
	// for {"text": <string>, "done": false}. acceptanceIn.UnmarshalJSON
	// handles both at decode time; the wire schema advertises the union.
	setAcceptanceItemShape(acc.Items, jsonschema.Ptr(domain.MaxAcceptanceText))
	accAdd := prop(item, "acceptance_add")
	setMaxItems(accAdd, domain.MaxAcceptance)
	setMaxLen(accAdd.Items, domain.MaxAcceptanceText)

	return &gomcp.Tool{
		Name: opTaskUpdate,
		Description: "Update one or more tasks. Per-item results by default (atomic:false); set atomic:true to make the whole batch commit or none of it does. " +
			"if_version is required for any replacement-style field. " +
			"Returns `data.items[]`: one `{key, ok, task|error}` entry per patch, in request order. " +
			versionEchoRule,
		InputSchema: s,
	}
}

// isJSONNull reports whether a raw JSON value is the literal null token.
func isJSONNull(r json.RawMessage) bool { return strings.TrimSpace(string(r)) == "null" }

// decodeRawPatches re-parses the tool call's raw arguments to recover, per
// patch and per field, whether a three-state field (estimate, assignee,
// due_at, parent) was absent, explicitly null, or given a value —
// distinctions Go's encoding/json throws away once it lands in a typed
// *T field (see taskPatchIn's doc comment).
func decodeRawPatches(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	var wrapper struct {
		Patches []map[string]json.RawMessage `json:"patches"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Patches, nil
}

func triFloatField(raw map[string]json.RawMessage, key string, val *float64) service.FieldFloat {
	r, present := raw[key]
	if !present {
		return service.FieldFloat{}
	}
	if isJSONNull(r) {
		return service.FieldFloat{Clear: true}
	}
	if val != nil {
		return service.FieldFloat{Set: true, Value: *val}
	}
	return service.FieldFloat{}
}

func triStringField(raw map[string]json.RawMessage, key string, val *string) service.FieldString {
	r, present := raw[key]
	if !present {
		return service.FieldString{}
	}
	if isJSONNull(r) {
		return service.FieldString{Clear: true}
	}
	if val != nil {
		return service.FieldString{Set: true, Value: *val}
	}
	return service.FieldString{}
}

func triTimeField(raw map[string]json.RawMessage, key, field string, val *string) (service.FieldTime, *domain.Error) {
	r, present := raw[key]
	if !present {
		return service.FieldTime{}, nil
	}
	if isJSONNull(r) {
		return service.FieldTime{Clear: true}, nil
	}
	if val != nil {
		t, derr := parseRFC3339(field, *val)
		if derr != nil {
			return service.FieldTime{}, derr
		}
		return service.FieldTime{Set: true, Value: t}, nil
	}
	return service.FieldTime{}, nil
}

// triParentField recovers the three-state parent field for task_update.
// Unlike task_create, there is no creation batch here, so "@ref" symbolic
// references have nothing to resolve against — PLAN §6.5 says task_update's
// parent is `key|null`, never a ref. Normalize as a plain task key. The
// shape is identical to triStringField's: absent / explicit null / set,
// distinguished by re-parsing req.Params.Arguments.
func triParentField(raw map[string]json.RawMessage, val *string) (service.FieldString, *domain.Error) {
	r, present := raw["parent"]
	if !present {
		return service.FieldString{}, nil
	}
	if isJSONNull(r) {
		return service.FieldString{Clear: true}, nil
	}
	if val != nil {
		pk, err := domain.NormalizeTaskKey(*val)
		if err != nil {
			return service.FieldString{}, domain.AsError(err)
		}
		return service.FieldString{Set: true, Value: pk}, nil
	}
	return service.FieldString{}, nil
}

// taskPatchToService converts one wire patch into service.TaskPatch. It
// validates request *shape* only (key format, enum values, mutually
// exclusive fields) — every business rule (if_version requirement, WIP,
// dependencies, cycles) is the service's job per AGENTS.md house style.
func taskPatchToService(idx int, in taskPatchIn, raw map[string]json.RawMessage) (service.TaskPatch, *domain.Error) {
	nk, err := domain.NormalizeTaskKey(in.Key)
	if err != nil {
		return service.TaskPatch{}, domain.AsError(err)
	}
	if len(in.Tags) > 0 && (len(in.TagsAdd) > 0 || len(in.TagsRemove) > 0) {
		return service.TaskPatch{}, domain.Invalid(fmt.Sprintf("patches[%d].tags", idx),
			"tags cannot be combined with tags_add/tags_remove", "Send either a full replacement tag set, or add/remove deltas, not both.")
	}
	if len(in.Acceptance) > 0 && (len(in.AcceptanceCheck) > 0 || len(in.AcceptanceAdd) > 0) {
		return service.TaskPatch{}, domain.Invalid(fmt.Sprintf("patches[%d].acceptance", idx),
			"acceptance cannot be combined with acceptance_check/acceptance_add", "Send either a full replacement checklist, or check/add deltas, not both.")
	}

	out := service.TaskPatch{
		Key:             nk,
		IfVersion:       in.IfVersion,
		Title:           in.Title,
		Body:            in.Body,
		Tags:            in.Tags,
		TagsAdd:         in.TagsAdd,
		TagsRemove:      in.TagsRemove,
		Column:          in.Column,
		Rank:            in.Rank,
		AcceptanceCheck: in.AcceptanceCheck,
		AcceptanceAdd:   in.AcceptanceAdd,
		Note:            in.Note,
		Focus:           in.Focus,
		MetadataMerge:   in.MetadataMerge,
		Force:           in.Force,
		Reason:          in.Reason,
	}
	if in.Type != nil {
		t := domain.Type(*in.Type)
		out.Type = &t
	}
	if in.Priority != nil {
		p, derr := parsePriorityName(fmt.Sprintf("patches[%d].priority", idx), *in.Priority)
		if derr != nil {
			return service.TaskPatch{}, derr
		}
		out.Priority = &p
	}
	if len(in.Acceptance) > 0 {
		items := make([]domain.AcceptanceItem, len(in.Acceptance))
		for i, a := range in.Acceptance {
			items[i] = domain.AcceptanceItem{Text: a.Text, Done: a.Done}
		}
		out.Acceptance = items
	}

	out.Estimate = triFloatField(raw, "estimate", in.Estimate)
	out.Assignee = triStringField(raw, "assignee", in.Assignee)
	dueAt, derr := triTimeField(raw, "due_at", fmt.Sprintf("patches[%d].due_at", idx), in.DueAt)
	if derr != nil {
		return service.TaskPatch{}, derr
	}
	out.DueAt = dueAt
	parent, derr := triParentField(raw, in.Parent)
	if derr != nil {
		return service.TaskPatch{}, derr
	}
	out.Parent = parent

	return out, nil
}

func registerTaskUpdate(s *gomcp.Server, svc service.Service) {
	tool := taskUpdateTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in taskUpdateInput) (*gomcp.CallToolResult, taskUpdateOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opTaskUpdate, aerr), taskUpdateOutput{OK: false, Op: opTaskUpdate, Error: newErrorEnvelope(aerr)}, nil
		}

		rawPatches, rawErr := decodeRawPatches(req.Params.Arguments)
		if rawErr != nil {
			derr := domain.Invalid("patches", rawErr.Error(), "Send a well-formed JSON array of patch objects.")
			return errorResult(opTaskUpdate, derr), taskUpdateOutput{OK: false, Op: opTaskUpdate, Error: newErrorEnvelope(derr)}, nil
		}

		type slot struct {
			key  string
			err  *domain.Error
			task *taskOut
			ok   bool
		}
		slots := make([]slot, len(in.Patches))
		var toSend []service.TaskPatch
		var sendIdx []int
		for i, p := range in.Patches {
			var rm map[string]json.RawMessage
			if i < len(rawPatches) {
				rm = rawPatches[i]
			}
			sp, derr := taskPatchToService(i, p, rm)
			if derr != nil {
				slots[i] = slot{key: p.Key, err: derr}
				continue
			}
			slots[i] = slot{key: sp.Key}
			toSend = append(toSend, sp)
			sendIdx = append(sendIdx, i)
		}

		// Atomic: a request-shape error anywhere fails the whole call before
		// ever reaching the service, matching task_create's all-or-nothing
		// contract for the same class of failure.
		if in.Atomic {
			for _, sl := range slots {
				if sl.err != nil {
					return errorResult(opTaskUpdate, sl.err), taskUpdateOutput{OK: false, Op: opTaskUpdate, Error: newErrorEnvelope(sl.err)}, nil
				}
			}
		}

		if len(toSend) > 0 {
			res, err := svc.TaskUpdate(ctx, actor, service.TaskUpdateInput{Patches: toSend, Atomic: in.Atomic})
			if err != nil {
				derr := asDomainError(err)
				return errorResult(opTaskUpdate, derr), taskUpdateOutput{OK: false, Op: opTaskUpdate, Error: newErrorEnvelope(derr)}, nil
			}
			includes := service.Includes{service.IncludeBody, service.IncludeAcceptance}
			for j, ir := range res.Items {
				if j >= len(sendIdx) {
					break
				}
				i := sendIdx[j]
				if ir.OK {
					out := taskViewOut(ir.Task, service.FullProjection(includes))
					slots[i] = slot{key: ir.Key, ok: true, task: &out}
				} else {
					slots[i] = slot{key: ir.Key, err: ir.Err}
				}
			}
		}

		items := make([]taskPatchResultOut, len(slots))
		warnings := []string{}
		for i, sl := range slots {
			if sl.ok {
				items[i] = taskPatchResultOut{Key: sl.key, OK: true, Task: sl.task}
			} else {
				items[i] = taskPatchResultOut{Key: sl.key, OK: false, Error: newErrorEnvelope(sl.err)}
				warnings = append(warnings, fmt.Sprintf("%s: %s", sl.key, sl.err.Code))
			}
		}

		out := taskUpdateOutput{
			OK: true, Op: opTaskUpdate,
			Data: &taskUpdateData{Items: items},
			Meta: &toolMeta{Count: len(items), Warnings: warnings},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
