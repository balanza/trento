package fixprci

// FinalStatus is the workflow-level outcome.
type FinalStatus string

const (
	FinalStatusGreen     FinalStatus = "green"
	FinalStatusExhausted FinalStatus = "exhausted"
	FinalStatusAborted   FinalStatus = "aborted"
)

// Input is the user-supplied parameters for one workflow run.
type Input struct {
	// Repo as owner/name, e.g. "trento-project/web".
	Repo string `json:"repo" validate:"required,contains=/"`

	// PR number on Repo.
	PRNumber int `json:"prNumber" validate:"required,min=1"`

	// Per-issueKey attempt cap before an issue is marked exhausted.
	MaxAttempts int `json:"maxAttempts" validate:"min=1,max=20"`

	// Total iteration hard cap (safety net for the outer loop).
	MaxIterations int `json:"maxIterations" validate:"min=1,max=50"`

	// If true, drop the temp clone after the workflow ends. Default false
	// (state kept for inspection under .workflows/.runs/<id>/work).
	CleanupOnExit bool `json:"cleanupOnExit"`
}

// Output is the structured result of one workflow run.
type Output struct {
	Repo        string         `json:"repo"`
	PRNumber    int            `json:"prNumber"`
	FinalStatus FinalStatus    `json:"finalStatus"`
	Iterations  int            `json:"iterations"`
	Commits     []string       `json:"commits"`
	Issues      []IssueOutcome `json:"issues"`
}

// IssueOutcome is one row of the per-issue report.
type IssueOutcome struct {
	IssueKey   string   `json:"issueKey"`
	Category   string   `json:"category"`   // flaky|bug|infra|unfixable
	Summary    string   `json:"summary"`
	Attempts   int      `json:"attempts"`
	FinalState string   `json:"finalState"` // fixed-pending-ci|exhausted|unfixable|still-flaky|rerun-only
	JobRefs    []string `json:"jobRefs"`    // "run/<runID>/job/<jobID>"
}

const (
	defaultMaxAttempts   = 5
	defaultMaxIterations = 15
)

func applyDefaults(in Input) Input {
	if in.MaxAttempts == 0 {
		in.MaxAttempts = defaultMaxAttempts
	}
	if in.MaxIterations == 0 {
		in.MaxIterations = defaultMaxIterations
	}
	return in
}
