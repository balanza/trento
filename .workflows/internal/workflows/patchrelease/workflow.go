// Package patchrelease implements the "patch-release" workflow: for
// every super-repo submodule (or a filtered subset), backport the
// merged main PRs whose milestone matches the next patch version onto
// the release branch, then open a version-bump PR that triggers the
// release.
//
// In the v1 scaffold every activity is a no-op stub, so a run completes
// instantly with empty reports.
package patchrelease

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/claude"
	"github.com/trento-project/trento-workflows/internal/activities/fs"
	"github.com/trento-project/trento-workflows/internal/activities/gh"
	"github.com/trento-project/trento-workflows/internal/activities/git"
	"github.com/trento-project/trento-workflows/internal/workflows/common/release"
	"github.com/trento-project/trento-workflows/internal/workflows/fixprci"
)

// WorkflowName is the Restate service identifier.
const WorkflowName = "trento.patch-release"

const (
	labelBackport       = "backport-as-hotfix"
	labelBackportColor  = "e4e669"
	labelBackportDesc   = "Cherry-pick backport to release branch"
	labelSkipNotes      = "skip-release-notes"
	labelSkipNotesColor = "ffffff"
	labelSkipNotesDesc  = "Exclude this PR from the generated changelog"

	baseRelease      = "release"
	baseMain         = "main"
	originReleaseRef = "origin/release"
	prListPageMax    = 1000
)

// Register returns the Restate workflow definition. The handler binary
// binds this into its endpoint.
func Register() restate.ServiceDefinition {
	return restate.NewWorkflow(WorkflowName).
		Handler("Run", restate.NewWorkflowHandler(Run))
}

// Run is the workflow entrypoint. Restate keys the invocation by user
// choice (no per-input key requirement); concurrent invocations on the
// same key serialize.
//
// Repos are processed in parallel: the workflow fires one
// `trento.patch-release.repo`/Process call per repo, each running in
// the same handler binary but on its own coroutine, then awaits all
// futures and aggregates the reports.
func Run(ctx restate.WorkflowContext, in Input) (Output, error) {
	in = applyDefaults(in)

	repoRoot, err := release.ResolveRepoRoot(ctx)
	if err != nil {
		return Output{}, err
	}

	repos, err := release.ResolveRepos(ctx, repoRoot, in.Submodules)
	if err != nil {
		return Output{}, err
	}

	// Fan out: one async call per repo. The futures share the journal
	// but each runs independently on the service side.
	type pending struct {
		repo string
		fut  restate.ResponseFuture[RepoReport]
	}
	futures := make([]pending, 0, len(repos))
	for _, repo := range repos {
		client := restate.Service[RepoReport](ctx, RepoServiceName, "Process")
		futures = append(futures, pending{
			repo: repo,
			fut:  client.RequestFuture(RepoInput{Repo: repo}),
		})
	}

	// Collect results in the order the repos were submitted.
	var out Output
	for _, p := range futures {
		report, err := p.fut.Response()
		if err != nil {
			report = RepoReport{
				Repo:    p.repo,
				Skipped: false,
				Failed:  []string{fmt.Sprintf("service call failed: %s", err.Error())},
			}
		}
		out.Reports = append(out.Reports, report)
		out.PRs = append(out.PRs, report.Backports...)
		if report.VersionBumpPR != "" {
			out.PRs = append(out.PRs, report.VersionBumpPR)
		}
		out.FailedPicks = append(out.FailedPicks, report.Failed...)
	}
	return out, nil
}

