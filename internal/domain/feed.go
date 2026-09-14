package domain

// FeedPostingRules is how an agent is expected to keep a project feed: the
// rhythm of posting, what a pause must say, and the promise the board makes
// back (it reports the gap since the last post and never claims to know
// whether an agent is alive).
//
// It lives in domain because two surfaces have to state the same rule and
// neither may import the other: the MCP `project_post` tool description
// (internal/mcp) and the paste-ready standing prompt in the web UI
// (internal/web/view). KANB-39 acceptance criterion 3 requires the rule to
// reach the MCP instruction, and a second hand-kept copy would drift from
// the first the moment either is edited — the same reasoning that keeps the
// rounding rule single-sourced.
//
// The text is prose, not a validation rule, so nothing here parses it; it is
// interpolated verbatim by both callers.
const FeedPostingRules = "post the FIRST thought at the start of work, then KEEP POSTING as the task moves through its stages — " +
	"one post per stage transition (claimed, started, blocked, unblocked, finished). " +
	"Pulse roughly every five minutes of ACTIVE work — not on a wall clock, only while you are actually doing things " +
	"for the task — so a long quiet stretch between posts is the honest signal that nothing has happened, not invented activity. " +
	"Always name the REASON when you stop (waiting on a tool, waiting on a human, paused for the night), " +
	"and always write a final SUMMARY post when you finish or stop. " +
	"The board shows the time of the last post and the gap since, and never shows an \"online\" dot — " +
	"it does not know whether your process is alive, and a fake indicator would lie exactly when a real failure matters most."
