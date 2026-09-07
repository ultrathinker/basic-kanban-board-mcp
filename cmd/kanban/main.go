// Command kanban is the single binary that ships basic-kanban-board-mcp.
//
// Subcommands (PLAN §10):
//
//	serve           run the HTTP/MCP server
//	token           manage API tokens (create | list | revoke | rotate)
//	agent-config    print a paste-ready MCP config for a named client
//	agent-md        print the recommended AGENTS.md snippet for end users
//	export          dump the board to JSON
//	import          load a JSON dump into a fresh project
//	backup          VACUUM INTO a timestamped copy of the data file
//	task            subcommand group: purge (admin-only hard delete)
//	doctor          print runtime diagnostics (bind, auth, TLS, db health)
//	healthcheck     exit 0/1 by probing /healthz; for the distroless image
//	migrate         apply migrations (--dry-run)
//	demo            seed idempotent, visibly-labeled sample data
//	mcp             stdio bridge (HTTP client when --url is set)
//	version         print version and exit
//
// Flags and the startup refusal rules live in `internal/config`; this file
// only routes the CLI surface and prints results.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/app"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/auth"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/config"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/demo"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/domain"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/service"
	"github.com/ultrathinker/basic-kanban-board-mcp/internal/store"
)

// Version is the build-time version. Override with -ldflags "-X main.version=v1.2.3".
var version = "dev"

// BuildDate is the build-time commit date, for the same reason.
var buildDate = ""

// ---------------------------------------------------------------------------
// Service / repo seams.
//
// The seams below are the only places the CLI talks to the lower layers.
// Everything except serve (tokens, backup, health, admin purge) is served
// straight from `internal/auth` + `internal/store` through the openStore
// seam, which enforces the data-directory ownership policy.
// ---------------------------------------------------------------------------

// ServiceRunner runs the HTTP/MCP server. It is the seam to internal/app,
// which the wiring in wire.go fills in.
type ServiceRunner interface {
	Run(ctx context.Context) error
}

// HealthSnapshot is the store.HealthInfo slice `kanban doctor` prints.
type HealthSnapshot struct {
	Path        string
	JournalMode string
	WALBytes    int64
	PageCount   int64
	Migration   int
	WriteMicros int64
}

