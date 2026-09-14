// Command kanban-adapter delivers addressed board messages to one CLI worker.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ultrathinker/basic-kanban-board-mcp/internal/adapter"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kanban-adapter:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("kanban-adapter", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	url := fs.String("url", "", "board base URL or MCP endpoint")
	project := fs.String("project", "", "project key")
	identity := fs.String("identity", "", "board token id this adapter receives for")
	state := fs.String("state", filepath.Join(".", "data", "adapter-state.json"), "durable adapter state file")
	session := fs.String("session", "", "target CLI session id")
	runnerName := fs.String("runner", "claude", "delivery runner: claude")
	workdir := fs.String("workdir", "", "working directory for the CLI")
	once := fs.Bool("once", false, "poll and deliver one pending message")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*url) == "" || strings.TrimSpace(*project) == "" || strings.TrimSpace(*identity) == "" {
		return errors.New("--url, --project, and --identity are required")
	}
	token := strings.TrimSpace(os.Getenv("KANBAN_ADAPTER_TOKEN"))
	if token == "" {
		return errors.New("KANBAN_ADAPTER_TOKEN is required and is never stored in adapter state")
	}
	store, err := adapter.OpenStore(*state, *url, *project, *identity)
	if err != nil {
		return err
	}
	client := adapter.MCPFeedClient{Endpoint: *url, Token: token, Project: *project}
	if *runnerName != "claude" {
		return fmt.Errorf("unsupported runner %q; only claude has a confirmed delivery implementation", *runnerName)
	}
	if strings.TrimSpace(*session) == "" {
		return errors.New("--session is required; the adapter does not invent a CLI session")
	}
	runner := adapter.ClaudeRunner{WorkDir: *workdir}
	poll := func() error {
		page, err := client.Read(context.Background(), store.Snapshot().Cursor)
		if err != nil {
			return err
		}
		if err := store.Receive(page, *session); err != nil {
			return err
		}
		return adapter.DeliverNext(context.Background(), store, runner, time.Now)
	}
	if *once {
		return poll()
	}
	return errors.New("continuous mode is intentionally not started by this command; run with --once from your supervisor")
}
