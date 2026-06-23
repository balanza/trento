// Package fixprci implements the "fix-pr-ci" workflow: watch a PR's
// GitHub Actions CI, classify failures with Claude into flaky/bug/
// infra/unfixable categories, rerun flakies, fix bugs autonomously
// (commit + push as fixup), loop until green or exhausted.
//
// Design: docs/superpowers/specs/2026-06-17-fix-pr-ci-workflow-design.md
package fixprci

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/claude"
	"github.com/trento-project/trento-workflows/internal/activities/fs"
	"github.com/trento-project/trento-workflows/internal/activities/gh"
	"github.com/trento-project/trento-workflows/internal/activities/git"
	"github.com/trento-project/trento-workflows/internal/lib"
)

// WorkflowName is the Restate service identifier.
const WorkflowName = "trento.fix-pr-ci"

//go:embed prompts/analyze-logs.md
var analyzeLogsTmpl string

//go:embed prompts/fix-bug.md
var fixBugTmpl string

//go:embed prompts/resolve-rebase-conflict.md
var resolveRebaseConflictTmpl string

// notACodeFixSentinel is the literal the fix-bug prompt emits when the
// issue isn't addressable from inside the repo. The workflow uses it to
// reclassify the issue as unfixable so it isn't retried.
const notACodeFixSentinel = "NOT_A_CODE_FIX"

// fixBugLogLines is how many trailing lines of each relevant job log we
// pass to the fix-bug prompt. Keeps the prompt focused on the failure
// site rather than re-feeding the whole CI run.
const fixBugLogLines = 200

// postBaseSyncSettle is how long we wait after a rebase-onto-base push
// before the next iteration polls CI. Force-pushing a rewritten PR
// branch invalidates the previous SHA's CI runs and triggers fresh
// runs for the new SHA; this settle gives GitHub Actions a moment to
// schedule them so the next poll doesn't see "no runs → green" by
// race.
const postBaseSyncSettle = 30 * time.Second

// Register returns the Restate workflow definition. The handler binary
// binds this into its endpoint.
func Register() restate.ServiceDefinition {
	return restate.NewWorkflow(WorkflowName).
		Handler("Run", restate.NewWorkflowHandler(Run))
}

