// Package gh wraps the `gh` CLI for GitHub operations the workflows
// need (REST API reads, PR listing/creation, label management, repo
// cloning). Relies on the host's `gh` auth.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/trento-project/trento-workflows/internal/lib"
)

// PR is the flattened typed view returned by PRList. Activities hide
// the nested JSON shape `gh pr list --json` emits.
type PR struct {
	Number         int
	Title          string
	MergeCommitOID string
	URL            string
	HeadRef        string
}

// PRListOpts mirrors the relevant flags of `gh pr list`.
type PRListOpts struct {
	Repo   string // owner/name
	State  string // open | merged | closed
	Base   string // target branch
	Head   string // source branch
	Search string // GitHub search syntax
	Limit  int    // max rows; 0 means unset (caller may default)
}

// PRCreateOpts mirrors the relevant flags of `gh pr create`.
type PRCreateOpts struct {
	Repo  string
	Title string
	Body  string
	Base  string
	Head  string
	Label string
}

// APIGet runs `gh api <path>` with an optional jq filter and returns
// the trimmed result text.
func APIGet(ctx context.Context, path, jq string) (string, error) {
	args := []string{"gh", "api", path}
	if jq != "" {
		args = append(args, "--jq", jq)
	}
	out, err := lib.MustSh(ctx, "", args...)
	if err != nil {
		return "", fmt.Errorf("gh.APIGet %s: %w", path, err)
	}
	return strings.TrimSpace(out), nil
}

// rawPR is the JSON shape `gh pr list --json number,title,url,headRefName,mergeCommit` emits.
type rawPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

// PRList runs `gh pr list` with the given opts and returns the parsed
// rows.
func PRList(ctx context.Context, opts PRListOpts) ([]PR, error) {
	args := []string{
		"gh", "pr", "list",
		"--repo", opts.Repo,
		"--json", "number,title,url,headRefName,mergeCommit",
	}
	if opts.State != "" {
		args = append(args, "--state", opts.State)
	}
	if opts.Base != "" {
		args = append(args, "--base", opts.Base)
	}
	if opts.Head != "" {
		args = append(args, "--head", opts.Head)
	}
	if opts.Search != "" {
		args = append(args, "--search", opts.Search)
	}
	if opts.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(opts.Limit))
	}
	out, err := lib.MustSh(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("gh.PRList %s: %w", opts.Repo, err)
	}
	var raws []rawPR
	if err := json.Unmarshal([]byte(out), &raws); err != nil {
		return nil, fmt.Errorf("gh.PRList parse: %w", err)
	}
	prs := make([]PR, 0, len(raws))
	for _, r := range raws {
		prs = append(prs, PR{
			Number:         r.Number,
			Title:          r.Title,
			MergeCommitOID: r.MergeCommit.OID,
			URL:            r.URL,
			HeadRef:        r.HeadRefName,
		})
	}
	return prs, nil
}

// PRCreate runs `gh pr create` and returns the PR URL emitted on stdout.
func PRCreate(ctx context.Context, opts PRCreateOpts) (string, error) {
	args := []string{
		"gh", "pr", "create",
		"--repo", opts.Repo,
		"--title", opts.Title,
		"--body", opts.Body,
		"--base", opts.Base,
		"--head", opts.Head,
	}
	if opts.Label != "" {
		args = append(args, "--label", opts.Label)
	}
	out, err := lib.MustSh(ctx, "", args...)
	if err != nil {
		return "", fmt.Errorf("gh.PRCreate %s: %w", opts.Repo, err)
	}
	return strings.TrimSpace(out), nil
}

// PREdit runs `gh pr edit <url> --body <body>`.
func PREdit(ctx context.Context, url, body string) error {
	if _, err := lib.MustSh(ctx, "", "gh", "pr", "edit", url, "--body", body); err != nil {
		return fmt.Errorf("gh.PREdit %s: %w", url, err)
	}
	return nil
}

// LabelEnsure runs `gh label create <name> --color <c> --description <d>
// --force` so the label exists with the right metadata. Idempotent.
func LabelEnsure(ctx context.Context, repo, name, color, desc string) error {
	if _, err := lib.MustSh(ctx, "",
		"gh", "label", "create", name,
		"--repo", repo,
		"--color", color,
		"--description", desc,
		"--force",
	); err != nil {
		return fmt.Errorf("gh.LabelEnsure %s/%s: %w", repo, name, err)
	}
	return nil
}

