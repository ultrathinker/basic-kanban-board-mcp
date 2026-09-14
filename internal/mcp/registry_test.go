package mcp

import (
	"context"
	"sort"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// The registry in registry.go feeds board_guide, the documentation generator
// and the schema-error remediation. None of them touches gomcp.AddTool, so
// none of them can notice a tool that was registered on the server and never
// added to the registry — or the reverse.
//
// That gap was real. A review canary added a tool to the server, left the
// documentation generator's own list alone, and the entire suite stayed green
// while docs/MCP-TOOLS.md described twelve tools and the server served
// thirteen, with progress_history missing from the document altogether. The
// only thing that caught it was a hardcoded "The 13 Tools" literal in a test
// — a literal that is itself the thing KANB-42 forbids, and that points the
// wrong way: it fails when the registry legitimately grows.
//
// This test closes the gap at its root. It boots a real server, asks it what
// it publishes, and compares that against the registry by NAME, so a mismatch
// says which tool is on which side.
// ---------------------------------------------------------------------------

func TestRegistryMatchesTheRunningServer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		newSrv    func(service.Service, string) *gomcp.Server
		factories []func() *gomcp.Tool
	}{
		{"full", NewServer, fullToolFactories},
		{"read-only", NewReadOnlyServer, readOnlyToolFactories},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cs, _ := roundtripServer(t, tc.newSrv)
			listed, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}

			served := make([]string, 0, len(listed.Tools))
			for _, tl := range listed.Tools {
				served = append(served, tl.Name)
			}
			registered := toolNames(tc.factories)
			sort.Strings(served)
			sort.Strings(registered)

			inRegistry := make(map[string]bool, len(registered))
			for _, n := range registered {
				inRegistry[n] = true
			}
			isServed := make(map[string]bool, len(served))
			for _, n := range served {
				isServed[n] = true
			}

			for _, n := range served {
				if !inRegistry[n] {
					t.Errorf("%s server publishes %q but registry.go does not list it: "+
						"board_guide, docs/MCP-TOOLS.md and the schema remediation are all blind to this tool",
						tc.name, n)
				}
			}
			for _, n := range registered {
				if !isServed[n] {
					t.Errorf("%s registry lists %q but the server never registers it: "+
						"the documentation describes a tool no client can call",
						tc.name, n)
				}
			}
			if len(served) != len(registered) {
				t.Errorf("%s: server publishes %d tools, registry lists %d", tc.name, len(served), len(registered))
			}
		})
	}
}

// TestRegistryIsTheOnlyToolList guards the property the fix is really about:
// the count exists in exactly one place. Every consumer derives from it, so
// this reads the registry once and checks the derived surfaces agree — no
// literal anywhere for a future contributor to update by hand and get wrong.
func TestRegistryIsTheOnlyToolList(t *testing.T) {
	t.Parallel()

	if len(fullToolFactories) == 0 || len(readOnlyToolFactories) == 0 {
		t.Fatal("a registry slice is empty; every derived surface would silently describe nothing")
	}
	if len(readOnlyToolFactories) >= len(fullToolFactories) {
		t.Errorf("read-only surface (%d) is not smaller than the full one (%d); mutating tools may have leaked in",
			len(readOnlyToolFactories), len(fullToolFactories))
	}

	// The schema-error remediation map is built from the same registry, so
	// every published tool must have an entry. A tool missing here degrades
	// to a generic error message instead of one naming its required fields.
	for _, name := range toolNames(fullToolFactories) {
		if toolInputSchemas[name] == nil {
			t.Errorf("tool %q has no input schema in the remediation map", name)
		}
	}

	// Read-only is a subset of full: a tool served only on /mcp/readonly
	// would be undocumented in the main tool reference.
	full := make(map[string]bool, len(fullToolFactories))
	for _, n := range toolNames(fullToolFactories) {
		full[n] = true
	}
	for _, n := range toolNames(readOnlyToolFactories) {
		if !full[n] {
			t.Errorf("read-only tool %q is absent from the full registry", n)
		}
	}
}
