package testonobs

// FixIterate selects the on-failure behavior.
type FixIterate string

const (
	// FixIterateOff disables fix-iterate; failed packages stay failed.
	FixIterateOff FixIterate = "off"
	// FixIterateReview gates Claude's proposed patch behind a human
	// awakeable approval before applying it.
	FixIterateReview FixIterate = "review"
	// FixIterateAuto applies Claude's proposed patch immediately,
	// republishes the affected submodule, re-polls until terminal, and
	// loops up to MaxAttempts. No human gate — use only when you trust
	// the workflow to edit submodule spec files autonomously.
	FixIterateAuto FixIterate = "auto"
)

// FinalStatus is the per-package outcome reported in Output.
type FinalStatus string

const (
	FinalStatusOK      FinalStatus = "ok"
	FinalStatusFailed  FinalStatus = "failed-after-N"
	FinalStatusSkipped FinalStatus = "skipped"
	FinalStatusAborted FinalStatus = "aborted"
)

// Input is the user-supplied parameters for one workflow run.
type Input struct {
	// Submodules selects which submodules to publish. Empty = all five with
	// packaging: web, wanda, agent, mcp-server, checks.
	Submodules []string `json:"submodules,omitempty"`

	// FixIterate selects on-failure behavior. 'auto' (default) lets the
	// workflow apply Claude's patches without a human gate; 'review'
	// inserts a HITL awakeable; 'off' disables fix-iterate.
	FixIterate FixIterate `json:"fixIterate,omitempty" validate:"oneof=off review auto"`

	// MaxAttempts caps the fix-iterate retry count per failing package.
	MaxAttempts int `json:"maxAttempts" validate:"min=1,max=10"`

	// CleanupOnSuccess removes the personal subproject after a green run.
	CleanupOnSuccess bool `json:"cleanupOnSuccess"`

	// CleanupOnFailure removes the personal subproject after a red run.
	// Default false so the state stays around for inspection.
	CleanupOnFailure bool `json:"cleanupOnFailure"`
}

// Output is the structured result of one workflow run.
type Output struct {
	Project string          `json:"project"`
	Results []PackageResult `json:"results"`
}

// PackageResult is the per-package outcome.
type PackageResult struct {
	Pkg         string      `json:"pkg"`
	FinalStatus FinalStatus `json:"finalStatus"`
	Attempts    int         `json:"attempts"`
	LogsRef     string      `json:"logsRef"`
}

// Submodule name constants. Kept here so schema/workflow/helpers all
// reference one source of truth.
const (
	SMWeb       = "web"
	SMWanda     = "wanda"
	SMAgent     = "agent"
	SMMCPServer = "mcp-server"
	SMChecks    = "checks"
)

// DefaultSubmodules returns the five submodules with packaging.
func DefaultSubmodules() []string {
	return []string{SMWeb, SMWanda, SMAgent, SMMCPServer, SMChecks}
}

// defaultMaxAttempts is the per-failing-package retry cap when the
// caller omits MaxAttempts.
const defaultMaxAttempts = 3

// applyDefaults mutates a copy of `in` with values for any zero-valued
// optional fields, then returns it.
func applyDefaults(in Input) Input {
	if len(in.Submodules) == 0 {
		in.Submodules = DefaultSubmodules()
	}
	if in.MaxAttempts == 0 {
		in.MaxAttempts = defaultMaxAttempts
	}
	if in.FixIterate == "" {
		in.FixIterate = FixIterateAuto
	}
	// CleanupOnSuccess defaults true; honor explicit false only when the
	// caller passed cleanupOnSuccess=false. Restate gives us the raw JSON
	// already decoded; if the field was omitted, in.CleanupOnSuccess is
	// false. Users who want the documented default must pass true.
	// (Documented in README; the simpler alternative — a pointer field —
	// pollutes downstream code.)
	return in
}