// RepoClone runs `gh repo clone <repo> <dir> -- --quiet`. Uses the
// user's gh auth for private repos.
func RepoClone(ctx context.Context, repo, dir string) error {
	if _, err := lib.MustSh(ctx, "", "gh", "repo", "clone", repo, dir, "--", "--quiet"); err != nil {
		return fmt.Errorf("gh.RepoClone %s: %w", repo, err)
	}
	return nil
}

// --- CI workflow runs + jobs ---

// WorkflowRun is the flattened typed view of one Actions workflow run.
type WorkflowRun struct {
	ID         int64
	Name       string
	Status     string // queued | in_progress | completed
	Conclusion string // success | failure | cancelled | timed_out | "" until terminal
	HeadSHA    string
	URL        string
}

// JobStep is one step of a job (used to spot which step failed for the
// log analysis prompt).
type JobStep struct {
	Name       string
	Number     int
	Conclusion string // success | failure | skipped | ""
}

// WorkflowJob is one job inside a workflow run.
type WorkflowJob struct {
	ID         int64
	RunID      int64
	Name       string
	Status     string
	Conclusion string
	Steps      []JobStep
}

type rawWorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
	URL        string `json:"html_url"`
}

type rawJob struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Steps      []struct {
		Name       string `json:"name"`
		Number     int    `json:"number"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

type rawJobsEnvelope struct {
	Jobs []rawJob `json:"jobs"`
}

func parseWorkflowRunsJSON(b []byte) ([]WorkflowRun, error) {
	var raws []rawWorkflowRun
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, fmt.Errorf("parseWorkflowRunsJSON: %w", err)
	}
	out := make([]WorkflowRun, 0, len(raws))
	for _, r := range raws {
		out = append(out, WorkflowRun{
			ID: r.ID, Name: r.Name, Status: r.Status, Conclusion: r.Conclusion,
			HeadSHA: r.HeadSHA, URL: r.URL,
		})
	}
	return out, nil
}

func parseJobsForRunJSON(b []byte) ([]WorkflowJob, error) {
	var env rawJobsEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("parseJobsForRunJSON: %w", err)
	}
	out := make([]WorkflowJob, 0, len(env.Jobs))
	for _, r := range env.Jobs {
		steps := make([]JobStep, 0, len(r.Steps))
		for _, s := range r.Steps {
			steps = append(steps, JobStep{Name: s.Name, Number: s.Number, Conclusion: s.Conclusion})
		}
		out = append(out, WorkflowJob{
			ID: r.ID, RunID: r.RunID, Name: r.Name, Status: r.Status,
			Conclusion: r.Conclusion, Steps: steps,
		})
	}
	return out, nil
}

// RunListForSHA lists workflow runs whose head SHA matches.
// Wraps `gh api repos/<repo>/actions/runs?head_sha=<sha>&per_page=100`
// and parses the `workflow_runs` array. We page once (100 entries) —
// any PR with >100 workflow runs on a single SHA has bigger problems.
func RunListForSHA(ctx context.Context, repo, sha string) ([]WorkflowRun, error) {
	path := fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", repo, sha)
	out, err := APIGet(ctx, path, ".workflow_runs")
	if err != nil {
		return nil, fmt.Errorf("gh.RunListForSHA %s/%s: %w", repo, sha, err)
	}
	if strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "null" {
		return nil, nil
	}
	return parseWorkflowRunsJSON([]byte(out))
}

// JobsForRun lists the jobs inside one workflow run.
// Wraps `gh api repos/<repo>/actions/runs/<runID>/jobs?per_page=100`.
func JobsForRun(ctx context.Context, repo string, runID int64) ([]WorkflowJob, error) {
	path := fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=100", repo, runID)
	out, err := APIGet(ctx, path, "")
	if err != nil {
		return nil, fmt.Errorf("gh.JobsForRun %s/%d: %w", repo, runID, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return parseJobsForRunJSON([]byte(out))
}

// JobLogs returns the raw log text for one job. gh follows the
// presigned-URL redirect transparently.
// Wraps `gh api repos/<repo>/actions/jobs/<jobID>/logs`.
//
// No-log-available handling: GitHub returns HTTP 404 (BlobNotFound)
// for jobs whose log blob has been deleted or never uploaded (typical
// for cancelled jobs that exited before any step ran), and the
// presigned-URL redirect to productionresultssa*.blob.core.windows.net
// is occasionally flaky (connection refused, connection reset, EOF).
// Callers that work through a per-job loop already skip entries with
// an empty log via the surrounding `if err != nil { continue }` in
// fixprci, but Restate retries a failing activity 13 times before the
// workflow sees the error — each retry re-executes `gh api` against
// GitHub. Returning (empty, nil) on any of the "log not available"
// categories collapses those retries to a single no-op activity call,
// which unblocks the workflow immediately. The empty log is appended
// to the bundle as a zero-byte entry; the analyze-logs prompt ignores
// entries with empty logs.
func JobLogs(ctx context.Context, repo string, jobID int64) (string, error) {
	path := fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repo, jobID)
	stdout, stderr, code, err := lib.Sh(ctx, "", "gh", "api", path)
	if err != nil {
		return "", fmt.Errorf("gh.JobLogs %s/%d: %w", repo, jobID, err)
	}
	if code != 0 {
		// 404 = blob not found (deleted or never uploaded). Treat as
		// "no log available" and return empty + nil. Any other
		// non-zero exit is a real failure and propagates.
		combined := stdout + stderr
		if strings.Contains(combined, "HTTP 404") ||
			strings.Contains(combined, "BlobNotFound") ||
			strings.Contains(combined, "connection refused") ||
			strings.Contains(combined, "connection reset") ||
			strings.Contains(combined, "EOF") {
			return "", nil
		}
		return "", fmt.Errorf("gh.JobLogs %s/%d: gh exited %d: %s", repo, jobID, code, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// RunRerunFailed reruns only the failed jobs of one workflow run.
// Wraps `gh api -X POST repos/<repo>/actions/runs/<runID>/rerun-failed-jobs`.
func RunRerunFailed(ctx context.Context, repo string, runID int64) error {
	path := fmt.Sprintf("repos/%s/actions/runs/%d/rerun-failed-jobs", repo, runID)
	if _, err := lib.MustSh(ctx, "", "gh", "api", "-X", "POST", path); err != nil {
		return fmt.Errorf("gh.RunRerunFailed %s/%d: %w", repo, runID, err)
	}
	return nil
}

// PRHeadSHA returns the current head SHA of the PR. Refresh between
// iterations: we push new commits, which moves the SHA.
// Wraps `gh api repos/<repo>/pulls/<num>` + jq for .head.sha.
func PRHeadSHA(ctx context.Context, repo string, prNumber int) (string, error) {
	path := fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber)
	out, err := APIGet(ctx, path, ".head.sha")
	if err != nil {
		return "", fmt.Errorf("gh.PRHeadSHA %s/%d: %w", repo, prNumber, err)
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("gh.PRHeadSHA %s/%d: empty sha", repo, prNumber)
	}
	return sha, nil
}

// PRHeadRef returns the head branch name of the PR (the local ref name
// we push to). For fork PRs this is the branch name on the fork; gh pr
// checkout sets up the remote so `git push origin <ref>` works.
func PRHeadRef(ctx context.Context, repo string, prNumber int) (string, error) {
	path := fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber)
	out, err := APIGet(ctx, path, ".head.ref")
	if err != nil {
		return "", fmt.Errorf("gh.PRHeadRef %s/%d: %w", repo, prNumber, err)
	}
	ref := strings.TrimSpace(out)
	if ref == "" {
		return "", fmt.Errorf("gh.PRHeadRef %s/%d: empty ref", repo, prNumber)
	}
	return ref, nil
}

// PRCheckout checks out the PR's head into the cloned repo. Wraps
// `gh pr checkout <num>` which handles fork PRs transparently by
// creating a remote and tracking branch.
func PRCheckout(ctx context.Context, repoDir string, prNumber int) error {
	if _, err := lib.MustSh(ctx, repoDir, "gh", "pr", "checkout", strconv.Itoa(prNumber)); err != nil {
		return fmt.Errorf("gh.PRCheckout #%d in %s: %w", prNumber, repoDir, err)
	}
	return nil
}

// PRBaseRef returns the base (target) branch name of the PR.
func PRBaseRef(ctx context.Context, repo string, prNumber int) (string, error) {
	path := fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber)
	out, err := APIGet(ctx, path, ".base.ref")
	if err != nil {
		return "", fmt.Errorf("gh.PRBaseRef %s/%d: %w", repo, prNumber, err)
	}
	ref := strings.TrimSpace(out)
	if ref == "" {
		return "", fmt.Errorf("gh.PRBaseRef %s/%d: empty ref", repo, prNumber)
	}
	return ref, nil
}

// PRMergeableState returns the GitHub mergeable_state of the PR.
// Possible values per the REST API: clean | dirty | unstable | behind
// | blocked | has_hooks | unknown. Returns "" when the API field is
// null or "unknown" (still being computed) — callers should treat that
// as "no actionable signal yet" and skip mergeability checks for this
// iteration.
func PRMergeableState(ctx context.Context, repo string, prNumber int) (string, error) {
	path := fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber)
	out, err := APIGet(ctx, path, ".mergeable_state")
	if err != nil {
		return "", fmt.Errorf("gh.PRMergeableState %s/%d: %w", repo, prNumber, err)
	}
	s := strings.TrimSpace(out)
	if s == "null" || s == "unknown" {
		return "", nil
	}
	return s, nil
}

// --- PR merge ---

// PRMergeMethod is the merge-button strategy.
type PRMergeMethod string

const (
	PRMergeMethodMerge  PRMergeMethod = "merge"
	PRMergeMethodSquash PRMergeMethod = "squash"
	PRMergeMethodRebase PRMergeMethod = "rebase"
)

// PRMerge merges the given PR using the requested strategy. Wraps
// `gh pr merge <num> --repo <r> --<method> --delete-branch`.
// --delete-branch cleans up the Dependabot head after merge (matches
// what the GitHub UI does when a maintainer merges a Dependabot PR).
func PRMerge(ctx context.Context, repo string, prNumber int, method PRMergeMethod) error {
	if _, err := lib.MustSh(ctx, "",
		"gh", "pr", "merge", strconv.Itoa(prNumber),
		"--repo", repo,
		"--"+string(method),
		"--delete-branch",
	); err != nil {
		return fmt.Errorf("gh.PRMerge %s/%d: %w", repo, prNumber, err)
	}
	return nil
}

// --- PR milestone ---

// PRSetMilestone assigns a milestone to the PR. Wraps
// `gh pr edit <num> --repo <r> --milestone <title>`. The milestone
// must already exist — call MilestoneEnsure first.
func PRSetMilestone(ctx context.Context, repo string, prNumber int, milestone string) error {
	if _, err := lib.MustSh(ctx, "",
		"gh", "pr", "edit", strconv.Itoa(prNumber),
		"--repo", repo,
		"--milestone", milestone,
	); err != nil {
		return fmt.Errorf("gh.PRSetMilestone %s/%d: %w", repo, prNumber, err)
	}
	return nil
}

// Milestone is one milestone in the repo's list.
type Milestone struct {
	Number int64
	Title  string
}

type rawMilestone struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
}

func parseMilestonesJSON(b []byte) ([]Milestone, error) {
	var raws []rawMilestone
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, fmt.Errorf("parseMilestonesJSON: %w", err)
	}
	out := make([]Milestone, 0, len(raws))
	for _, r := range raws {
		out = append(out, Milestone{Number: r.Number, Title: r.Title})
	}
	return out, nil
}

// MilestoneEnsure creates the milestone if it does not exist. Wraps
// `gh api repos/<r>/milestones` for the pre-check and
// `gh api repos/<r>/milestones -X POST -f title=<t>` for the create
// path. Idempotent. Returns the milestone's numeric ID.
func MilestoneEnsure(ctx context.Context, repo, title string) (int64, error) {
	existing, err := listMilestones(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("gh.MilestoneEnsure %s/%q: list: %w", repo, title, err)
	}
	for _, m := range existing {
		if m.Title == title {
			return m.Number, nil
		}
	}
	if _, err := lib.MustSh(ctx, "",
		"gh", "api", "repos/"+repo+"/milestones",
		"-X", "POST",
		"-f", "title="+title,
	); err != nil {
		return 0, fmt.Errorf("gh.MilestoneEnsure %s/%q: create: %w", repo, title, err)
	}
	// Re-list to discover the assigned number.
	created, err := listMilestones(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("gh.MilestoneEnsure %s/%q: re-list: %w", repo, title, err)
	}
	for _, m := range created {
		if m.Title == title {
			return m.Number, nil
		}
	}
	return 0, fmt.Errorf("gh.MilestoneEnsure %s/%q: not found after create", repo, title)
}

func listMilestones(ctx context.Context, repo string) ([]Milestone, error) {
	out, err := lib.MustSh(ctx, "", "gh", "api", "repos/"+repo+"/milestones?per_page=100&state=all")
	if err != nil {
		return nil, fmt.Errorf("gh.listMilestones %s: %w", repo, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return parseMilestonesJSON([]byte(out))
}
