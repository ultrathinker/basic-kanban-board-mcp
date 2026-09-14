package adapter

// Capability describes a fact about a concrete CLI integration, rather than a
// promise made by the generic adapter interface.
type Capability struct {
	CLI              string
	Launch           string
	Resume           string
	InputWhileActive string
	Stop             string
	CrashRecovery    string
}

// ConfirmedCapabilities lists only features evidenced by the installed Claude
// Code 2.1.270 help text and this adapter's concrete runner.
func ConfirmedCapabilities() []Capability {
	return []Capability{{
		CLI:              "Claude Code 2.1.270",
		Launch:           "available: --print and --bg are documented",
		Resume:           "implemented: --print --resume for a saved stopped session",
		InputWhileActive: "not confirmed; adapter does not claim support",
		Stop:             "CLI command exists, but adapter does not invoke it",
		CrashRecovery:    "not confirmed; use the durable checkpoint fallback",
	}}
}
