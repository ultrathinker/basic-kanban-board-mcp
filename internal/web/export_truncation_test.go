package web

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// ---------------------------------------------------------------------------
// KANB-29: the web export must not lose done tasks silently.
//
// The board read behind the export caps done tasks at domain.MaxDoneLimit.
// The CLI path already owned up to that -- a "truncated" block in the
// document plus a warning line on stderr -- while the web path produced a
// short document and said nothing, which is the exact failure the card was
// filed against, only on the other surface.
// ---------------------------------------------------------------------------

// truncatingService hands back one project whose done column did not fit.
// DoneShown/DoneTotal are board_get's own accounting, so the export does not
// have to re-count anything to know a gap exists.
type truncatingService struct {
	stubService
	doneShown int
	doneTotal int
}

func (s truncatingService) BoardGet(context.Context, service.Actor, service.BoardGetInput) (*service.Board, error) {
	return &service.Board{Projects: []service.BoardProject{{
		Key:       "BMB",
		Name:      "Truncation",
		DoneShown: s.doneShown,
		DoneTotal: s.doneTotal,
	}}}, nil
}

func (truncatingService) ProgressHistory(context.Context, service.Actor, service.ProgressHistoryInput) (*service.ProgressHistoryResult, error) {
	return &service.ProgressHistoryResult{}, nil
}

func (truncatingService) ChatList(context.Context, service.Actor, service.ChatListInput) (*service.ChatListResult, error) {
	return &service.ChatListResult{}, nil
}

// TestWebExport_NamesTheDoneLimit: when the board hands back fewer done
// tasks than it has, the downloaded document says so, in the same shape and
// with the same numbers the CLI document uses.
func TestWebExport_NamesTheDoneLimit(t *testing.T) {
	t.Parallel()
	svc := truncatingService{doneShown: 200, doneTotal: 512}

	doc, err := buildWebExportDoc(context.Background(), svc, service.Actor{Name: "alex"}, "BMB")
	if err != nil {
		t.Fatalf("buildWebExportDoc: %v", err)
	}
	if len(doc.Truncated) != 1 {
		t.Fatalf("truncated = %+v, want exactly one notice", doc.Truncated)
	}
	got := doc.Truncated[0]
	if got.Project != "BMB" || got.Included != 200 || got.Total != 512 {
		t.Errorf("truncated[0] = %+v, want {BMB 200 512}", got)
	}

	// The importer reads JSON, not the Go struct, so assert the wire form:
	// the field names have to be the ones cmd/kanban already writes.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Truncated []struct {
			Project  string `json:"project"`
			Included int    `json:"included"`
			Total    int    `json:"total"`
		} `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Truncated) != 1 || wire.Truncated[0].Project != "BMB" ||
		wire.Truncated[0].Included != 200 || wire.Truncated[0].Total != 512 {
		t.Errorf("wire truncated = %+v, want one {BMB 200 512} entry", wire.Truncated)
	}
}

// TestWebExport_NoNoticeWhenNothingWasDropped keeps the notice honest in the
// other direction: a complete export must not carry a "truncated" key at
// all, or every download would read as lossy and the real warning would stop
// meaning anything.
func TestWebExport_NoNoticeWhenNothingWasDropped(t *testing.T) {
	t.Parallel()
	svc := truncatingService{doneShown: 7, doneTotal: 7}

	doc, err := buildWebExportDoc(context.Background(), svc, service.Actor{Name: "alex"}, "BMB")
	if err != nil {
		t.Fatalf("buildWebExportDoc: %v", err)
	}
	if len(doc.Truncated) != 0 {
		t.Fatalf("truncated = %+v, want none", doc.Truncated)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := wire["truncated"]; present {
		t.Errorf("a complete export must omit \"truncated\" entirely, got %s", raw)
	}
}