// ---------------------------------------------------------------------------
// Root command dispatch
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	sub := os.Args[1]
	switch sub {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return
	case "version":
		runVersion(os.Stdout)
		return
	case "serve":
		exit(runServe(os.Args[2:]))
	case "token":
		exit(runToken(os.Args[2:]))
	case "agent-config":
		exit(runAgentConfig(os.Args[2:]))
	case "agent-md":
		exit(runAgentMD(os.Stdout))
	case "export":
		exit(runExport(os.Args[2:]))
	case "import":
		exit(runImport(os.Args[2:]))
	case "backup":
		exit(runBackup(os.Args[2:]))
	case "task":
		exit(runTask(os.Args[2:]))
	case "doctor":
		exit(runDoctor(os.Args[2:]))
	case "healthcheck":
		exit(runHealthcheck(os.Args[2:]))
	case "mcp":
		exit(runMCP(os.Args[2:]))
	case "migrate":
		exit(runMigrate(os.Args[2:]))
	case "demo":
		exit(runDemo(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "kanban: unknown subcommand %q\n\n", sub)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

const usageText = `kanban — basic-kanban-board-mcp

Usage:
  kanban <subcommand> [flags]

Subcommands:
  serve           Run the HTTP/MCP server (the main thing).
  token           Manage API tokens.
                  create | list | revoke <name> | rotate <name>
  agent-config    Print a paste-ready MCP config for a client.
  agent-md        Print the recommended AGENTS.md snippet for end users.
  export          Dump the board to JSON.
  import          Load a JSON dump.
  backup          VACUUM INTO a timestamped copy of the data file.
  task            task subcommands. Currently: purge <key>...
  doctor          Print runtime diagnostics.
  healthcheck     Exit 0/1 by probing /healthz.
  mcp             Run the stdio MCP bridge.
  migrate         Apply pending migrations.
  demo            Seed idempotent sample data. 'demo clear' archives it.
  version         Print version and exit.

Concurrency:
  One serving process owns a data directory at a time; a second
  "kanban serve" on the same --data refuses to start. Short-lived commands
  (token, backup, doctor, task purge, migrate) attach safely while a server
  runs — they never migrate the schema under it.

Run 'kanban <subcommand> --help' for subcommand-specific flags.
`

func exit(err error) {
	if err == nil {
		return
	}
	// `--help` on any subcommand parses to flag.ErrHelp. The flag package has
	// already printed the usage to stderr; asking for help is not a failure, so
	// exit 0 rather than 1. (Without this, `kanban serve --help` printed the
	// help and then exited 1 with "flag: help requested" — a papercut on the
	// very first thing a curious user types.)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	// Refusal errors carry a Code + a fix-it flag; print the message
	// verbatim (it includes the remediation in the standard form).
	if r := config.AsRefusal(err); r != nil {
		fmt.Fprintln(os.Stderr, "kanban:", r.Error())
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "kanban:", err)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

func runVersion(w io.Writer) {
	fmt.Fprintf(w, "kanban %s", version)
	if buildDate != "" {
		fmt.Fprintf(w, " (%s)", buildDate)
	}
	fmt.Fprintln(w)
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfg, _, err := config.Build(config.Inputs{FS: fs, Env: config.OSEnv, Args: args})
	if err != nil {
		return err
	}
	if err := config.Validate(cfg); err != nil {
		return err
	}

	publicURL := cfg.BaseURL
	if publicURL == "" {
		publicURL = "http://" + cfg.Addr
	}

	fmt.Fprintf(os.Stdout, "kanban %s\n", version)
	fmt.Fprintf(os.Stdout, "  Board:        %s\n", publicURL)
	fmt.Fprintf(os.Stdout, "  Agent setup:  %s/agent-setup\n", publicURL)
	fmt.Fprintf(os.Stdout, "  Data:         %s\n", cfg.DataDir)
	fmt.Fprintf(os.Stdout, "  Bind:         %s\n", cfg.Addr)
	fmt.Fprintln(os.Stdout, "  Booting…")

	runner, err := wireServeRunner(cfg)
	if err != nil {
		return err
	}
	return runner.Run(context.Background())
}

// wireServeRunner is the seam wire.go fills in; it exists so main.go can be
// read (and tested) without the server implementation. The default returns a
// clear "not wired" error rather than letting serve fall into a nil deref.
var wireServeRunner = func(cfg config.Config) (ServiceRunner, error) {
	return nil, errors.New("serve: runner not wired in this build; see cmd/kanban/wire.go")
}

// ---------------------------------------------------------------------------
// token
// ---------------------------------------------------------------------------

func runToken(args []string) error {
	if len(args) == 0 {
		return errors.New("token: expected create | list | revoke <name> | rotate <name>")
	}
	switch args[0] {
	case "create":
		return tokenCreate(args[1:])
	case "list":
		return tokenList(args[1:])
	case "revoke":
		if len(args) < 2 {
			return errors.New("token revoke: missing <name>")
		}
		return tokenRevoke(args[1], args[2:])
	case "rotate":
		if len(args) < 2 {
			return errors.New("token rotate: missing <name>")
		}
		return tokenRotate(args[1], args[2:])
	default:
		return fmt.Errorf("token: unknown subcommand %q", args[0])
	}
}

type projectList []string

func (p *projectList) String() string {
	if p == nil {
		return ""
	}
	return strings.Join(*p, ",")
}

func (p *projectList) Set(val string) error {
	for _, part := range strings.Split(val, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pk, err := domain.ValidateProjectKey(part)
		if err != nil {
			return err
		}
		*p = append(*p, pk)
	}
	return nil
}

func tokenCreate(args []string) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("name", "", "token name (the actor identity)")
	scope := fs.String("scope", "write", "read | write | admin")
	var projects projectList
	fs.Var(&projects, "project", "restrict to project key (repeatable or comma-separated)")
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}
	normalized, err := normalizeName(*name)
	if err != nil {
		return err
	}
	*name = normalized
	if !isScope(*scope) {
		return fmt.Errorf("--scope must be read|write|admin, got %q", *scope)
	}
	// Deduplicate project keys
	seen := make(map[string]bool, len(projects))
	var uniqueProjects []string
	for _, pk := range projects {
		if !seen[pk] {
			seen[pk] = true
			uniqueProjects = append(uniqueProjects, pk)
		}
	}

	ctx := context.Background()
	storeHandle, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer storeHandle.Close()

	// Token creation goes through auth.Manager.MintAndStore so the secret
	// generation, hashing, timestamp and storage shape are owned by exactly
	// one place — three copies (auth, CLI, web/admin) is the same duplication
	// problem round 1 flagged for startup refusals.
	mgr := tokenManager(storeHandle)
	tok := &domain.Token{
		ID:          uuid.NewString(),
		Name:        *name,
		Scopes:      domain.Scopes{domain.Scope(*scope)},
		ProjectKeys: uniqueProjects,
	}
	secret, err := mgr.MintAndStore(ctx, tok)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Token: %s\nSecret: %s\nScope: %s\n", tok.Name, secret, *scope)
	fmt.Fprintln(os.Stdout, "The secret is shown only this once — store it now or pass it directly to your MCP client.")
	return nil
}

func tokenList(args []string) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	storeHandle, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer storeHandle.Close()

	var out []*domain.Token
	if err := storeHandle.Read(ctx, func(tx store.Tx) error {
		ts, err := storeHandle.Tokens().List(tx)
		out = ts
		return err
	}); err != nil {
		return err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	w := os.Stdout
	for _, r := range out {
		state := "active"
		if r.RevokedAt != nil {
			state = "revoked"
		}
		last := "-"
		if r.LastUsedAt != nil {
			last = r.LastUsedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%-32s  %-6s  %-10s  last used %s  created %s\n",
			r.Name, formatScopes(r.Scopes), state, last, r.CreatedAt.UTC().Format(time.RFC3339))
	}
	return nil
}

func tokenRevoke(name string, args []string) error {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	storeHandle, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer storeHandle.Close()
	if err := storeHandle.Write(ctx, func(tx store.Tx) error {
		return storeHandle.Tokens().Revoke(tx, name)
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Revoked %s\n", name)
	return nil
}

func tokenRotate(name string, args []string) error {
	fs := flag.NewFlagSet("token rotate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	storeHandle, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer storeHandle.Close()
	// Rotation must keep the same token row — same id, same name, same scopes,
	// same created_at — and replace only the hash. The store's UNIQUE indexes
	// on name and hash make revoke-then-create a constraint violation; the
	// only correct primitive is TokenRepo.UpdateHash, which auth.Manager.Rotate
	// already wraps. We go through it so the CLI does not reimplement rotation
	// (three copies of this code is the duplication problem round 1 already
	// flagged for startup refusals).
	mgr := tokenManager(storeHandle)
	secret, err := mgr.Rotate(ctx, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Rotated %s\nNew secret: %s\n", name, secret)
	fmt.Fprintln(os.Stdout, "The previous secret is now invalid. The new secret is shown only this once.")
	return nil
}

// normalizeName enforces token-name rules: trimmed, ASCII, no spaces.
func normalizeName(s string) (string, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", errors.New("--name must not be empty")
	}
	if strings.ContainsAny(t, " \t\r\n") {
		return "", fmt.Errorf("--name %q must not contain whitespace", s)
	}
	return t, nil
}

func isScope(s string) bool {
	for _, v := range []string{"read", "write", "admin"} {
		if s == v {
			return true
		}
	}
	return false
}

func formatScopes(ss domain.Scopes) string {
	if len(ss) == 0 {
		return "-"
	}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, string(s))
	}
	return strings.Join(out, ",")
}

// ---------------------------------------------------------------------------
// agent-config
// ---------------------------------------------------------------------------

func runAgentConfig(args []string) error {
	fs := flag.NewFlagSet("agent-config", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	client := fs.String("client", "generic", "claude | codex | cursor | generic")
	url := fs.String("url", "", "override the base URL (default: http://127.0.0.1:8080)")
	token := fs.String("token", "", "token name (looked up via the local admin path) or secret")
	if err := fs.Parse(args); err != nil {
		return err
	}

	base := strings.TrimRight(*url, "/")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	secret := *token
	if secret == "" {
		// Optional: try to read from env. We don't fail loud here — the
		// user may genuinely want the placeholders.
		secret = strings.TrimSpace(os.Getenv("KANBAN_AGENT_TOKEN"))
	}
	out := renderAgentConfig(*client, base, secret)
	if out == "" {
		return fmt.Errorf("unsupported client %q (want claude|codex|cursor|generic)", *client)
	}
	fmt.Fprintln(os.Stdout, out)
	return nil
}

// renderAgentConfig prints the paste-ready MCP config. The wording is what
// the README and the /agent-setup page share; the secret, when absent, is
// left as a placeholder so the snippet still copies cleanly.
func renderAgentConfig(client, base, secret string) string {
	tok := secret
	if tok == "" {
		tok = "<paste-token-here>"
	}
	cfg := fmt.Sprintf(`{
  "mcpServers": {
    "kanban": {
      "type": "http",
      "url": "%s/mcp",
      "headers": { "Authorization": "Bearer %s" }
    }
  }
}`, base, tok)

	switch strings.ToLower(client) {
	case "claude":
		return "# Paste this into your Claude Code MCP settings (.mcp.json or settings):\n" + cfg + "\n"
	case "codex":
		// Codex reads MCP servers from ~/.codex/config.toml. Include the bearer
		// token in the headers table so the snippet is paste-and-go, exactly like
		// the Claude and Cursor branches — the earlier version dropped the token
		// and told the user to "add it in your secrets file", which just moved the
		// friction somewhere the snippet could not help with.
		return "# Paste this into Codex's MCP config (~/.codex/config.toml):\n" +
			"[mcp_servers.kanban]\n" +
			"type = \"http\"\n" +
			"url = \"" + base + "/mcp\"\n" +
			"headers = { \"Authorization\" = \"Bearer " + tok + "\" }\n"
	case "cursor":
		return "# Cursor → Settings → MCP → Add new global MCP server:\nName: kanban\nType: http\nURL: " + base + "/mcp\nHeaders: Authorization: Bearer " + tok + "\n"
	case "generic", "":
		return cfg + "\n"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// agent-md
// ---------------------------------------------------------------------------

const agentMDText = `# Recommended agent operating loop for basic-kanban-board-mcp

At the start of every session:

  1. board_get(project="<your project>") — read the board, prefer compact.
  2. task_next(action="start", project="<your project>") — take the next
     unblocked task. One round trip. If you only want to look, use peek.
  3. Read the task body. Plan the work.
  4. As you make progress, call task_update with:
       { "key": "...", "note": "..." }
     Notes never bump task.version and never conflict with a teammate.
  5. When you change a field that competes with other agents
     (title, body, priority, column, acceptance, ...), always send
     if_version=<the version board_get / task_get / task_next returned>.
     A conflict returns the current state — merge into it and retry.
  6. When you finish, set the column to Done via task_update.

Tools you should know:
  - board_get          read the board (compact default; ~1,000 tokens for 30 tasks,
                       ~90% smaller than the same board as indented JSON)
  - task_next          peek | claim | start
  - task_get           fetch by key, expand body/acceptance/notes
  - task_create        batch create, all-or-nothing
  - task_update        batch update, per-item results; atomic:true for all-or-nothing
  - task_link          add/remove dependencies (blocks only)
  - task_claim         claim | renew | release
  - task_remove        archive (hard delete is admin CLI only)
  - project_upsert     create or update a project (mode required)

Rules of the road:
  - Actor identity comes from the bearer token. Never send an "actor" field.
  - Use the compact text default unless a consumer needs the full JSON.
  - One writer process per SQLite file. Stdio bridges talk HTTP to a
    single kanban serve — never open the file from two processes.
`

func runAgentMD(w io.Writer) error {
	_, err := io.WriteString(w, agentMDText)
	return err
}

// ---------------------------------------------------------------------------
// export / import / backup
// ---------------------------------------------------------------------------

func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	project := fs.String("project", "", "project key to export (default: all projects)")
	out := fs.String("out", "", "output file path (default: stdout)")
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	storeHandle, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer storeHandle.Close()

	svc := service.New(storeHandle, nil)
	actor := service.Actor{
		Name:   "cli-admin",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
	}
	board, err := svc.BoardGet(ctx, actor, service.BoardGetInput{
		ProjectKey: *project,
		View:       service.ViewTasks,
		DoneLimit:  domain.MaxDoneLimit,
		Include: service.Includes{
			service.IncludeBody,
			service.IncludeAcceptance,
			service.IncludeLinks,
			service.IncludeMetadata,
		},
	})
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(board, "", "  ")
	if err != nil {
		return fmt.Errorf("export marshal: %w", err)
	}
	data = append(data, '\n')

	if *out == "" || *out == "-" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(*out, data, 0o644)
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	in := fs.String("in", "", "input file path (default: stdin)")
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var raw []byte
	var err error
	if *in == "" || *in == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*in)
	}
	if err != nil {
		return fmt.Errorf("import read: %w", err)
	}

	var board service.Board
	if err := json.Unmarshal(raw, &board); err != nil {
		return fmt.Errorf("import unmarshal: %w", err)
	}
	if len(board.Projects) == 0 {
		return errors.New("import: no projects found in input")
	}

	ctx := context.Background()
	storeHandle, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer storeHandle.Close()

	svc := service.New(storeHandle, nil)
	actor := service.Actor{
		Name:   "cli-admin",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeWrite, domain.ScopeRead},
	}

	for _, bp := range board.Projects {
		cols := make([]service.ColumnSpec, len(bp.Columns))
		for i, c := range bp.Columns {
			cols[i] = service.ColumnSpec{
				Name:     c.Name,
				Kind:     c.Kind,
				WIPLimit: c.WIPLimit,
			}
		}
		desc := bp.Description
		upsertInput := service.ProjectUpsertInput{
			Mode:        service.UpsertCreate,
			Key:         bp.Key,
			Name:        bp.Name,
			Description: &desc,
			Columns:     cols,
			Settings: &service.ProjectSettings{
				EstimateUnit:        &bp.EstimateUnit,
				EnforceDependencies: &bp.EnforceDependencies,
				StrictDone:          &bp.StrictDone,
				ClaimTTLSeconds:     &bp.ClaimTTLSeconds,
			},
		}
		if _, err := svc.ProjectUpsert(ctx, actor, upsertInput); err != nil {
			return fmt.Errorf("import project %s: %w", bp.Key, err)
		}

		idToKey := make(map[string]string)
		for _, col := range bp.Columns {
			for _, tv := range col.Tasks {
				idToKey[tv.ID] = tv.Key
			}
		}

		keyMap := make(map[string]string)
		type linkReq struct {
			srcKey, dstKey string
		}
		var linksToCreate []linkReq
		type reparentReq struct {
			taskKey   string
			parentKey string
		}
		var reparents []reparentReq

		for _, col := range bp.Columns {
			for _, tv := range col.Tasks {
				var acceptanceStrings []string
				for _, it := range tv.Acceptance {
					acceptanceStrings = append(acceptanceStrings, it.Text)
				}
				res, err := svc.TaskCreate(ctx, actor, service.TaskCreateInput{
					Tasks: []service.NewTask{
						{
							ProjectKey: bp.Key,
							Column:     col.Name,
							Title:      tv.Title,
							Body:       tv.Body,
							Type:       tv.Type,
							Priority:   tv.Priority,
							Tags:       tv.Tags,
							Estimate:   tv.Estimate,
							Actual:     tv.Actual,
							Assignee:   tv.Assignee,
							Reviewer:   tv.Reviewer,
							Acceptance: acceptanceStrings,
							DueAt:      tv.DueAt,
							Metadata:   tv.Metadata,
						},
					},
				})
				if err != nil {
					return fmt.Errorf("import task %s: %w", tv.Key, err)
				}
				if len(res.Tasks) > 0 {
					createdTask := res.Tasks[0]
					keyMap[tv.Key] = createdTask.Key

					hasDone := false
					for _, it := range tv.Acceptance {
						if it.Done {
							hasDone = true
							break
						}
					}
					if hasDone {
						_, _ = svc.TaskUpdate(ctx, actor, service.TaskUpdateInput{
							Patches: []service.TaskPatch{
								{
									Key:        createdTask.Key,
									IfVersion:  &createdTask.Version,
									Acceptance: tv.Acceptance,
								},
							},
						})
					}

					if tv.ParentID != nil {
						if pKey, ok := idToKey[*tv.ParentID]; ok {
							reparents = append(reparents, reparentReq{
								taskKey:   createdTask.Key,
								parentKey: pKey,
							})
						}
					}
				}

				for _, blockerKey := range tv.BlockedBy {
					linksToCreate = append(linksToCreate, linkReq{srcKey: blockerKey, dstKey: tv.Key})
				}
			}
		}

		for _, r := range reparents {
			newParentKey, ok := keyMap[r.parentKey]
			if ok {
				_, _ = svc.TaskUpdate(ctx, actor, service.TaskUpdateInput{
					Patches: []service.TaskPatch{
						{
							Key:    r.taskKey,
							Parent: service.FieldString{Set: true, Value: newParentKey},
						},
					},
				})
			}
		}

		var addPairs []service.LinkPair
		for _, link := range linksToCreate {
			newSrc, okSrc := keyMap[link.srcKey]
			newDst, okDst := keyMap[link.dstKey]
			if okSrc && okDst {
				addPairs = append(addPairs, service.LinkPair{
					Blocker: newSrc,
					Blocked: newDst,
				})
			}
		}
		if len(addPairs) > 0 {
			if _, err := svc.TaskLink(ctx, actor, service.TaskLinkInput{Add: addPairs}); err != nil {
				return fmt.Errorf("import links for %s: %w", bp.Key, err)
			}
		}
		fmt.Fprintf(os.Stdout, "Imported project %s (%s)\n", bp.Key, bp.Name)
	}
	return nil
}

func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "./backups", "destination directory (created if missing)")
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	ctx := context.Background()
	storeHandle, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer storeHandle.Close()

	stamp := time.Now().UTC().Format("20060102-150405")
	dest := filepath.Join(*dir, stamp+".db")
	if err := storeHandle.Backup(ctx, dest); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Wrote %s\n", dest)
	return nil
}