// processRepo runs the per-repo patch-release flow. Errors are recorded
// in the report rather than aborting the workflow — one bad repo should
// not stop the others.
func processRepo(ctx restate.Context, repo string) RepoReport {
	report := RepoReport{
		Repo: repo, Skipped: false, CurrentVersion: "", NextVersion: "",
		Backports: nil, VersionBumpPR: "", Failed: nil,
	}

	current, next, ok := release.ResolveVersions(ctx, repo)
	if !ok {
		return report
	}
	report.CurrentVersion = current
	report.NextVersion = next

	ensureLabels(ctx, repo)

	prs, err := listMilestonePRs(ctx, repo, next)
	if err != nil {
		return report
	}
	if len(prs) == 0 {
		report.Skipped = true
		return report
	}

	workDir, ok := prepareClone(ctx, repo)
	if !ok {
		return report
	}

	backportedNums := backportAllPRs(ctx, repo, workDir, next, prs, &report)
	report.VersionBumpPR = createVersionBumpPR(ctx, repo, workDir, next, backportedNums)
	return report
}

// ensureLabels creates the two labels the workflow tags PRs with. Errors
// are ignored — `gh label create --force` is idempotent and benign.
func ensureLabels(ctx restate.Context, repo string) {
	_ = runV(ctx, "label:backport:"+repo, func(rctx restate.RunContext) error {
		return gh.LabelEnsure(rctx, repo, labelBackport, labelBackportColor, labelBackportDesc)
	})
	_ = runV(ctx, "label:skipNotes:"+repo, func(rctx restate.RunContext) error {
		return gh.LabelEnsure(rctx, repo, labelSkipNotes, labelSkipNotesColor, labelSkipNotesDesc)
	})
}

// listMilestonePRs returns the merged main PRs whose milestone matches
// `next`.
func listMilestonePRs(ctx restate.Context, repo, next string) ([]gh.PR, error) {
	return release.RunT(ctx, "prList:"+repo, func(rctx restate.RunContext) ([]gh.PR, error) {
		return gh.PRList(rctx, gh.PRListOpts{
			Repo:   repo,
			State:  "merged",
			Base:   baseMain,
			Head:   "",
			Search: fmt.Sprintf(`milestone:"%s"`, next),
			Limit:  prListPageMax,
		})
	})
}

// prepareClone clones the repo into a fresh temp dir and fetches the
// release branch.
func prepareClone(ctx restate.Context, repo string) (string, bool) {
	workDir, err := release.RunT(ctx, "mktemp:"+repo, func(rctx restate.RunContext) (string, error) {
		return fs.MkTempDir(rctx, "trento-patch-release-")
	})
	if err != nil {
		return "", false
	}
	if err := runV(ctx, "clone:"+repo, func(rctx restate.RunContext) error {
		return gh.RepoClone(rctx, repo, workDir)
	}); err != nil {
		return "", false
	}
	if err := runV(ctx, "fetch:"+repo, func(rctx restate.RunContext) error {
		return git.FetchOrigin(rctx, workDir, baseRelease)
	}); err != nil {
		return "", false
	}
	return workDir, true
}

// backportAllPRs cherry-picks each PR sequentially and records the
// outcomes in `report`. Returns the backport PR numbers (parsed from
// the backport PR URLs) for the version-bump PR's "Depends on" body.
func backportAllPRs(
	ctx restate.Context,
	repo, workDir, next string,
	prs []gh.PR,
	report *RepoReport,
) []int {
	var nums []int
	for _, pr := range prs {
		url, ok := backportOnePR(ctx, repo, pr, workDir, next, report)
		if !ok || url == "" {
			continue
		}
		report.Backports = append(report.Backports, url)
		if num, ok := parsePRNumberFromURL(url); ok {
			nums = append(nums, num)
		}
	}
	return nums
}