// Run is the workflow entrypoint. Virtual-object key:
// fmt.Sprintf("%s#%d", in.Repo, in.PRNumber). Concurrent invocations on
// the same (repo, prNumber) serialize.
//
// High-level flow:
//  1. Clone the repo + checkout the PR head into a fresh workDir.
//  2. Loop iter = 1..MaxIterations:
//     - refresh head SHA, wait for every CI run to be terminal
//     - if all green, return FinalStatusGreen
//     - fetch failed-job logs, ask Claude to classify
//     - increment attempts per issueKey, drop exhausted/unfixable from active
//     - if no active issues remain, FinalStatusExhausted
//     - bugs → sequential fix prompts → lint+format → commit → push
//     - flaky/infra (only if nothing was pushed) → gh.RunRerunFailed
//     - no-progress detector: 2 consecutive no-op iterations → exhausted
//  3. Conditional cleanup, return Output.
func Run(ctx restate.WorkflowContext, in Input) (Output, error) {
	in = applyDefaults(in)
	if err := validator.New().Struct(in); err != nil {
		return Output{}, terminal(fmt.Errorf("input validation: %w", err))
	}

	out := Output{Repo: in.Repo, PRNumber: in.PRNumber}

	workDir, err := setupCheckout(ctx, in)
	if err != nil {
		out.FinalStatus = FinalStatusAborted
		return out, nil
	}
	defer maybeCleanup(ctx, workDir, in.CleanupOnExit)

	headRef, err := runT(ctx, "gh.PRHeadRef", func(rctx restate.RunContext) (string, error) {
		return gh.PRHeadRef(rctx, in.Repo, in.PRNumber)
	})
	if err != nil {
		out.FinalStatus = FinalStatusAborted
		return out, nil
	}

	attemptsByKey := map[string]int{}
	issuesByKey := map[string]*IssueOutcome{}
	noProgress := 0

	for iter := 1; iter <= in.MaxIterations; iter++ {
		out.Iterations = iter

		sha, err := runT(ctx, fmt.Sprintf("gh.PRHeadSHA:iter%d", iter), func(rctx restate.RunContext) (string, error) {
			return gh.PRHeadSHA(rctx, in.Repo, in.PRNumber)
		})
		if err != nil {
			out.FinalStatus = FinalStatusAborted
			finalizeIssueStates(&out, issuesByKey)
			return out, nil
		}

		switch syncWithBaseIfNeeded(ctx, workDir, in, headRef, &out, iter) {
		case baseSyncSkipped:
			// PR is mergeable against base (or state is unknown) — proceed.
		case baseSyncPushed:
			// Rebased + force-pushed onto base. New SHA exists upstream;
			// give GitHub a moment to schedule its CI before the next
			// iter's poll. Counts as progress (resets noProgress).
			_ = sleep(ctx, postBaseSyncSettle)
			noProgress = 0
			continue
		case baseSyncFailed:
			// Dirty PR + couldn't resolve. Bail.
			out.FinalStatus = FinalStatusAborted
			finalizeIssueStates(&out, issuesByKey)
			return out, nil
		}

		runs, err := waitForRuns(ctx, in.Repo, sha)
		if err != nil {
			out.FinalStatus = FinalStatusAborted
			finalizeIssueStates(&out, issuesByKey)
			return out, nil
		}
		if len(runs) == 0 {
			out.FinalStatus = FinalStatusGreen
			finalizeIssueStates(&out, issuesByKey)
			return out, nil
		}

		failed := failedRuns(runs)
		if len(failed) == 0 {
			out.FinalStatus = FinalStatusGreen
			finalizeIssueStates(&out, issuesByKey)
			return out, nil
		}

		entries, err := collectFailedJobLogs(ctx, in.Repo, failed)
		if err != nil || len(entries) == 0 {
			out.FinalStatus = FinalStatusAborted
			finalizeIssueStates(&out, issuesByKey)
			return out, nil
		}
		bundle := buildLogBundle(entries)

		commitMade := false
		reranSomething := false

		issues, ok := analyzeLogs(ctx, in, sha, bundle, iter)
		if ok {
			active := []issue{}
			for _, iss := range issues {
				attemptsByKey[iss.IssueKey]++
				recordIssue(issuesByKey, iss, attemptsByKey[iss.IssueKey])
				if attemptsByKey[iss.IssueKey] > in.MaxAttempts {
					issuesByKey[iss.IssueKey].FinalState = "exhausted"
					continue
				}
				if iss.Category == "unfixable" {
					issuesByKey[iss.IssueKey].FinalState = "unfixable"
					continue
				}
				active = append(active, iss)
			}
			if len(active) == 0 {
				out.FinalStatus = FinalStatusExhausted
				finalizeIssueStates(&out, issuesByKey)
				return out, nil
			}

			bugs, rerunable := splitIssuesByAction(active)

			if len(bugs) > 0 {
				edited := applyBugFixes(ctx, workDir, bugs, entries, issuesByKey)
				if edited && runLintFormat(ctx, in.Repo, workDir) {
					if commitAndPush(ctx, workDir, headRef, &out) {
						commitMade = true
					}
				}
			}

			// A push creates a new SHA whose CI re-runs everything anyway,
			// so explicit reruns are only needed when nothing was pushed.
			if !commitMade && len(rerunable) > 0 {
				for _, runID := range uniqueRunIDsFromIssues(rerunable) {
					label := fmt.Sprintf("gh.RunRerunFailed:%d", runID)
					if err := runV(ctx, label, func(rctx restate.RunContext) error {
						return gh.RunRerunFailed(rctx, in.Repo, runID)
					}); err == nil {
						reranSomething = true
					}
				}
			}
		}

		if !commitMade && !reranSomething {
			noProgress++
			if noProgress >= 2 {
				out.FinalStatus = FinalStatusExhausted
				finalizeIssueStates(&out, issuesByKey)
				return out, nil
			}
		} else {
			noProgress = 0
		}
	}

	out.FinalStatus = FinalStatusExhausted
	finalizeIssueStates(&out, issuesByKey)
	return out, nil
}

