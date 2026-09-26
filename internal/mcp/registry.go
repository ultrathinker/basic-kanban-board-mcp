package mcp

import (
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// THE tool registry. One list per server variant, and nothing else in this
// package may keep a second one.
//
// This file exists because the alternative already failed in production. The
// docs said "nine tools" in ten places while the server shipped twelve, and
// KANB-42 was written to stop that recurring. The first implementation of
// that card left the perimeter in FOUR hand-written copies — the registerX
// calls in NewServer, a slice feeding board_guide, the same again for the
// read-only server, and a private pair in docgen.go. Only one of those pairs
// was reconciled by a test; a review canary added a tool to the server, left
// docgen.go alone, and the whole suite stayed green while the committed
// documentation described a server that no longer existed.
//
// So: board_guide reads these slices, the documentation generator reads these
// slices, and TestRegistryMatchesTheRunningServer reconciles them against the
// tools a real server actually advertises. Adding a tool means adding its
// registerX call and its factory here; forget either and that test goes red
// naming the tool.
// ---------------------------------------------------------------------------

// fullToolFactories is every tool published on the full /mcp server.
var fullToolFactories = []func() *gomcp.Tool{
	boardGetTool,
	taskNextTool,
	taskGetTool,
	taskCreateTool,
	taskUpdateTool,
	taskLinkTool,
	taskClaimTool,
	taskRemoveTool,
	projectUpsertTool,
	projectPostTool,
	progressSetTool,
	progressHistoryTool,
	executorKeyIssueTool,
	boardGuideTool,
}

// readOnlyToolFactories is the shorter surface published on /mcp/readonly.
// Mutating tools never appear there (PLAN §6 rule 4).
var readOnlyToolFactories = []func() *gomcp.Tool{
	boardGetTool,
	taskGetTool,
	taskNextTool,
	progressHistoryTool,
	boardGuideTool,
}

// toolNames lists the published names of a registry slice. Callers that need
// to compare a registry against a live server use this rather than rebuilding
// the loop, so both sides of the comparison read the same field.
func toolNames(factories []func() *gomcp.Tool) []string {
	out := make([]string, 0, len(factories))
	for _, f := range factories {
		out = append(out, f().Name)
	}
	return out
}