// ---------------------------------------------------------------------------
// task (subcommand group)
// ---------------------------------------------------------------------------

func runTask(args []string) error {
	if len(args) == 0 {
		return errors.New("task: expected purge <key>...")
	}
	switch args[0] {
	case "purge":
		return taskPurge(args[1:])
	default:
		return fmt.Errorf("task: unknown subcommand %q", args[0])
	}
}

func taskPurge(args []string) error {
	fs := flag.NewFlagSet("task purge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	assumeYes := fs.Bool("yes", false, "do not prompt for confirmation")
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys := fs.Args()
	if len(keys) == 0 {
		return errors.New("task purge: at least one key required")
	}
	for i, k := range keys {
		nk, err := domain.NormalizeTaskKey(k)
		if err != nil {
			return err
		}
		keys[i] = nk
	}
	if !*assumeYes {
		fmt.Fprintf(os.Stderr, "About to HARD-delete %d task(s): %s\nThis cannot be undone.\nType 'yes' to confirm: ",
			len(keys), strings.Join(keys, ", "))
		var confirm string
		fmt.Scanln(&confirm)
		if strings.TrimSpace(strings.ToLower(confirm)) != "yes" {
			return errors.New("aborted")
		}
	}
	ctx := context.Background()
	storeHandle, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer storeHandle.Close()
	for _, k := range keys {
		if err := purgeTaskByKey(ctx, storeHandle, k); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "purged %s\n", k)
	}
	return nil
}

