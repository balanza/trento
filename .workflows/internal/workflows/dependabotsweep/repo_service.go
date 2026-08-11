package dependabotsweep

import (
	"fmt"
	"strings"

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

	// Phase 1 — submit milestones + fix-pr-ci for every PR.
	// Send is non-blocking: after this loop, all fix-pr-ci
	// workflows are executing concurrently (each keyed on a
	// different repo+PR, so Restate runs them in parallel).
	type prWork struct {
		pr       gh.PR
		attached restate.AttachFuture[fixprci.Output]
	}
	works := make([]prWork, 0, len(prs))
	for _, pr := range prs {
		attached := submitPR(ctx, in, pr, next)
		works = append(works, prWork{pr, attached})
	}

	// Phase 2 — collect results and merge/skip. Response() blocks
	// on each PR's fix-pr-ci, but since all were submitted in Phase 1,
	// the total wall time is bounded by the slowest workflow, not the
	// sum.
	for _, w := range works {
		outcome := collectPR(ctx, in, w.pr, next, w.attached)
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

// submitPR attaches the milestone and submits fix-pr-ci (non-blocking).
// Returns an AttachFuture that the caller awaits later.
func submitPR(ctx restate.Context, in RepoInput, pr gh.PR, next string) restate.AttachFuture[fixprci.Output] {
	// 1. Attach the milestone up front, but only if the PR includes at
	//    least one file outside /test and /.github (i.e. it touches
	//    code that actually participates in the release bundle).
	if !in.DryRun && hasNonBundleChanges(ctx, in.Repo, pr) {
		_ = runV(ctx, fmt.Sprintf("milestone:%s:%d", in.Repo, pr.Number), func(rctx restate.RunContext) error {
			return gh.PRSetMilestone(rctx, in.Repo, pr.Number, next)
		})
	}

	// 2. Submit fix-pr-ci (non-blocking Send). The workflow starts
	//    executing immediately; we'll attach and await its result in
	//    collectPR.
	key := fmt.Sprintf("%s#%d", in.Repo, pr.Number)
	input := fixprci.Input{
		Repo:          in.Repo,
		PRNumber:      pr.Number,
		MaxIterations: in.MaxIterationsPerPR,
	}
	inv := restate.WorkflowSend(ctx, fixprci.WorkflowName, key, "Run").Send(input)
	return restate.AttachInvocation[fixprci.Output](ctx, inv.GetInvocationId())
}

// collectPR awaits the fix-pr-ci result and applies merge-or-skip.
func collectPR(ctx restate.Context, in RepoInput, pr gh.PR, next string, attached restate.AttachFuture[fixprci.Output]) PROutcome {
	outcome := PROutcome{
		Number:    pr.Number,
		URL:       pr.URL,
		Title:     pr.Title,
		Milestone: next,
	}

	result, err := attached.Response()
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
		merged, mergeNote := tryMergeWithApproval(ctx, in.Repo, pr.Number)
		if mergeNote != "" {
			outcome.Note = mergeNote
		}
		if !merged {
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

// tryMergeWithApproval drives the merge step with an approval-recovery
// path for the "mergeable_state=blocked" case. The state machine:
//
//   initial state        | approve? | post-approval state | outcome
//   ---------------------+----------+---------------------+-------------------------------
//   "clean"              | n/a      | n/a                 | mergeNow
//   "blocked"            | yes      | "clean"             | mergeNow
//   "blocked"            | yes      | "blocked"           | SkippedNotReady (more reviews)
//   "blocked"            | no (err) | n/a                 | SkippedNotReady (approval failed)
//   "dirty"              | n/a      | n/a                 | SkippedNotReady (rebase needed)
//   "behind"             | n/a      | n/a                 | SkippedNotReady (rebase needed)
//   "unstable"           | n/a      | n/a                 | SkippedNotReady (CI still failing)
//   "" (unknown)         | n/a      | n/a                 | SkippedNotReady (state not yet computed)
//   any other value      | n/a      | n/a                 | SkippedNotReady
//
// The check is split into two pure helpers (classifyMergeState and
// classifyPostApprovalState) so the state machine is unit-testable
// without running gh.
//
// When approval is applied but the post-approval state is still
// "blocked", the PR typically needs additional reviewers (e.g.
// CODEOWNERS). The approval the sweep applied is still recorded on
// the PR — it just doesn't satisfy the protection rule. The release
// manager sees the milestone-attached PR in the report.
func tryMergeWithApproval(ctx restate.Context, repo string, prNumber int) (merged bool, note string) {
	state, err := runT(ctx, fmt.Sprintf("mergeable:%s:%d", repo, prNumber), func(rctx restate.RunContext) (string, error) {
		return gh.PRMergeableState(rctx, repo, prNumber)
	})
	if err != nil {
		return false, fmt.Sprintf("mergeable_state check failed: %s", err.Error())
	}

	switch classifyMergeState(state) {
	case mergeNow:
		return doMerge(ctx, repo, prNumber), ""
	case approveThenMerge:
		// CI is green per fix-pr-ci; "blocked" means a non-CI branch
		// protection rule (most often "required reviews"). Approve
		// via the user's gh auth and re-check.
		approveErr := runV(ctx, fmt.Sprintf("approve:%s:%d", repo, prNumber), func(rctx restate.RunContext) error {
			return gh.PRApprove(rctx, repo, prNumber)
		})
		if approveErr != nil {
			return false, fmt.Sprintf("mergeable_state=blocked, approval failed: %s", approveErr.Error())
		}
		newState, err := runT(ctx, fmt.Sprintf("mergeableAfterApprove:%s:%d", repo, prNumber), func(rctx restate.RunContext) (string, error) {
			return gh.PRMergeableState(rctx, repo, prNumber)
		})
		if err != nil {
			return false, fmt.Sprintf("approval applied, post-approval state check failed: %s", err.Error())
		}
		if classifyPostApprovalState(newState) == mergeNow {
			return doMerge(ctx, repo, prNumber), ""
		}
		return false, classifyPostApprovalNote(newState)
	case skipNotReady:
		return false, classifyInitialNote(state)
	}
	return false, classifyInitialNote(state)
}

// mergeAction is the pure decision the workflow applies given the
// mergeable state. Extracted for unit testing — see
// repo_service_test.go.
type mergeAction int

const (
	mergeNow mergeAction = iota
	approveThenMerge
	skipNotReady
)

// classifyMergeState maps a freshly-fetched PR mergeable_state to the
// action the workflow should take. Per the GitHub REST API docs,
// "blocked" is a non-CI branch-protection requirement (reviews,
// signed commits, etc.); "unstable" is a CI failure; "dirty" /
// "behind" are merge conflicts / out-of-date; the empty string means
// the API has not yet computed the state.
func classifyMergeState(state string) mergeAction {
	switch state {
	case "clean":
		return mergeNow
	case "blocked":
		return approveThenMerge
	default:
		// "dirty", "behind", "unstable", "has_hooks", "" (unknown),
		// and any future value the API may add.
		return skipNotReady
	}
}

// classifyPostApprovalState maps a re-fetched mergeable_state (after
// the sweep applied an approval) to the action the workflow should
// take. The only "go ahead" case is "clean"; everything else
// (including "blocked", which means additional reviewers are still
// required) leaves the PR unmerged for a human.
func classifyPostApprovalState(state string) mergeAction {
	if state == "clean" {
		return mergeNow
	}
	return skipNotReady
}

// classifyInitialNote builds the human-readable reason for skipping a
// PR on the initial mergeable_state check (no approval attempted).
func classifyInitialNote(state string) string {
	switch state {
	case "":
		return "mergeable_state unknown (API not yet computed)"
	case "dirty":
		return "mergeable_state=dirty (branch needs rebase)"
	case "behind":
		return "mergeable_state=behind (branch needs rebase)"
	case "unstable":
		return "mergeable_state=unstable (CI still failing — fix-pr-ci should have caught this)"
	case "has_hooks":
		return "mergeable_state=has_hooks (pre-receive hooks configured — needs human)"
	default:
		return fmt.Sprintf("mergeable_state=%q (not actionable)", state)
	}
}

// classifyPostApprovalNote builds the human-readable reason for
// skipping a PR after the sweep applied an approval but the
// post-approval state was still not "clean".
func classifyPostApprovalNote(state string) string {
	switch state {
	case "blocked":
		return "approval applied but additional reviews required (e.g. CODEOWNERS)"
	case "behind":
		return "approval applied but branch is now behind base"
	case "dirty":
		return "approval applied but branch now has merge conflicts"
	case "unstable":
		return "approval applied but a CI check is still failing"
	case "":
		return "approval applied but mergeable_state not yet computed"
	default:
		return fmt.Sprintf("approval applied but mergeable_state=%q", state)
	}
}

// hasNonBundleChanges returns true if the PR modifies at least one file
// outside the /test and /.github directories — i.e. it touches code that
// participates in the release bundle. Dependabot PRs that only touch
// test fixtures or workflow configs are ignored for milestone purposes.
func hasNonBundleChanges(ctx restate.Context, repo string, pr gh.PR) bool {
	files, err := runT(ctx, fmt.Sprintf("prFiles:%s:%d", repo, pr.Number), func(rctx restate.RunContext) ([]string, error) {
		return gh.PRFiles(rctx, repo, pr.Number)
	})
	if err != nil || len(files) == 0 {
		// If we can't fetch files (or there are none), err on the side
		// of caution and assume the PR matters — attach the milestone.
		return true
	}
	for _, f := range files {
		if !strings.HasPrefix(f, "test/") && !strings.HasPrefix(f, ".github/") {
			return true
		}
	}
	return false
}

func doMerge(ctx restate.Context, repo string, prNumber int) bool {
	if err := runV(ctx, fmt.Sprintf("merge:%s:%d", repo, prNumber), func(rctx restate.RunContext) error {
		return gh.PRMerge(rctx, repo, prNumber, gh.PRMergeMethodSquash)
	}); err != nil {
		// Surfaced via the surrounding caller's state — the merge
		// attempt's failure already overrides SkippedNotReady.
		return false
	}
	return true
}
