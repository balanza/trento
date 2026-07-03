package dependabotsweep

import (
	"fmt"

	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/gh"
	"github.com/trento-project/trento-workflows/internal/workflows/common/release"
	"github.com/trento-project/trento-workflows/internal/workflows/fixprci"
)

// RepoServiceName is the Restate service identifier used by the
// top-level dependabot-sweep workflow to fan out one Process call per
// repo. The service is keyed on the repo string so concurrent
// invocations on the same repo serialize.
const RepoServiceName = "trento.dependabot-sweep.repo"

// RepoInput is the wire-shape of one Process invocation.
type RepoInput struct {
	Repo               string `json:"repo"` // owner/name
	MaxIterationsPerPR int    `json:"maxIterationsPerPR"`
	DryRun             bool   `json:"dryRun"`
}

// RegisterRepoService returns the Restate service definition. The
// handler binary binds this alongside the dependabot-sweep workflow.
func RegisterRepoService() restate.ServiceDefinition {
	return restate.NewService(RepoServiceName).
		Handler("Process", restate.NewServiceHandler(Process))
}

// Process is the per-repo handler. Never returns a non-nil error —
// per-PR failures are captured in the PROutcome and per-repo failures
// in report.Errors so the top-level workflow can keep processing other
// repos. A non-nil error here would propagate to the caller's
// RequestFuture and we'd lose the partial report.
func Process(ctx restate.Context, in RepoInput) (RepoReport, error) {
	return processRepo(ctx, in), nil
}

// processRepo runs the per-repo sweep flow. Errors are recorded in
// the report rather than aborting.
func processRepo(ctx restate.Context, in RepoInput) RepoReport {
	report := RepoReport{Repo: in.Repo}

	current, next, ok := release.ResolveVersions(ctx, in.Repo)
	if !ok {
		report.Errors = append(report.Errors, "could not resolve release version")
		return report
	}
	report.CurrentVersion = current
	report.NextVersion = next
	report.Milestone = next

	prs, err := listDependabotPRs(ctx, in.Repo)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("list dependabot PRs: %s", err.Error()))
		return report
	}
	if len(prs) == 0 {
		report.Skipped = true
		return report
	}

	if _, err := runT(ctx, "milestoneEnsure:"+in.Repo, func(rctx restate.RunContext) (int64, error) {
		return gh.MilestoneEnsure(rctx, in.Repo, next)
	}); err != nil {
		// Non-fatal: the merge will surface the missing milestone as an
		// error on each PR. Record and continue.
		report.Errors = append(report.Errors, fmt.Sprintf("milestone ensure: %s", err.Error()))
	}

	for _, pr := range prs {
		outcome := processPR(ctx, in, pr, next)
		report.PRs = append(report.PRs, outcome)
	}
	return report
}

// listDependabotPRs returns the open Dependabot PRs on `repo`.
func listDependabotPRs(ctx restate.Context, repo string) ([]gh.PR, error) {
	return runT(ctx, "prList:"+repo, func(rctx restate.RunContext) ([]gh.PR, error) {
		return gh.PRList(rctx, gh.PRListOpts{
			Repo:   repo,
			State:  "open",
			Search: "author:app/dependabot",
			Limit:  200,
		})
	})
}

// processPR is the per-PR sweep loop. The exact ordering matches the
// spec's pseudocode: milestone first (so a crash mid-run still leaves
// the PR labelled), then fix-pr-ci, then merge-or-skip.
func processPR(ctx restate.Context, in RepoInput, pr gh.PR, next string) PROutcome {
	outcome := PROutcome{
		Number:    pr.Number,
		URL:       pr.URL,
		Title:     pr.Title,
		Milestone: next,
	}

	// 1. Attach the milestone up front, regardless of CI state. Doing
	//    it before merging means a mid-run crash still leaves the PR
	//    labelled for the correct release.
	if !in.DryRun {
		_ = runV(ctx, fmt.Sprintf("milestone:%s:%d", in.Repo, pr.Number), func(rctx restate.RunContext) error {
			return gh.PRSetMilestone(rctx, in.Repo, pr.Number, next)
		})
	}

	// 2. Delegate to fix-pr-ci. It handles the "green fast-path"
	//    itself: if all CI runs are already terminal-success, it
	//    returns FinalStatusGreen after one iteration without ever
	//    asking Claude. So the sweep always dispatches unconditionally
	//    and trusts fix-pr-ci to short-circuit.
	key := fmt.Sprintf("%s#%d", in.Repo, pr.Number)
	fix := restate.Workflow[fixprci.Output](ctx, fixprci.WorkflowName, key, "Run")
	result, err := fix.Request(fixprci.Input{
		Repo:          in.Repo,
		PRNumber:      pr.Number,
		MaxIterations: in.MaxIterationsPerPR,
	})
	if err != nil {
		outcome.State = PRStateFailedInternal
		outcome.Note = fmt.Sprintf("fix-pr-ci call failed: %s", err.Error())
		return outcome
	}
	outcome.Iterations = result.Iterations

	switch result.FinalStatus {
	case fixprci.FinalStatusGreen:
		if in.DryRun {
			outcome.State = PRStateSkippedDryRun
			outcome.Note = "would merge; DryRun=true"
			return outcome
		}
		if !mergeableNow(ctx, in.Repo, pr.Number) {
			outcome.State = PRStateSkippedNotReady
			outcome.Note = "mergeable_state blocked/dirty/behind"
			return outcome
		}
		if err := runV(ctx, fmt.Sprintf("merge:%s:%d", in.Repo, pr.Number), func(rctx restate.RunContext) error {
			return gh.PRMerge(rctx, in.Repo, pr.Number, gh.PRMergeMethodSquash)
		}); err != nil {
			outcome.State = PRStateFailedInternal
			outcome.Note = fmt.Sprintf("gh pr merge failed: %s", err.Error())
			return outcome
		}
		if len(result.Commits) == 0 {
			outcome.State = PRStateMergedClean
		} else {
			outcome.State = PRStateMergedAfterFix
		}
		return outcome
	case fixprci.FinalStatusExhausted, fixprci.FinalStatusAborted:
		outcome.State = PRStateLeftForHuman
		outcome.Note = fmt.Sprintf("fix-pr-ci %s after %d iters", result.FinalStatus, result.Iterations)
		return outcome
	default:
		outcome.State = PRStateFailedInternal
		outcome.Note = fmt.Sprintf("unknown fix-pr-ci status: %q", result.FinalStatus)
		return outcome
	}
}

// mergeableNow returns true when the PR's mergeable_state is "clean".
// Other values (dirty, behind, blocked, has_hooks, unknown) and any
// gh error skip the merge — we don't try to rebase or unblock in v1.
func mergeableNow(ctx restate.Context, repo string, prNumber int) bool {
	state, err := runT(ctx, fmt.Sprintf("mergeable:%s:%d", repo, prNumber), func(rctx restate.RunContext) (string, error) {
		return gh.PRMergeableState(rctx, repo, prNumber)
	})
	if err != nil || state == "" {
		// "" = API returned null/unknown; treat as not-ready and skip.
		return false
	}
	return state == "clean"
}