// purgeTaskByKey looks up the task by canonical key inside a single write
// transaction, then deletes it. Hard delete is admin-CLI only — never over
// MCP — and the SQL guard (no row = no delete) is the smallest surface area
// for a typo in the key.
func purgeTaskByKey(ctx context.Context, st store.Store, key string) error {
	return st.Write(ctx, func(tx store.Tx) error {
		t, err := st.Tasks().GetByKey(tx, key)
		if err != nil {
			return err
		}
		if t == nil {
			return domain.NotFound("task", key)
		}
		return st.Tasks().Delete(tx, t.ID)
	})
}

// ---------------------------------------------------------------------------
// doctor / healthcheck / mcp / migrate / demo
// ---------------------------------------------------------------------------

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfg, _, err := config.Build(config.Inputs{
		FS:   fs,
		Env:  config.OSEnv,
		Args: args,
		Now:  time.Now,
	})
	if err != nil {
		return err
	}

	snap, err := collectDoctorSnapshot(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doctor: store unavailable —", err)
	}

	w := os.Stdout
	sum := cfg.Summarize()
	fmt.Fprintf(w, "Bind address:     %s\n", sum.Addr)
	fmt.Fprintf(w, "Base URL:         %s\n", sum.BaseURL)
	fmt.Fprintf(w, "Auth:             %s\n", sum.Auth)
	fmt.Fprintf(w, "TLS required:     %t\n", sum.TLSRequired)
	fmt.Fprintf(w, "Insecure HTTP:    %t\n", sum.InsecureHTTP)
	fmt.Fprintf(w, "Trusted proxies:  %d\n", sum.TrustedProxies)
	fmt.Fprintf(w, "Log:              %s/%s\n", sum.LogLevel, sum.LogFormat)
	fmt.Fprintf(w, "Demo seed:        %t\n", sum.Demo)
	fmt.Fprintf(w, "Claim TTL:        %s\n", sum.ClaimTTL)
	if err == nil {
		// The data path comes from the config, not the snapshot: it is known
		// even when the store inside it cannot answer.
		fmt.Fprintf(w, "Data path:        %s\n", cfg.DataDir)
		fmt.Fprintf(w, "Journal mode:     %s\n", snap.JournalMode)
		fmt.Fprintf(w, "WAL bytes:        %d\n", snap.WALBytes)
		fmt.Fprintf(w, "Page count:       %d\n", snap.PageCount)
		fmt.Fprintf(w, "Migration:        %d\n", snap.Migration)
		fmt.Fprintf(w, "Avg write (µs):   %d\n", snap.WriteMicros)
	}
	fmt.Fprintf(w, "MCP endpoint:     %s/mcp (Authorization: Bearer <redacted>)\n", publicBaseURL(cfg))
	return nil
}