// backportOnePR cherry-picks one merged main PR onto a per-PR branch off
// the release branch, pushes it, and opens (or finds) the backport PR.
// Returns the PR URL and ok=true on success (including the "already
// backported" short-circuit). On unresolved conflict, records into
// report.Failed and returns "", false.
func backportOnePR(
	ctx restate.Context,
	repo string,
	pr gh.PR,
	workDir, next string,
	report *RepoReport,
) (string, bool) {
	branch := fmt.Sprintf("backport-pr-%d-to-release", pr.Number)
	prTag := fmt.Sprintf("%s:%d", repo, pr.Number)

	if url, ok := alreadyBackported(ctx, repo, branch, prTag); ok {
		return url, true
	}
	if err := runV(ctx, "checkoutB:"+prTag, func(rctx restate.RunContext) error {
		return git.CheckoutNewBranch(rctx, workDir, branch, originReleaseRef)
	}); err != nil {
		return "", false
	}
	if !pickAndMaybeResolve(ctx, repo, pr, workDir, branch, prTag, report) {
		return "", false
	}
	if err := runV(ctx, "push:"+prTag, func(rctx restate.RunContext) error {
		return git.PushBranch(rctx, workDir, branch, true)
	}); err != nil {
		return "", false
	}
	url := findOrCreateBackportPR(ctx, repo, pr, branch, next, prTag)
	if num, ok := parsePRNumberFromURL(url); ok {
		spawnFixPRCI(ctx, repo, num)
	}
	detach(ctx, workDir, prTag)
	return url, true
}

// spawnFixPRCIDelay is how long Restate waits before actually starting
// the fix-pr-ci workflow for a newly-opened backport PR. The delay
// gives GitHub Actions time to schedule the PR's CI runs — querying
// too eagerly would see zero runs and conclude "green" prematurely.
const spawnFixPRCIDelay = time.Minute

// spawnFixPRCI fires the trento.fix-pr-ci workflow on (repo, prNumber)
// as a detached, delay-scheduled invocation. The patch-release workflow
// does NOT wait for fix-pr-ci to complete — it may run for hours,
// fixing CI on the new backport PR independently. Each PR gets its own
// virtual-object key, so fix-pr-ci runs for different backports proceed
// in parallel.
//
// Restate's `WithDelay` queues the invocation server-side, so this
// returns immediately and patch-release continues to the next backport
// without blocking on the 1-minute warmup.
//
// Returns nothing: spawning is best-effort. The backport PR being
// opened is the primary deliverable; if Restate rejects the send
// (e.g. duplicate key already in flight), the user can re-run
// fix-pr-ci manually with the same inputs.
func spawnFixPRCI(ctx restate.Context, repo string, prNumber int) {
	key := fmt.Sprintf("%s#%d", repo, prNumber)
	_ = restate.WorkflowSend(ctx, fixprci.WorkflowName, key, "Run").
		Send(
			fixprci.Input{Repo: repo, PRNumber: prNumber},
			restate.WithDelay(spawnFixPRCIDelay),
		)
}

// alreadyBackported returns (url, true) if there is already a MERGED PR
// on the backport branch.
func alreadyBackported(ctx restate.Context, repo, branch, prTag string) (string, bool) {
	merged, err := release.RunT(ctx, "checkMerged:"+prTag, func(rctx restate.RunContext) ([]gh.PR, error) {
		return gh.PRList(rctx, gh.PRListOpts{
			Repo: repo, State: "merged", Base: baseRelease, Head: branch,
			Search: "", Limit: 1,
		})
	})
	if err == nil && len(merged) > 0 {
		return merged[0].URL, true
	}
	return "", false
}

// pickResult encodes the outcome of one cherry-pick attempt. It is
// returned as a value (not signalled via error sentinel) so the
// information survives Restate's journal round-trip — restate serializes
// errors to strings, losing sentinel identity.
type pickResult int

const (
	pickClean pickResult = iota
	pickConflict
	pickFailed
)

