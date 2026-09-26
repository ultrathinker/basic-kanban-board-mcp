package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
)

// The export document and both halves of the round trip live in the service
// (service.ExportDocument, Service.Export, Service.Import), so `kanban
// export`, `kanban import` and the web Export buttons share one definition
// (KANB-70). These aliases keep the CLI's own spelling for its tests.
type (
	exportDocument     = service.ExportDocument
	exportProgressMark = service.ExportProgressMark
	exportChatMessage  = service.ExportChatMessage
	exportJournalEntry = service.ExportJournalEntry
)

// stripLegacyWIPLimits and decodeExportDocument are the two halves of
// service.DecodeExportDocument, kept addressable for the CLI's tests.
var (
	stripLegacyWIPLimits = service.StripLegacyWIPLimits
	decodeExportDocument = service.DecodeStrictExportDocument
)

// importBoard parses an export and restores it through Service.Import: one
// transaction, all or nothing, refused up front when a project of the file
// already exists. Everything worth knowing that is not an error — a dropped
// WIP limit, a coordinator whose token does not exist here — is written to
// stderr, where the person who ran the command is looking.
func importBoard(ctx context.Context, svc service.Service, actor service.Actor, raw []byte) (*service.ImportResult, error) {
	doc, dropped, err := service.DecodeExportDocument(raw)
	if err != nil {
		return nil, err
	}
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d column(s) in this export carry wip_limit; it was dropped on import because the board has no WIP limits since 26.09.2026\n", dropped)
	}
	res, err := svc.Import(ctx, actor, &doc)
	if err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	return res, nil
}
