package web

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// ---------------------------------------------------------------------------
// KANB-70: the web export is the CLI export.
//
// The web Export button used to build its own, smaller document: done cards
// capped at board_get's limit, plus a "truncated" field the strict importer
// does not know, so the download of any mature project could not be restored
// at all. The document now comes from Service.Export, the builder `kanban
// export` uses, and nothing is capped.
// ---------------------------------------------------------------------------

// TestWebExport_DoneBeyondTheBoardCapIsCompleteAndImportable downloads a
// project holding more done cards than board_get ever returns and restores
// the download through the importer's own decoder and Service.Import.
func TestWebExport_DoneBeyondTheBoardCapIsCompleteAndImportable(t *testing.T) {
	env := newExportEnv(t)
	ctx := context.Background()
	admin := service.Actor{Name: "seed", Scopes: domain.Scopes{domain.ScopeAdmin}}

	if _, err := env.svc.ProjectUpsert(ctx, admin, service.ProjectUpsertInput{
		Mode: service.UpsertCreate, Key: "BMB", Name: "Done overflow",
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	doneCount := domain.MaxDoneLimit + 5
	for created := 0; created < doneCount; {
		batch := min(doneCount-created, domain.MaxBatchTasks)
		tasks := make([]service.NewTask, batch)
		for i := range tasks {
			tasks[i] = service.NewTask{ProjectKey: "BMB", Column: "Done", Title: fmt.Sprintf("done %d", created+i), Type: domain.TypeTask}
		}
		if _, err := env.svc.TaskCreate(ctx, admin, service.TaskCreateInput{Tasks: tasks}); err != nil {
			t.Fatalf("seed done tasks: %v", err)
		}
		created += batch
	}

	rec := env.postExport(t, "/p/BMB/export")
	if rec.Code != 200 {
		t.Fatalf("export status = %d, body %.300s", rec.Code, rec.Body.String())
	}
	doc, _, err := service.DecodeExportDocument(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("the importer refuses the web download: %v", err)
	}
	if got := countDone(doc.Projects); got != doneCount {
		t.Fatalf("web export carries %d done cards, want all %d", got, doneCount)
	}

	dst, err := store.Open(ctx, store.Config{Path: filepath.Join(t.TempDir(), "kanban.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	dstSvc := service.New(dst, nil)
	if _, err := dstSvc.Import(ctx, admin, &doc); err != nil {
		t.Fatalf("Import of the web download: %v", err)
	}
	back, err := dstSvc.BoardGet(ctx, admin, service.BoardGetInput{
		ProjectKey: "BMB", View: service.ViewTasks, DoneLimit: service.DoneLimitUnlimited,
	})
	if err != nil {
		t.Fatalf("BoardGet after import: %v", err)
	}
	if got := countDone(back.Projects); got != doneCount {
		t.Fatalf("restored project has %d done cards, want %d", got, doneCount)
	}
}

func countDone(projects []service.BoardProject) int {
	n := 0
	for _, p := range projects {
		for _, c := range p.Columns {
			if c.Kind == domain.KindDone {
				n += len(c.Tasks)
			}
		}
	}
	return n
}
