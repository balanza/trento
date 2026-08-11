package patchrelease

// Input parameters for one patch-release run.
type Input struct {
	// Submodules names (e.g. "web", "wanda") to limit processing to.
	// Empty = every submodule declared in the super-repo's .gitmodules.
	Submodules []string `json:"submodules,omitempty"`
	// Agent selects the coding agent backend. "claude" (default) or "pi".
	Agent string `json:"agent,omitempty"`
}

// Output is the aggregated result of one patch-release run.
type Output struct {
	Reports     []RepoReport `json:"reports"`
	PRs         []string     `json:"prs"`         // every PR URL touched (created or pre-existing)
	FailedPicks []string     `json:"failedPicks"` // "owner/repo #num (branch)" entries
}

// RepoReport is the per-repo summary.
type RepoReport struct {
	Repo           string   `json:"repo"`              // owner/name
	Skipped        bool     `json:"skipped,omitempty"` // true when no PRs match the milestone
	CurrentVersion string   `json:"currentVersion"`
	NextVersion    string   `json:"nextVersion"`
	Backports      []string `json:"backports"`     // backport PR URLs
	VersionBumpPR  string   `json:"versionBumpPR"` // the "Trigger release X.Y.Z" PR URL
	Failed         []string `json:"failed"`        // unresolved-cherry-pick entries
}

func applyDefaults(in Input) Input {
	if in.Agent == "" {
		in.Agent = "claude"
	}
	return in
}