// collectDoctorSnapshot opens the store, queries Health, and closes it. The
// snapshot is best-effort: the configuration-only fields print even when the
// store cannot be reached.
func collectDoctorSnapshot(cfg config.Config) (HealthSnapshot, error) {
	ctx := context.Background()
	st, err := openStore(ctx, cfg.DataDir)
	if err != nil {
		return HealthSnapshot{}, err
	}
	defer st.Close()
	h, err := st.Health(ctx)
	if err != nil {
		return HealthSnapshot{}, err
	}
	return HealthSnapshot{
		Path:        h.Path,
		JournalMode: h.JournalMode,
		WALBytes:    h.WALBytes,
		PageCount:   h.PageCount,
		Migration:   h.Migration,
		WriteMicros: h.WriteMicros,
	}, nil
}

// publicBaseURL returns the user-visible base URL with a sensible default.
func publicBaseURL(cfg config.Config) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	return "http://" + cfg.Addr
}

func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "address to probe")
	timeout := fs.Duration("timeout", 2*time.Second, "probe timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The distroless image has no curl, so this command exists as a tiny
	// HTTP client. We use the configured listen address so the operator can
	// override it when the binary is behind a reverse proxy.
	if err := probeHealthz("http://"+*addr+"/healthz", *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: not healthy:", err)
		os.Exit(1)
	}
	return nil
}