// setupCheckout clones the repo into a fresh workDir and checks out the
// PR's head branch. Returns the workDir path on success.
func setupCheckout(ctx restate.Context, in Input) (string, error) {
	workDir, err := runT(ctx, "fs.MkTempDir", func(rctx restate.RunContext) (string, error) {
		return fs.MkTempDir(rctx, "fixprci-")
	})
	if err != nil {
		return "", err
	}
	if err := runV(ctx, "gh.RepoClone:"+in.Repo, func(rctx restate.RunContext) error {
		return gh.RepoClone(rctx, in.Repo, workDir)
	}); err != nil {
		return "", err
	}
	if err := runV(ctx, fmt.Sprintf("gh.PRCheckout:%d", in.PRNumber), func(rctx restate.RunContext) error {
		return gh.PRCheckout(rctx, workDir, in.PRNumber)
	}); err != nil {
		return "", err
	}
	return workDir, nil
}

// maybeCleanup removes workDir when in.CleanupOnExit is true. Best-effort
// — failures to remove the tree are journaled but not surfaced.
func maybeCleanup(ctx restate.Context, workDir string, do bool) {
	if !do || workDir == "" {
		return
	}
	_ = runV(ctx, "cleanup:"+workDir, func(_ restate.RunContext) error {
		return os.RemoveAll(workDir)
	})
}

// collectFailedJobLogs walks each failed run, fetches its jobs, filters
// the ones that themselves ended in failure/cancelled/timed_out, and
// pulls each job's log. Returns the bundle entries in (run, job) order.
//
// Sequential rather than parallel: Restate's workflow loop is
// single-coroutine; parallel fetches would need RunAsync + WaitFirst
// for marginal latency wins on a typical "≤ a few dozen failed jobs"
// workload.
func collectFailedJobLogs(ctx restate.Context, repo string, runs []gh.WorkflowRun) ([]logEntry, error) {
	out := []logEntry{}
	for _, r := range runs {
		jobs, err := runT(ctx, fmt.Sprintf("gh.JobsForRun:%d", r.ID), func(rctx restate.RunContext) ([]gh.WorkflowJob, error) {
			return gh.JobsForRun(rctx, repo, r.ID)
		})
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if j.Status != "completed" {
				continue
			}
			switch j.Conclusion {
			case "failure", "cancelled", "timed_out":
				// proceed to fetch the log
			default:
				continue
			}
			log, err := runT(ctx, fmt.Sprintf("gh.JobLogs:%d", j.ID), func(rctx restate.RunContext) (string, error) {
				return gh.JobLogs(rctx, repo, j.ID)
			})
			if err != nil {
				// best-effort: one missing log shouldn't abort the iter.
				continue
			}
			out = append(out, logEntry{
				RunID: r.ID, JobID: j.ID,
				Name: j.Name, Conclusion: j.Conclusion, Log: log,
			})
		}
	}
	return out, nil
}

// analyzeLogs renders the analyze-logs prompt with the iteration's log
// bundle and asks Claude to classify failures into issues.
func analyzeLogs(ctx restate.Context, in Input, sha, bundle string, iter int) ([]issue, bool) {
	prompt := strings.NewReplacer(
		"{{repo}}", in.Repo,
		"{{prNumber}}", fmt.Sprintf("%d", in.PRNumber),
		"{{headSHA}}", sha,
		"{{logBundle}}", bundle,
	).Replace(analyzeLogsTmpl)

	resp, err := runT(ctx, fmt.Sprintf("claude.analyzeLogs:iter%d", iter), func(rctx restate.RunContext) (claude.Response, error) {
		return claude.Invoke(rctx, claude.Request{
			Prompt:       prompt,
			AllowedTools: []string{"Read"},
		})
	})
	if err != nil {
		return nil, false
	}
	return parseAnalysis(resp.Text)
}

// recordIssue creates or updates the per-key issue record. FinalState
// is filled in by callers (loop body sets exhausted/unfixable;
// applyBugFixes sets fixed-pending-ci/unfixable; finalizeIssueStates
// fills in defaults at the end).
func recordIssue(byKey map[string]*IssueOutcome, iss issue, attempts int) {
	cur, ok := byKey[iss.IssueKey]
	if !ok {
		cur = &IssueOutcome{IssueKey: iss.IssueKey}
		byKey[iss.IssueKey] = cur
	}
	cur.Category = iss.Category
	cur.Summary = iss.Summary
	cur.JobRefs = iss.JobRefs
	cur.Attempts = attempts
}

