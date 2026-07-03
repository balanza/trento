// Package dependabotsweep implements the "dependabot-sweep" workflow:
// for every super-repo submodule (or a filtered subset), list open
// Dependabot PRs, drive each to green via trento.fix-pr-ci, attach
// the next-patch-version milestone, and squash-merge. Repos in
// parallel, PRs-in-a-repo sequential.
//
// In the v1 scaffold every activity is a no-op stub, so a run completes
// instantly with empty reports.
package dependabotsweep

// PRState is the per-PR terminal outcome of the sweep.
type PRState string

const (
	PRStateMergedClean     PRState = "merged-clean"     // CI was green already, merged
	PRStateMergedAfterFix  PRState = "merged-after-fix" // fix-pr-ci ended green, merged
	PRStateLeftForHuman    PRState = "left-for-human"   // fix-pr-ci exhausted/aborted
	PRStateSkippedDryRun   PRState = "skipped-dry-run"  // would-have-merged in DryRun
	PRStateSkippedNotReady PRState = "skipped-not-ready" // mergeable_state blocked/dirty/behind
	PRStateFailedInternal  PRState = "failed-internal"  // sweep-side error, see Note
)

// Input parameters for one dependabot-sweep run.
type Input struct {
	// Submodules names (e.g. "web", "wanda") to limit processing to.
	// Empty = every submodule declared in the super-repo's .gitmodules.
	Submodules []string `json:"submodules,omitempty"`

	// Per-PR cap on how many fix-pr-ci iterations we allow before we
	// give up and leave the PR for a human. Forwarded to fix-pr-ci as
	// its MaxIterations. Default 10.
	MaxIterationsPerPR int `json:"maxIterationsPerPR,omitempty"`

	// If true, DO NOT merge anything. Report what would have happened.
	// fix-pr-ci still runs in full and may push commits; only the
	// squash-merge step is gated. Default false.
	DryRun bool `json:"dryRun,omitempty"`
}

// PROutcome is the per-PR summary inside a RepoReport.
type PROutcome struct {
	Number     int     `json:"number"`
	URL        string  `json:"url"`
	Title      string  `json:"title"`
	State      PRState `json:"state"`
	Milestone  string  `json:"milestone,omitempty"`  // milestone attached (or would attach)
	FixRunID   string  `json:"fixRunId,omitempty"`   // fix-pr-ci invocation id, when spawned
	Iterations int     `json:"iterations,omitempty"` // reported by fix-pr-ci
	Note       string  `json:"note,omitempty"`       // short human-readable context
}

// RepoReport is the per-repo summary.
type RepoReport struct {
	Repo           string      `json:"repo"`              // owner/name
	Skipped        bool        `json:"skipped,omitempty"` // no open dependabot PRs
	CurrentVersion string      `json:"currentVersion"`
	NextVersion    string      `json:"nextVersion"`
	Milestone      string      `json:"milestone"`         // NextVersion, or "" on skip
	PRs            []PROutcome `json:"prs"`
	Errors         []string    `json:"errors,omitempty"`  // per-repo top-level errors
}

// Output is the aggregated result of one dependabot-sweep run.
type Output struct {
	Reports  []RepoReport `json:"reports"`
	Merged   []string     `json:"merged"`   // PR URLs merged this run
	LeftOpen []string     `json:"leftOpen"` // PR URLs still open, human-followup
}

const (
	defaultMaxIterationsPerPR = 10
)

func applyDefaults(in Input) Input {
	if in.MaxIterationsPerPR == 0 {
		in.MaxIterationsPerPR = defaultMaxIterationsPerPR
	}
	return in
}