// pickAndMaybeResolve runs the cherry-pick and, on conflict, delegates
// to Claude. Returns true when the cherry-pick is complete (either
// cleanly or after AI resolution).
func pickAndMaybeResolve(
	ctx restate.Context,
	repo string, pr gh.PR,
	workDir, branch, prTag string,
	report *RepoReport,
) bool {
	result, pickErr := release.RunT(ctx, "cherryPick:"+prTag, func(rctx restate.RunContext) (pickResult, error) {
		cherryErr := git.CherryPick(rctx, workDir, pr.MergeCommitOID)
		if errors.Is(cherryErr, git.ErrCherryPickConflict) {
			return pickConflict, nil
		}
		if cherryErr != nil {
			return pickFailed, fmt.Errorf("git.CherryPick: %w", cherryErr)
		}
		return pickClean, nil
	})

	switch result {
	case pickClean:
		return true
	case pickConflict:
		if resolveConflictWithAI(ctx, repo, pr, workDir, branch) {
			return true
		}
		abortFailedPick(ctx, workDir, prTag)
		report.Failed = append(report.Failed,
			fmt.Sprintf("%s #%d (%s)", repo, pr.Number, branch))
		return false
	case pickFailed:
		report.Failed = append(report.Failed,
			fmt.Sprintf("%s #%d (%s) — %s", repo, pr.Number, branch, pickErr.Error()))
		return false
	}
	return false
}

// abortFailedPick rolls the worktree back to detached release after a
// cherry-pick that nobody could resolve.
func abortFailedPick(ctx restate.Context, workDir, prTag string) {
	_ = runV(ctx, "cherryPickAbort:"+prTag, func(rctx restate.RunContext) error {
		return git.CherryPickAbort(rctx, workDir)
	})
	detach(ctx, workDir, prTag)
}

// detach leaves the worktree on a detached release HEAD between PRs.
func detach(ctx restate.Context, workDir, prTag string) {
	_ = runV(ctx, "detach:"+prTag, func(rctx restate.RunContext) error {
		return git.CheckoutDetach(rctx, workDir, originReleaseRef)
	})
}

// findOrCreateBackportPR returns an open backport PR for the branch or
// opens a new one.
func findOrCreateBackportPR(
	ctx restate.Context,
	repo string, pr gh.PR, branch, next, prTag string,
) string {
	existing, _ := release.RunT(ctx, "checkOpen:"+prTag, func(rctx restate.RunContext) ([]gh.PR, error) {
		return gh.PRList(rctx, gh.PRListOpts{
			Repo: repo, State: "open", Base: baseRelease, Head: branch,
			Search: "", Limit: 1,
		})
	})
	if len(existing) > 0 {
		return existing[0].URL
	}
	url, _ := release.RunT(ctx, "create:"+prTag, func(rctx restate.RunContext) (string, error) {
		return gh.PRCreate(rctx, gh.PRCreateOpts{
			Repo:  repo,
			Title: pr.Title,
			Body:  fmt.Sprintf("Backport of #%d for the %s patch release.", pr.Number, next),
			Base:  baseRelease,
			Head:  branch,
			Label: labelBackport,
		})
	})
	return url
}

// resolveConflictWithAI dispatches the conflict to Claude with the
// resolve-conflict prompt. Returns true when the agent completed the
// cherry-pick (i.e. .git/CHERRY_PICK_HEAD no longer exists).
func resolveConflictWithAI(
	ctx restate.Context,
	repo string, pr gh.PR, workDir, branch string,
) bool {
	tag := fmt.Sprintf("%s:%d", repo, pr.Number)
	if _, err := release.RunT(ctx, "claude:resolve:"+tag, func(rctx restate.RunContext) (claude.Response, error) {
		return claude.Invoke(rctx, claude.Request{
			Prompt:       buildResolveConflictPrompt(repo, pr, branch),
			Files:        nil,
			AllowedTools: []string{"Bash", "Read", "Edit", "Write"},
			Cwd:          workDir,
		})
	}); err != nil {
		return false
	}
	inProgress, err := release.RunT(ctx, "cherryPickInProgress:"+tag, func(rctx restate.RunContext) (bool, error) {
		return git.CherryPickInProgress(rctx, workDir)
	})
	if err != nil {
		return false
	}
	return !inProgress
}