// splitIssuesByAction partitions active issues into {bugs, rerunable}.
// Bugs go through the fix-bug prompt; rerunable (flaky + infra) trigger
// gh.RunRerunFailed when no bug fix was committed this round.
func splitIssuesByAction(active []issue) (bugs, rerunable []issue) {
	for _, iss := range active {
		if iss.Category == "bug" {
			bugs = append(bugs, iss)
		} else {
			rerunable = append(rerunable, iss)
		}
	}
	return bugs, rerunable
}

// finalizeIssueStates assigns default FinalState values for issues that
// were seen but never moved to a terminal state, then copies the map
// into out.Issues sorted by IssueKey for deterministic Output.
func finalizeIssueStates(out *Output, byKey map[string]*IssueOutcome) {
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	// inline insertion sort — N is tiny (handful of issues per run).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	out.Issues = make([]IssueOutcome, 0, len(keys))
	for _, k := range keys {
		v := byKey[k]
		if v.FinalState == "" {
			switch v.Category {
			case "flaky":
				v.FinalState = "still-flaky"
			case "infra":
				v.FinalState = "rerun-only"
			case "bug":
				v.FinalState = "exhausted"
			case "unfixable":
				v.FinalState = "unfixable"
			default:
				v.FinalState = "exhausted"
			}
		}
		out.Issues = append(out.Issues, *v)
	}
}

// applyBugFixes runs the fix-bug prompt for each bug sequentially.
// Returns true if any prompt left the working tree dirty (an agent
// actually edited something). NOT_A_CODE_FIX responses reclassify the
// issue to unfixable so it isn't retried.
func applyBugFixes(
	ctx restate.Context,
	workDir string,
	bugs []issue,
	entries []logEntry,
	byKey map[string]*IssueOutcome,
) bool {
	edited := false
	for _, b := range bugs {
		dirtyBefore, _ := runT(ctx, "git.Dirty:before:"+b.IssueKey, func(rctx restate.RunContext) (bool, error) {
			return git.Dirty(rctx, workDir)
		})

		prompt := strings.NewReplacer(
			"{{repo}}", workDirRepoLabel(workDir),
			"{{issueKey}}", b.IssueKey,
			"{{summary}}", b.Summary,
			"{{hint}}", b.Hint,
			"{{relevantLogExcerpt}}", excerptForIssue(entries, b, fixBugLogLines),
		).Replace(fixBugTmpl)

		resp, err := runT(ctx, "claude.fixBug:"+b.IssueKey, func(rctx restate.RunContext) (claude.Response, error) {
			return claude.Invoke(rctx, claude.Request{
				Prompt:       prompt,
				AllowedTools: []string{"Bash", "Read", "Edit", "Write"},
				Cwd:          workDir,
			})
		})
		if err != nil {
			continue
		}
		if strings.TrimSpace(resp.Text) == notACodeFixSentinel {
			if v, ok := byKey[b.IssueKey]; ok {
				v.FinalState = "unfixable"
			}
			continue
		}

		dirtyAfter, _ := runT(ctx, "git.Dirty:after:"+b.IssueKey, func(rctx restate.RunContext) (bool, error) {
			return git.Dirty(rctx, workDir)
		})
		if !dirtyBefore && dirtyAfter {
			edited = true
			if v, ok := byKey[b.IssueKey]; ok {
				v.FinalState = "fixed-pending-ci"
			}
		}
	}
	return edited
}

// workDirRepoLabel returns a short label for the workDir to embed in
// the fix-bug prompt's `{{repo}}` placeholder. We pass the workDir
// itself (the agent already sees this as its cwd); a richer label
// could fetch the upstream `repo` string but isn't worth the
// extra activity call.
func workDirRepoLabel(workDir string) string {
	return workDir
}