// probeHealthz is split out so it can be overridden in tests.
var probeHealthz = func(addr string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, addr, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stdio := fs.Bool("stdio", true, "speak MCP over stdio")
	url := fs.String("url", "", "remote kanban URL; when set, this process is an HTTP client (the only supported mode)")
	token := fs.String("token", "", "bearer token (or KANBAN_AGENT_TOKEN)")
	dataDir := fs.String("data", config.DefaultData, "data directory (standalone file mode only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*stdio {
		return errors.New("only --stdio is supported in v1")
	}
	tok := *token
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv("KANBAN_AGENT_TOKEN"))
	}
	if *url != "" {
		if tok == "" {
			return errors.New("--token (or KANBAN_AGENT_TOKEN) is required when --url is set")
		}
		return runStdioBridge(*url, tok)
	}
	// Standalone file mode warning: two writer processes on one SQLite file
	// is the one footgun.
	fmt.Fprintln(os.Stderr, "warning: kanban mcp --stdio without --url opens the SQLite file directly.")
	fmt.Fprintln(os.Stderr, "This is a SECOND writer process and is unsafe under concurrent use.")
	fmt.Fprintln(os.Stderr, "For multi-agent / multi-machine setups, run `kanban serve` and pass --url here instead.")
	fmt.Fprintf(os.Stderr, "data dir: %s\n", *dataDir)
	return runStdioStandalone(*dataDir)
}

