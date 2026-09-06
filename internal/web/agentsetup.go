package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/web/view"
)

// handleAgentSetup is "GET /agent-setup". It is intentionally reachable
// without a session: the footer links to it unconditionally, and a fresh
// operator following the startup banner needs it before they have signed in
// anywhere.
func (w *Web) handleAgentSetup(rw http.ResponseWriter, r *http.Request) {
	tok, _ := w.sessionFromRequest(r)
	base := strings.TrimRight(w.d.BaseURL, "/")
	if base == "" {
		base = "http://" + r.Host
	}
	tokenName := ""
	if tok != nil {
		tokenName = tok.Name
	}
	page := w.newPage(r.Context(), rw, r, tok, "agent-setup")
	page.Model = agentSetupModel(base, tokenName)
	w.render(rw, r, http.StatusOK, "page-agent-setup", page)
}

// agentSetupModel mirrors view.SampleAgentSetup's snippets against the real
// base URL (the sample fixture exists for the UI agent's preview tool; this
// package builds the same shape from live config).
func agentSetupModel(base, tokenName string) view.AgentSetupModel {
	cfg := func() string {
		return fmt.Sprintf(`{
  "mcpServers": {
    "kanban": {
      "type": "http",
      "url": %q,
      "headers": {
        "Authorization": "Bearer ${KANBAN_TOKEN}"
      }
    }
  }
}`, base+"/mcp")
	}
	return view.AgentSetupModel{
		BaseURL:   base,
		TokenName: tokenName,
		Snippets: []view.AgentSnippet{
			{
				ID: "claude", Title: "Claude Code",
				Description: "Drop into ~/.claude.json or paste into the project .mcp.json.",
				Config:      cfg(), Format: "json",
			},
			{
				ID: "codex", Title: "Codex CLI",
				Description: "Add to ~/.codex/config.toml under [mcp_servers.kanban].",
				Config: fmt.Sprintf(`[mcp_servers.kanban]
type = "http"
url = %q
headers = { "Authorization" = "Bearer ${KANBAN_TOKEN}" }`, base+"/mcp"),
				Format: "toml",
			},
			{
				ID: "cursor", Title: "Cursor",
				Description: "Cursor → Settings → MCP → Add new global MCP server.",
				Config:      cfg(), Format: "json",
			},
			{
				ID: "generic", Title: "Generic (streamable-HTTP)",
				Description: "Any MCP client that speaks streamable-HTTP over bearer auth.",
				Config:      fmt.Sprintf("POST %s/mcp\nAuthorization: Bearer ${KANBAN_TOKEN}\nContent-Type: application/json", base),
				Format:      "text",
			},
		},
	}
}
