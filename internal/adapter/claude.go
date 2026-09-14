package adapter

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeRunner delivers into a stopped Claude Code session through its
// documented non-interactive resume interface. A zero exit status is the only
// acknowledgement that becomes DeliveryConfirmed. Claude Code does not expose
// a verified API for injecting into an already running session, so this runner
// never claims to do that.
type ClaudeRunner struct {
	Executable string
	WorkDir    string
}

// Deliver resumes sessionID and waits for Claude Code to accept and finish the
// turn. The prompt contains the original feed id so a worker can cite it.
func (r ClaudeRunner) Deliver(ctx context.Context, sessionID string, msg QueuedMessage) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("Claude delivery requires a saved session id")
	}
	executable := r.Executable
	if executable == "" {
		executable = "claude"
	}
	prompt := fmt.Sprintf("Board message %s from %s (%s):\n\n%s", msg.ID, msg.Author, msg.Kind, msg.Body)
	cmd := exec.CommandContext(ctx, executable, "--print", "--output-format", "json", "--resume", sessionID, prompt)
	cmd.Dir = r.WorkDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Claude Code did not acknowledge the turn: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