// createVersionBumpPR pushes `version-<next>` with VERSION bumped and
// opens (or updates) the "Trigger release <next>" PR.
func createVersionBumpPR(
	ctx restate.Context,
	repo, workDir, next string,
	backportNums []int,
) string {
	branch := "version-" + next
	tag := repo + ":vb"

	if !stageVersionBumpCommit(ctx, workDir, branch, next, tag) {
		return ""
	}

	body := buildVersionBumpBody(backportNums)
	if url, ok := updateExistingVersionBumpPR(ctx, repo, branch, body, tag); ok {
		return url
	}
	url, _ := release.RunT(ctx, "create:"+tag, func(rctx restate.RunContext) (string, error) {
		return gh.PRCreate(rctx, gh.PRCreateOpts{
			Repo:  repo,
			Title: "Trigger release " + next,
			Body:  body,
			Base:  baseRelease,
			Head:  branch,
			Label: labelSkipNotes,
		})
	})
	return url
}

// stageVersionBumpCommit creates the branch, writes VERSION, commits,
// and pushes. Returns false if any step fails.
func stageVersionBumpCommit(
	ctx restate.Context,
	workDir, branch, next, tag string,
) bool {
	if err := runV(ctx, "checkoutB:"+tag, func(rctx restate.RunContext) error {
		return git.CheckoutNewBranch(rctx, workDir, branch, originReleaseRef)
	}); err != nil {
		return false
	}
	if err := runV(ctx, "writeVersion:"+tag, func(rctx restate.RunContext) error {
		return fs.WriteFile(rctx, workDir+"/VERSION", next+"\n")
	}); err != nil {
		return false
	}
	if err := runV(ctx, "add:"+tag, func(rctx restate.RunContext) error {
		return git.Add(rctx, workDir, []string{"VERSION"})
	}); err != nil {
		return false
	}
	if err := runV(ctx, "commit:"+tag, func(rctx restate.RunContext) error {
		return git.Commit(rctx, workDir, "Bump version to "+next)
	}); err != nil {
		return false
	}
	if err := runV(ctx, "push:"+tag, func(rctx restate.RunContext) error {
		return git.PushBranch(rctx, workDir, branch, true)
	}); err != nil {
		return false
	}
	return true
}

// updateExistingVersionBumpPR finds an open version-bump PR and updates
// its body. Returns (url, true) when one exists.
func updateExistingVersionBumpPR(
	ctx restate.Context,
	repo, branch, body, tag string,
) (string, bool) {
	existing, _ := release.RunT(ctx, "checkOpen:"+tag, func(rctx restate.RunContext) ([]gh.PR, error) {
		return gh.PRList(rctx, gh.PRListOpts{
			Repo: repo, State: "open", Base: baseRelease, Head: branch,
			Search: "", Limit: 1,
		})
	})
	if len(existing) == 0 {
		return "", false
	}
	url := existing[0].URL
	_ = runV(ctx, "edit:"+tag, func(rctx restate.RunContext) error {
		return gh.PREdit(rctx, url, body)
	})
	return url, true
}

// --- pure helpers ---

// parsePRNumberFromURL extracts N from a GitHub PR URL like
// https://github.com/owner/repo/pull/N.
func parsePRNumberFromURL(url string) (int, bool) {
	idx := strings.LastIndex(url, "/")
	if idx < 0 || idx == len(url)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(url[idx+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

func buildResolveConflictPrompt(repo string, pr gh.PR, branch string) string {
	return fmt.Sprintf(`You are inside the git repository for %s.

A cherry-pick of commit %s (PR #%d: '%s') onto branch '%s' has produced conflicts.

Please:
1. Run 'git status' to identify the conflicted files.
2. Examine each conflict and resolve it, favouring the incoming changes unless they semantically clash with release-branch-specific code.
3. Stage the resolved files with 'git add'.
4. Complete the cherry-pick with 'git cherry-pick --continue --no-edit'.

Do NOT run 'git push'.
`, repo, pr.MergeCommitOID, pr.Number, pr.Title, branch)
}

func buildVersionBumpBody(numbers []int) string {
	var b strings.Builder
	for _, n := range numbers {
		fmt.Fprintf(&b, "Depends on #%d\n", n)
	}
	return b.String()
}