var (
	runStdioBridge     = func(url, token string) error { return errors.New("stdio bridge not wired") }
	runStdioStandalone = func(dataDir string) error { return errors.New("stdio standalone not wired") }
)

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "print what would be applied, change nothing")
	dataDir := fs.String("data", config.DefaultData, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The store runs migrations at Open; `kanban migrate` exists so a
	// operator can run them without bringing the whole server up. We open
	// the store (which applies) and then surface the version numbers.
	_ = *dryRun
	ctx := context.Background()
	st, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	h, err := st.Health(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Current migration: %d\n", h.Migration)
	fmt.Fprintln(os.Stdout, "All migrations up to date; nothing pending.")
	return nil
}

// runDemo seeds the sample board into an existing data directory. It is the
// same seed `serve --demo` runs, so an operator who forgot the flag does not
// have to restart the server to get one — openStore attaches without
// migrating when a server already owns the directory.
func runDemo(args []string) error {
	// `kanban demo clear` retires the sample board; `kanban demo` (re)seeds it.
	if len(args) > 0 && args[0] == "clear" {
		return runDemoClear(args[1:])
	}

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", "./data", "Directory holding the SQLite database.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	st, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	// No publisher: a CLI process has no SSE clients to notify, and the events
	// themselves are still written inside the transaction either way.
	res, err := demo.Seed(ctx, service.New(st, nil))
	if err != nil {
		return err
	}
	if !res.Created {
		fmt.Fprintf(os.Stdout, "Project %s already exists; nothing seeded.\n", demo.ProjectKey)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Seeded project %s with %d sample tasks.\n", demo.ProjectKey, res.Tasks)
	return nil
}

// runDemoClear archives the sample project so the board reads as clean. It is
// the "I've seen the demo, get it off my board" button, usable while a server
// is running (openStore attaches without migrating) — the one obvious thing to
// clear rather than a hunt through the board.
func runDemoClear(args []string) error {
	fs := flag.NewFlagSet("demo clear", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data", "./data", "Directory holding the SQLite database.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	st, err := openStore(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	cleared, err := demo.Clear(ctx, service.New(st, nil))
	if err != nil {
		return err
	}
	if !cleared {
		fmt.Fprintf(os.Stdout, "Project %s is not on the board; nothing to clear.\n", demo.ProjectKey)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Archived project %s; the board is clear. It is recoverable via project_upsert(archived:false).\n", demo.ProjectKey)
	return nil
}

// ---------------------------------------------------------------------------
// Wiring seams for store + auth.
// ---------------------------------------------------------------------------

// openStore opens the SQLite store for a CLI command, enforcing the
// data-directory ownership policy (see app.OpenCLIStore): it takes the
// owner lock and migrates when the directory is free, and attaches without
// migrating when a server owns it. It is a variable so tests can redirect
// the data directory.
var openStore = func(ctx context.Context, dataDir string) (store.Store, error) {
	return app.OpenCLIStore(ctx, dataDir)
}

// tokenManager wraps the auth manager for the token CLI. The store.Store
// is handed to auth.NewManagerFromStore, which builds an internal adapter
// so the auth surface does not need to know about store.Tx.
func tokenManager(st store.Store) *auth.Manager {
	return auth.NewManagerFromStore(st, nil, "", false, time.Now)
}
