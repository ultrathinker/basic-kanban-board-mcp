package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// KANB-67: a project's idle threshold is a setting like claim_ttl_seconds,
// so a restored board must keep it — and a project that never set it must
// come back unset, not with some value import invented.
func TestExportImport_IdleAfterRoundTrips(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	ctx := context.Background()

	st, err := openStore(ctx, dir1)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	svc := service.New(st, nil)
	actor := service.Actor{
		Name:   "tester",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
	}
	const idleAfter = 172800
	for _, in := range []service.ProjectUpsertInput{
		{Mode: service.UpsertCreate, Key: "SLOW", Name: "Review takes days",
			Settings: &service.ProjectSettings{IdleAfterSeconds: intPtrLocal(idleAfter), ClaimTTLSeconds: intPtrLocal(1800)}},
		{Mode: service.UpsertCreate, Key: "PLAIN", Name: "Never set it"},
	} {
		if _, err := svc.ProjectUpsert(ctx, actor, in); err != nil {
			t.Fatalf("ProjectUpsert %s: %v", in.Key, err)
		}
	}
	st.Close()

	exportFile := filepath.Join(t.TempDir(), "export.json")
	if err := runExport([]string{"--data", dir1, "--out", exportFile}); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	if err := runImport([]string{"--data", dir2, "--in", exportFile}); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	restored := mustOpenStore(t, dir2)
	want := map[string][2]int{"SLOW": {idleAfter, 1800}, "PLAIN": {0, int(domain.ClaimTTLDefault.Seconds())}}
	if err := restored.Read(ctx, func(tx store.Tx) error {
		for key, w := range want {
			p, err := restored.Projects().GetByKey(tx, key)
			if err != nil {
				return err
			}
			if p.IdleAfterSeconds != w[0] || p.ClaimTTLSeconds != w[1] {
				t.Errorf("%s restored with idle_after=%d claim_ttl=%d, want %d and %d",
					key, p.IdleAfterSeconds, p.ClaimTTLSeconds, w[0], w[1])
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
