package mcp

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateDoc is the golden-test lever for docs/MCP-TOOLS.md: a test that
// fails on drift, but a contributor who actually changed the registry
// can rerun with -update and commit the new content. The default
// behaviour is the gate; -update is the only way to land a regeneration.
var updateDoc = flag.Bool("update-doc", false, "rewrite docs/MCP-TOOLS.md to match the current registry")

// TestDocGen_MatchesCommittedGolden is KANB-43's "the test goes red on
// divergence": it regenerates docs/MCP-TOOLS.md from the registry in
// registry.go and compares byte-for-byte to the committed file. Change the
// registry without regenerating and this fails.
//
// What it does NOT catch, and an earlier version of this comment wrongly
// claimed it did: a tool registered on the server but absent from the
// registry. The generator reads only the registry, so the rendered document
// would match the committed one and this test would pass while the server
// served a tool no document mentions. A review canary proved exactly that.
// TestRegistryMatchesTheRunningServer is what closes it, by asking a live
// server what it publishes.
func TestDocGen_MatchesCommittedGolden(t *testing.T) {
	t.Parallel()
	got := GenerateMCPToolsDocs()

	// The repo layout puts the doc two directories up from this test
	// (internal/mcp -> internal -> repo root -> docs/MCP-TOOLS.md).
	// Resolving through a path anchored at the working directory is
	// brittle; instead, walk up from this file's directory until we
	// find docs/MCP-TOOLS.md. The file always exists once a previous
	// regeneration committed it, so a missing file is a real failure.
	path, err := findRepoDoc("MCP-TOOLS.md")
	if err != nil {
		t.Fatalf("locate docs/MCP-TOOLS.md: %v", err)
	}

	if *updateDoc {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("update %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run with -update-doc to create)", path, err)
	}
	if !bytes.Equal([]byte(got), want) {
		diffDoc(t, path, string(want), got)
	}
}

// findRepoDoc walks upward from this test file's directory looking for
// docs/<name>. It deliberately stops at the first docs/ directory it
// sees rather than guessing at depth — the package is in
// internal/mcp so we expect two hops (../..).
func findRepoDoc(name string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		candidate := filepath.Join(dir, "docs", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// diffDoc fails the test with a focused view of the first line where
// generation and committed content disagree. Following the pattern of
// compact_test.go's diffGolden, the first divergent line and both
// versions are printed, with a small window around the disagreement so
// the regression is obvious in CI output.
func diffDoc(t *testing.T, path, want, got string) {
	t.Helper()
	wlines := strings.Split(want, "\n")
	glines := strings.Split(got, "\n")
	n := len(wlines)
	if len(glines) > n {
		n = len(glines)
	}
	for i := 0; i < n; i++ {
		var wl, gl string
		if i < len(wlines) {
			wl = wlines[i]
		}
		if i < len(glines) {
			gl = glines[i]
		}
		if wl != gl {
			t.Errorf("docs/MCP-TOOLS.md diverges from generator at line %d:\n  committed: %q\n  generated: %q", i+1, wl, gl)
			const window = 5
			from := i - window
			if from < 0 {
				from = 0
			}
			to := i + window + 1
			if to > n {
				to = n
			}
			t.Logf("--- committed lines %d..%d ---", from+1, to)
			for j := from; j < to && j < len(wlines); j++ {
				t.Logf("  committed:%4d: %s", j+1, wlines[j])
			}
			t.Logf("--- generated lines %d..%d ---", from+1, to)
			for j := from; j < to && j < len(glines); j++ {
				t.Logf("  generated:%4d: %s", j+1, glines[j])
			}
			return
		}
	}
	if len(wlines) != len(glines) {
		t.Errorf("docs/MCP-TOOLS.md length diverges from generator: committed=%d lines, generated=%d lines",
			len(wlines), len(glines))
	}
}

// TestDocGen_ToolCountMatchesRegistry checks the Quick Reference header
// carries the count the registry actually holds — without naming that count.
//
// It used to read `wantPrefix := "## Quick Reference: The 13 Tools"`. That
// literal was the only thing catching registry drift, and it caught it by
// accident: it fails just as loudly when the surface legitimately grows to
// fourteen, reporting "count drifted from registry" when the registry is the
// one thing that is right. A count written down in the test is the same
// defect as a count written down in the doc, which is what KANB-42 is about.
func TestDocGen_ToolCountMatchesRegistry(t *testing.T) {
	t.Parallel()
	got := GenerateMCPToolsDocs()
	want := fmt.Sprintf("## Quick Reference: The %d Tools", len(fullToolFactories))
	if !strings.Contains(got, want) {
		t.Errorf("doc quick-reference header does not match the registry: want %q in output", want)
	}
	// Any OTHER count in that header means the generator and the registry
	// disagree — including a stale one left behind by a partial edit.
	for n := 1; n <= len(fullToolFactories)+5; n++ {
		if n == len(fullToolFactories) {
			continue
		}
		if stale := fmt.Sprintf("## Quick Reference: The %d Tools", n); strings.Contains(got, stale) {
			t.Errorf("doc carries %q alongside the registry count of %d", stale, len(fullToolFactories))
		}
	}
}

// TestDocGen_NoNineToolsInDoc is KANB-43 criterion #3 applied to the
// generated doc itself. The same drift discipline that forbids "nine
// tools" prose in the static parts also forbids it in the dynamic
// parts: nothing in the generator emits "nine tools", and a future
// contributor who tries will see this test go red.
func TestDocGen_NoNineToolsInDoc(t *testing.T) {
	t.Parallel()
	got := GenerateMCPToolsDocs()
	for _, banned := range []string{"nine tools", "Nine Tools", "nine MCP"} {
		if strings.Contains(got, banned) {
			t.Errorf("doc still contains %q; remove or replace with the registry-derived count", banned)
		}
	}
}

// TestDocGen_ExampleCallsIncludeRequiredFields checks that the auto-
// generated Example Call for each tool names every required property,
// so an agent reading the doc has the keys it must send. Optional
// properties are not included; that is a deliberate choice (the doc
// stays terse) and the test pins it.
func TestDocGen_ExampleCallsIncludeRequiredFields(t *testing.T) {
	t.Parallel()
	got := GenerateMCPToolsDocs()

	// Walk every fullToolFactories tool: pull its schema's
	// required list and verify the example block contains the literal
	// "key": token for each one. A schema that declares "foo" as
	// required must produce an example whose "foo": appears in the
	// generated text — otherwise the doc would teach a request that
	// the schema would refuse.
	for _, f := range fullToolFactories {
		tool := f()
		required := schemaRequiredSet(tool.InputSchema)
		if len(required) == 0 {
			continue
		}
		for name := range required {
			token := `"` + name + `": <value>`
			if !strings.Contains(got, token) {
				t.Errorf("tool %s: example does not include required key %q (token %q)", tool.Name, name, token)
			}
		}
	}
}