// runLintFormat dispatches the per-repo lint+format commands in
// workDir sequentially. Returns true iff every command exits 0. A
// non-zero exit short-circuits the rest and returns false — the commit
// is skipped this iteration and the lint failure shows up on the next
// CI run (which never happens because we don't push, so the no-progress
// detector eventually bails).
func runLintFormat(ctx restate.Context, repo, workDir string) bool {
	cmds := lintFormatCmds(repo)
	for i, argv := range cmds {
		label := fmt.Sprintf("lintFormat:%d:%s", i, argv[0])
		ok, err := runT(ctx, label, func(rctx restate.RunContext) (bool, error) {
			_, _, code, sErr := lib.Sh(rctx, workDir, argv...)
			if sErr != nil {
				return false, sErr
			}
			return code == 0, nil
		})
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// commitAndPush stages everything, makes a fixup commit, and pushes.
// Handles one non-fast-forward push rejection by rebasing on the
// remote head ref and retrying once. Returns true iff a commit was
// successfully pushed.
func commitAndPush(ctx restate.Context, workDir, headRef string, out *Output) bool {
	dirty, err := runT(ctx, "git.Dirty:precommit", func(rctx restate.RunContext) (bool, error) {
		return git.Dirty(rctx, workDir)
	})
	if err != nil || !dirty {
		return false
	}
	if err := runV(ctx, "git.Add:.", func(rctx restate.RunContext) error {
		return git.Add(rctx, workDir, []string{"."})
	}); err != nil {
		return false
	}
	sha, err := runT(ctx, "git.CommitFixup", func(rctx restate.RunContext) (string, error) {
		return git.CommitFixup(rctx, workDir)
	})
	if err != nil {
		return false
	}

	if pushErr := tryPush(ctx, workDir, headRef, "first"); pushErr == nil {
		out.Commits = append(out.Commits, sha)
		return true
	}

	// Non-FF rejection: fetch + rebase + (resolve conflict if any) + retry.
	if err := runV(ctx, "git.FetchOrigin:"+headRef, func(rctx restate.RunContext) error {
		return git.FetchOrigin(rctx, workDir, headRef)
	}); err != nil {
		return false
	}
	if !rebaseWithConflictResolution(ctx, workDir, "origin/"+headRef) {
		return false
	}
	if pushErr := tryPush(ctx, workDir, headRef, "retry"); pushErr != nil {
		return false
	}
	out.Commits = append(out.Commits, sha)
	return true
}

// baseSyncOutcome is the result of the per-iteration "is the PR
// conflicted with its base branch?" check. Returned as a typed enum
// (not a (bool, bool) tuple) so the workflow's switch on it stays
// readable.
type baseSyncOutcome int

const (
	baseSyncSkipped baseSyncOutcome = iota // not dirty, or state unknown
	baseSyncPushed                         // dirty → rebased + force-pushed
	baseSyncFailed                         // dirty → couldn't resolve, abort
)

// syncWithBaseIfNeeded checks if the PR is in `dirty` mergeable_state
// (i.e. it has merge conflicts with its base branch). When it is, the
// function fetches the base, rebases the PR head onto origin/<base>
// (with AI conflict resolution if the rebase itself conflicts), then
// force-with-lease pushes the rewritten head.
//
// Returns:
//   - baseSyncSkipped: state is anything other than `dirty` (or the API
//     hasn't computed it yet) — nothing to do.
//   - baseSyncPushed: the rebase + push succeeded. Caller should let
//     the new SHA settle, then `continue` to next iteration.
//   - baseSyncFailed: state was `dirty` but the rebase or AI couldn't
//     resolve. Caller maps to FinalStatusAborted.
//
// Why each iteration, not just at start: base can move during the
// workflow's lifetime (someone merges to base while we're fixing CI),
// and our own fixups can occasionally introduce a conflict against
// base. The API call is cheap (one gh REST hit).
func syncWithBaseIfNeeded(
	ctx restate.Context,
	workDir string, in Input, headRef string,
	out *Output, iter int,
) baseSyncOutcome {
	state, err := runT(ctx, fmt.Sprintf("gh.PRMergeableState:iter%d", iter), func(rctx restate.RunContext) (string, error) {
		return gh.PRMergeableState(rctx, in.Repo, in.PRNumber)
	})
	if err != nil || state != "dirty" {
		// API error or non-dirty state → skip. Errors don't block; the
		// next iteration retries the check.
		return baseSyncSkipped
	}

	baseRef, err := runT(ctx, "gh.PRBaseRef", func(rctx restate.RunContext) (string, error) {
		return gh.PRBaseRef(rctx, in.Repo, in.PRNumber)
	})
	if err != nil {
		return baseSyncFailed
	}
	if err := runV(ctx, "git.FetchOrigin:base:"+baseRef, func(rctx restate.RunContext) error {
		return git.FetchOrigin(rctx, workDir, baseRef)
	}); err != nil {
		return baseSyncFailed
	}
	if !rebaseWithConflictResolution(ctx, workDir, "origin/"+baseRef) {
		return baseSyncFailed
	}
	if err := runV(ctx, fmt.Sprintf("git.PushBranch:%s:base-sync:iter%d", headRef, iter), func(rctx restate.RunContext) error {
		return git.PushBranch(rctx, workDir, headRef, true) // force-with-lease — rebase rewrote history
	}); err != nil {
		return baseSyncFailed
	}
	if sha, herr := runT(ctx, fmt.Sprintf("git.Head:after-base-sync:iter%d", iter), func(rctx restate.RunContext) (string, error) {
		return git.Head(rctx, workDir)
	}); herr == nil && sha != "" {
		out.Commits = append(out.Commits, "base-sync:"+sha)
	}
	return baseSyncPushed
}

// rebaseOutcome is the typed result of a rebase attempt. Returned from
// inside an activity so the conflict-handling branching survives
// Restate's journal round-trip (errors lose sentinel identity through
// serialization).
type rebaseOutcome int

const (
	rebaseClean    rebaseOutcome = iota // succeeded outright
	rebaseConflict                      // stopped on merge conflict; worktree mid-rebase
	rebaseFailed                        // hard failure (git couldn't start, etc.)
)

// rebaseWithConflictResolution wraps `git rebase ontoRef` with an
// AI-driven resolution step. Returns true iff the worktree ends with a
// completed (or never-needed) rebase, ready for the caller to push.
//
// Flow:
//  1. git.Rebase. Clean → done. Hard failure → bail.
//  2. Conflict → invoke Claude to resolve.
//  3. After Claude: if rebase still in progress → abort + bail.
//     If rebase no longer in progress → AI completed it → push-ready.
func rebaseWithConflictResolution(ctx restate.Context, workDir, ontoRef string) bool {
	outcome, _ := runT(ctx, "git.Rebase:"+ontoRef, func(rctx restate.RunContext) (rebaseOutcome, error) {
		err := git.Rebase(rctx, workDir, ontoRef)
		if err == nil {
			return rebaseClean, nil
		}
		if errors.Is(err, git.ErrRebaseConflict) {
			return rebaseConflict, nil
		}
		return rebaseFailed, err
	})
	switch outcome {
	case rebaseClean:
		return true
	case rebaseFailed:
		return false
	case rebaseConflict:
		if resolveRebaseConflictWithAI(ctx, workDir, ontoRef) {
			return true
		}
		_ = runV(ctx, "git.RebaseAbort", func(rctx restate.RunContext) error {
			return git.RebaseAbort(rctx, workDir)
		})
		return false
	}
	return false
}

// resolveRebaseConflictWithAI dispatches a Claude call with the
// resolve-rebase-conflict prompt. The agent edits the conflicted
// files, stages them, and runs `git rebase --continue` itself. We then
// verify the rebase is no longer in progress as proof of success.
func resolveRebaseConflictWithAI(ctx restate.Context, workDir, ontoRef string) bool {
	prompt := strings.NewReplacer(
		"{{ontoRef}}", ontoRef,
	).Replace(resolveRebaseConflictTmpl)

	if _, err := runT(ctx, "claude.resolveRebaseConflict:"+ontoRef, func(rctx restate.RunContext) (claude.Response, error) {
		return claude.Invoke(rctx, claude.Request{
			Prompt:       prompt,
			AllowedTools: []string{"Bash", "Read", "Edit", "Write"},
			Cwd:          workDir,
		})
	}); err != nil {
		return false
	}
	inProgress, err := runT(ctx, "git.RebaseInProgress", func(rctx restate.RunContext) (bool, error) {
		return git.RebaseInProgress(rctx, workDir)
	})
	if err != nil {
		return false
	}
	return !inProgress
}

// tryPush wraps git.PushBranch with a label that includes a tag so
// "first" and "retry" attempts journal as distinct steps.
func tryPush(ctx restate.Context, workDir, headRef, tag string) error {
	return runV(ctx, "git.PushBranch:"+headRef+":"+tag, func(rctx restate.RunContext) error {
		return git.PushBranch(rctx, workDir, headRef, false)
	})
}
