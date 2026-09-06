package mcp

import (
	"context"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

const opTaskClaim = "task_claim"

type taskClaimInput struct {
	Key        string `json:"key" jsonschema:"task key, case-insensitive"`
	Action     string `json:"action" jsonschema:"claim = take a free/expired lease, renew = extend your own, release = give it up"`
	TTLSeconds int    `json:"ttl_seconds,omitempty" jsonschema:"lease duration in seconds; clamped to the supported range; 0 = project default"`
	Force      bool   `json:"force,omitempty" jsonschema:"admin scope only: steal a live lease held by someone else"`
}

type taskClaimData struct {
	Task                  taskOut `json:"task"`
	ClaimedBy             string  `json:"claimed_by,omitempty"`
	ClaimExpiresAt        *string `json:"claim_expires_at,omitempty"`
	LeaseRemainingSeconds int     `json:"lease_remaining_seconds,omitempty"`
}

type taskClaimOutput struct {
	OK    bool           `json:"ok"`
	Op    string         `json:"op"`
	Data  *taskClaimData `json:"data,omitempty"`
	Meta  *toolMeta      `json:"meta,omitempty"`
	Error *errorEnvelope `json:"error,omitempty"`
}

func taskClaimTool() *gomcp.Tool {
	s := schemaFor[taskClaimInput]()
	setEnum(prop(s, "action"), string(service.ClaimTake), string(service.ClaimRenew), string(service.ClaimRelease))
	setMin(prop(s, "ttl_seconds"), 0)
	setMax(prop(s, "ttl_seconds"), domain.ClaimTTLMax.Seconds())

	return &gomcp.Tool{
		Name:        opTaskClaim,
		Description: "Claim, renew or release the lease on a single task with an atomic compare-and-swap. Same actor never needs force; an expired former owner cannot renew after another actor won.",
		InputSchema: s,
	}
}

func registerTaskClaim(s *gomcp.Server, svc service.Service) {
	tool := taskClaimTool()
	gomcp.AddTool(s, tool, func(ctx context.Context, req *gomcp.CallToolRequest, in taskClaimInput) (*gomcp.CallToolResult, taskClaimOutput, error) {
		actor, aerr := actorFromContext(ctx)
		if aerr != nil {
			return errorResult(opTaskClaim, aerr), taskClaimOutput{OK: false, Op: opTaskClaim, Error: newErrorEnvelope(aerr)}, nil
		}

		key, err := domain.NormalizeTaskKey(in.Key)
		if err != nil {
			derr := domain.AsError(err)
			return errorResult(opTaskClaim, derr), taskClaimOutput{OK: false, Op: opTaskClaim, Error: newErrorEnvelope(derr)}, nil
		}

		res, svcErr := svc.TaskClaim(ctx, actor, service.TaskClaimInput{
			Key:        key,
			Action:     service.ClaimAction(in.Action),
			TTLSeconds: in.TTLSeconds,
			Force:      in.Force,
		})
		if svcErr != nil {
			derr := asDomainError(svcErr)
			return errorResult(opTaskClaim, derr), taskClaimOutput{OK: false, Op: opTaskClaim, Error: newErrorEnvelope(derr)}, nil
		}

		includes := service.Includes{service.IncludeBody, service.IncludeAcceptance}
		out := taskClaimOutput{
			OK: true, Op: opTaskClaim,
			Data: &taskClaimData{
				Task:                  taskViewOut(&res.Task, service.FullProjection(includes)),
				ClaimedBy:             res.ClaimedBy,
				ClaimExpiresAt:        formatTimePtr(res.ClaimExpiresAt),
				LeaseRemainingSeconds: res.RemainSeconds,
			},
		}
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: jsonText(out)}}}, out, nil
	})
}
