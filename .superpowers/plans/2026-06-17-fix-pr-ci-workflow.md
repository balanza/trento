# Fix-PR-CI workflow — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `trento.fix-pr-ci` Restate workflow that watches a PR's CI, classifies failures into flaky/bug/infra/unfixable with Claude, reruns flakies, fixes bugs autonomously, and loops until green or exhausted.

**Architecture:** New Go workflow package under `.workflows/internal/workflows/fixprci/`, plus targeted additions to existing activity packages (`gh`, `git`). Single `Run` function (no sub-services) using Restate primitives for durability. One Claude analyze-logs prompt per iteration; one Claude fix-bug prompt per detected bug.

**Tech Stack:** Go 1.25, `github.com/restatedev/sdk-go` v0.24, `github.com/go-playground/validator/v10`, `github.com/stretchr/testify` (added in Task 1 — first test consumer), the `gh` and `git` CLIs.

## Global Constraints

- **Determinism rule** (from the existing workflows): nothing in `workflow.go` may call `time.Now()`, `rand.*`, `os.Getenv`, read the filesystem outside an activity, or spawn goroutines. Non-deterministic work goes inside `restate.Run` / `restate.RunVoid` closures (the `runT` / `runV` helpers already in each workflow package).
- **Activity wrapping**: every shell-out lives in `internal/activities/<surface>/`, never inline in the workflow. The activity returns typed values; the workflow only ever sees the typed value.
- **Error wrapping**: external errors are wrapped at the activity boundary (`fmt.Errorf("ActivityName: %w", err)`). The `runT` / `runV` helpers prepend the step label so call sites stay terse.
- **No edits outside this monorepo's tooling tree.** All workflow code lives under `.workflows/`. Activities never touch `web/`, `wanda/`, `agent/`, etc. — those are submodules with their own repos. The clone-into-workDir pattern is how this constraint is enforced inside the new workflow.
- **TDD discipline for pure functions only.** Parse helpers, dispatch tables, and applyDefaults get unit tests. Shell-out wrappers and Restate-driven workflow code do not (the codebase has no shell-mock layer yet and no Restate test harness — both deliberate, both deferred). Where a step says "write test first" it means a Go unit test of pure logic.
- **Module path**: `github.com/trento-project/trento-workflows`. Import the workflow's package as `github.com/trento-project/trento-workflows/internal/workflows/fixprci`.
- **Commit cadence**: one commit per task. Use the existing `<area>: <subject>` style (look at recent `.workflows/`-related commits for examples; the project uses concise lower-case subjects).

---

## File Structure

**New files (created by this plan):**

```
.workflows/
├── internal/
│   ├── activities/
│   │   ├── gh/gh_test.go               # Task 1 — parse tests for new types
│   │   └── git/git_test.go             # Task 4 — CommitFixup wiring test
│   └── workflows/
│       └── fixprci/
│           ├── schema.go               # Task 6
│           ├── schema_test.go          # Task 6
│           ├── helpers.go              # Task 7
│           ├── helpers_test.go         # Task 7
│           ├── workflow.go             # Tasks 10, 11, 12, 13
│           └── prompts/
│               ├── analyze-logs.md     # Task 8
│               └── fix-bug.md          # Task 8
```

**Existing files modified:**

```
.workflows/
├── go.mod                              # Task 1 (+testify)
├── go.sum                              # Task 1 (auto)
├── internal/activities/gh/gh.go        # Tasks 1, 2, 3
└── internal/activities/git/git.go      # Task 4
└── cmd/handler/main.go                 # Task 13

Makefile                                # Task 13
```

**File responsibilities** (no file does two things):

- `schema.go`: Input/Output/IssueOutcome structs + applyDefaults. No I/O.
- `helpers.go`: `runV` / `runT` / `sleep` / `terminal` (restate-aware boilerplate); pure helpers (`lintFormatCmds`, `parseAnalysis`, `failedRuns`, `uniqueRunIDs`, `buildLogBundle`, `tailLines`); the adaptive `waitForRuns` poller.
- `workflow.go`: `Register`, `Run`, and the loop body broken into sub-functions (`setupCheckout`, `analyzeIteration`, `applyBugFixes`, `runLintFormat`, `commitAndPush`, `rerunFlakies`). Pure orchestration on top of helpers + activities.
- `prompts/*.md`: prompt templates, embedded via `embed.FS`.

---

## Tasks

### Task 1: Add `gh` types + listing functions (RunListForSHA, JobsForRun) with parse tests

**Files:**
- Modify: `.workflows/internal/activities/gh/gh.go` (append new types and two functions; keep existing exports untouched)
- Create: `.workflows/internal/activities/gh/gh_test.go`
- Modify: `.workflows/go.mod` (add testify)

**Interfaces:**
- Consumes: existing `lib.MustSh` and the `gh` CLI.
- Produces:
  ```go
  type WorkflowRun struct { ID int64; Name, Status, Conclusion, HeadSHA, URL string }
  type WorkflowJob struct { ID, RunID int64; Name, Status, Conclusion string; Steps []JobStep }
  type JobStep    struct { Name string; Number int; Conclusion string }
  func RunListForSHA(ctx context.Context, repo, sha string) ([]WorkflowRun, error)
  func JobsForRun(ctx context.Context, repo string, runID int64) ([]WorkflowJob, error)
  ```

- [ ] **Step 1: Add testify to go.mod**

Run: `cd .workflows && go get github.com/stretchr/testify@v1.10.0 && go mod tidy`
Expected: `go.mod` now lists `github.com/stretchr/testify v1.10.0` as a direct dep; `go.sum` updated.

- [ ] **Step 2: Write failing parse tests for RunListForSHA / JobsForRun**

Create `.workflows/internal/activities/gh/gh_test.go`:

```go
package gh

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseWorkflowRunsList(t *testing.T) {
	raw := `[
      {"id":111,"name":"CI","status":"completed","conclusion":"success","head_sha":"abc","html_url":"https://x/runs/111"},
      {"id":222,"name":"Build","status":"in_progress","conclusion":"","head_sha":"abc","html_url":"https://x/runs/222"}
    ]`
	got, err := parseWorkflowRunsJSON([]byte(raw))
	assert.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, int64(111), got[0].ID)
	assert.Equal(t, "CI", got[0].Name)
	assert.Equal(t, "completed", got[0].Status)
	assert.Equal(t, "success", got[0].Conclusion)
	assert.Equal(t, "abc", got[0].HeadSHA)
	assert.Equal(t, "https://x/runs/111", got[0].URL)
	assert.Equal(t, "", got[1].Conclusion)
}

func TestParseJobsForRun(t *testing.T) {
	raw := `{"jobs":[
      {"id":900,"run_id":111,"name":"test","status":"completed","conclusion":"failure",
       "steps":[
         {"name":"Set up","number":1,"conclusion":"success"},
         {"name":"Run tests","number":2,"conclusion":"failure"}
       ]}
    ]}`
	got, err := parseJobsForRunJSON([]byte(raw))
	assert.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, int64(900), got[0].ID)
	assert.Equal(t, int64(111), got[0].RunID)
	assert.Equal(t, "failure", got[0].Conclusion)
	assert.Len(t, got[0].Steps, 2)
	assert.Equal(t, "failure", got[0].Steps[1].Conclusion)
}
```

- [ ] **Step 3: Run tests — expect compile failure (parser fns don't exist)**

Run: `cd .workflows && go test ./internal/activities/gh/`
Expected: `undefined: parseWorkflowRunsJSON` and `undefined: parseJobsForRunJSON`.

- [ ] **Step 4: Append types and parser-plus-activity code to gh.go**

Append to `.workflows/internal/activities/gh/gh.go` (do NOT touch existing exports):

```go
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
```

- [ ] **Step 5: Run tests — expect PASS**

Run: `cd .workflows && go test ./internal/activities/gh/`
Expected: `PASS  ok  github.com/trento-project/trento-workflows/internal/activities/gh`.

- [ ] **Step 6: Verify the whole module still builds**

Run: `cd .workflows && go build ./...`
Expected: exits 0, no output.

- [ ] **Step 7: Commit**

```bash
git add .workflows/go.mod .workflows/go.sum \
        .workflows/internal/activities/gh/gh.go \
        .workflows/internal/activities/gh/gh_test.go
git commit -m "gh: add WorkflowRun/Job types and CI listing activities"
```

---

### Task 2: Add `gh.JobLogs` and `gh.RunRerunFailed`

**Files:**
- Modify: `.workflows/internal/activities/gh/gh.go`

**Interfaces:**
- Consumes: `lib.MustSh`.
- Produces:
  ```go
  func JobLogs(ctx context.Context, repo string, jobID int64) (string, error)
  func RunRerunFailed(ctx context.Context, repo string, runID int64) error
  ```

These wrap the `gh` CLI directly; no parsing, so no unit tests. The logic is small enough to review by reading.

- [ ] **Step 1: Append JobLogs and RunRerunFailed to gh.go**

Append to `.workflows/internal/activities/gh/gh.go`:

```go
// JobLogs returns the raw log text for one job. gh follows the
// presigned-URL redirect transparently.
// Wraps `gh api repos/<repo>/actions/jobs/<jobID>/logs`.
func JobLogs(ctx context.Context, repo string, jobID int64) (string, error) {
	path := fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repo, jobID)
	out, err := lib.MustSh(ctx, "", "gh", "api", path)
	if err != nil {
		return "", fmt.Errorf("gh.JobLogs %s/%d: %w", repo, jobID, err)
	}
	return out, nil
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
```

- [ ] **Step 2: Build and lint**

Run: `cd .workflows && go build ./... && go vet ./...`
Expected: exits 0.

- [ ] **Step 3: Commit**

```bash
git add .workflows/internal/activities/gh/gh.go
git commit -m "gh: add JobLogs and RunRerunFailed activities"
```

---

### Task 3: Add `gh.PRHeadSHA`, `gh.PRHeadRef`, `gh.PRCheckout`

**Files:**
- Modify: `.workflows/internal/activities/gh/gh.go`

**Interfaces:**
- Produces:
  ```go
  func PRHeadSHA(ctx context.Context, repo string, prNumber int) (string, error)
  func PRHeadRef(ctx context.Context, repo string, prNumber int) (string, error)
  func PRCheckout(ctx context.Context, repoDir string, prNumber int) error
  ```

- [ ] **Step 1: Append the three functions to gh.go**

Append to `.workflows/internal/activities/gh/gh.go`:

```go
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
```

- [ ] **Step 2: Build**

Run: `cd .workflows && go build ./...`
Expected: exits 0.

- [ ] **Step 3: Commit**

```bash
git add .workflows/internal/activities/gh/gh.go
git commit -m "gh: add PRHeadSHA, PRHeadRef, PRCheckout activities"
```

---

### Task 4: Add `git.CommitFixup`

**Files:**
- Modify: `.workflows/internal/activities/git/git.go`
- Create: `.workflows/internal/activities/git/git_test.go`

**Interfaces:**
- Produces:
  ```go
  func CommitFixup(ctx context.Context, repoPath string) (string, error)
  ```

The test exercises CommitFixup against a real temporary git repo (cheap, deterministic, no network).

- [ ] **Step 1: Write the failing test**

Create `.workflows/internal/activities/git/git_test.go`:

```go
package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommitFixup(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()

	// init repo, configure identity, make one commit, then dirty + fixup
	mustRun(t, tmp, "git", "init", "-q", "-b", "main")
	mustRun(t, tmp, "git", "config", "user.email", "t@example.com")
	mustRun(t, tmp, "git", "config", "user.name", "t")
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "a"), []byte("1\n"), 0o644))
	mustRun(t, tmp, "git", "add", "a")
	mustRun(t, tmp, "git", "commit", "-q", "-m", "first")
	headBefore := mustRunOut(t, tmp, "git", "rev-parse", "HEAD")

	require.NoError(t, os.WriteFile(filepath.Join(tmp, "a"), []byte("2\n"), 0o644))
	mustRun(t, tmp, "git", "add", "a")

	sha, err := CommitFixup(ctx, tmp)
	require.NoError(t, err)
	assert.NotEmpty(t, sha)
	headAfter := mustRunOut(t, tmp, "git", "rev-parse", "HEAD")
	assert.NotEqual(t, headBefore, headAfter)
	subject := mustRunOut(t, tmp, "git", "log", "-1", "--pretty=%s")
	assert.Contains(t, subject, "fixup!")
}

func mustRun(t *testing.T, dir string, argv ...string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "%s: %s", argv, string(out))
}

func mustRunOut(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err)
	return string(out)
}
```

- [ ] **Step 2: Run test — expect compile failure (CommitFixup undefined)**

Run: `cd .workflows && go test ./internal/activities/git/`
Expected: `undefined: CommitFixup`.

- [ ] **Step 3: Append CommitFixup to git.go**

Append to `.workflows/internal/activities/git/git.go`:

```go
// CommitFixup runs `git commit --fixup=HEAD --no-verify` so the user
// can `git rebase -i --autosquash` later. Returns the new short SHA.
// --no-verify skips local pre-commit hooks; lint+format are run
// upstream of this activity in the workflow loop, so re-running hooks
// here is redundant and would slow the loop.
func CommitFixup(ctx context.Context, repoPath string) (string, error) {
	if _, err := lib.MustSh(ctx, repoPath, "git", "commit", "--fixup=HEAD", "--no-verify", "--quiet"); err != nil {
		return "", fmt.Errorf("git.CommitFixup: %w", err)
	}
	out, err := lib.MustSh(ctx, repoPath, "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git.CommitFixup rev-parse: %w", err)
	}
	return strings.TrimSpace(out), nil
}
```

- [ ] **Step 4: Run test — expect PASS**

Run: `cd .workflows && go test ./internal/activities/git/`
Expected: `PASS`.

- [ ] **Step 5: Commit**

```bash
git add .workflows/internal/activities/git/git.go \
        .workflows/internal/activities/git/git_test.go
git commit -m "git: add CommitFixup activity"
```

---

### Task 5: Create `fixprci/schema.go` + tests

**Files:**
- Create: `.workflows/internal/workflows/fixprci/schema.go`
- Create: `.workflows/internal/workflows/fixprci/schema_test.go`

**Interfaces:**
- Produces:
  ```go
  type Input struct { Repo string; PRNumber, MaxAttempts, MaxIterations int; CleanupOnExit bool }
  type Output struct { Repo string; PRNumber int; FinalStatus FinalStatus; Iterations int; Commits []string; Issues []IssueOutcome }
  type IssueOutcome struct { IssueKey, Category, Summary string; Attempts int; FinalState string; JobRefs []string }
  type FinalStatus string  // FinalStatusGreen | FinalStatusExhausted | FinalStatusAborted
  func applyDefaults(in Input) Input
  ```

- [ ] **Step 1: Write the failing test**

Create `.workflows/internal/workflows/fixprci/schema_test.go`:

```go
package fixprci

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyDefaults(t *testing.T) {
	out := applyDefaults(Input{Repo: "x/y", PRNumber: 1})
	assert.Equal(t, 5, out.MaxAttempts)
	assert.Equal(t, 15, out.MaxIterations)
	assert.False(t, out.CleanupOnExit)
}

func TestApplyDefaultsPreservesCallerValues(t *testing.T) {
	out := applyDefaults(Input{
		Repo: "x/y", PRNumber: 1,
		MaxAttempts: 2, MaxIterations: 3, CleanupOnExit: true,
	})
	assert.Equal(t, 2, out.MaxAttempts)
	assert.Equal(t, 3, out.MaxIterations)
	assert.True(t, out.CleanupOnExit)
}
```

- [ ] **Step 2: Run test — expect compile failure (Input, applyDefaults undefined)**

Run: `cd .workflows && go test ./internal/workflows/fixprci/`
Expected: `undefined: Input`, `undefined: applyDefaults`.

- [ ] **Step 3: Create schema.go**

Create `.workflows/internal/workflows/fixprci/schema.go`:

```go
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
	// (state kept for inspection).
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
	FinalState string   `json:"finalState"` // fixed|exhausted|unfixable|still-flaky|rerun-only
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
```

- [ ] **Step 4: Run test — expect PASS**

Run: `cd .workflows && go test ./internal/workflows/fixprci/`
Expected: `PASS`.

- [ ] **Step 5: Commit**

```bash
git add .workflows/internal/workflows/fixprci/schema.go \
        .workflows/internal/workflows/fixprci/schema_test.go
git commit -m "fixprci: add schema + applyDefaults"
```

---

### Task 6: Create `fixprci/helpers.go` (boilerplate + pure helpers + tests)

**Files:**
- Create: `.workflows/internal/workflows/fixprci/helpers.go`
- Modify: `.workflows/internal/workflows/fixprci/schema_test.go` → rename to `helpers_test.go` is no-op; just add tests there.
- Create: `.workflows/internal/workflows/fixprci/helpers_test.go`

**Interfaces:**
- Consumes: `gh.WorkflowRun`, `IssueOutcome` from earlier tasks.
- Produces:
  ```go
  // restate boilerplate
  func runV(ctx restate.Context, label string, fn func(restate.RunContext) error) error
  func runT[T any](ctx restate.Context, label string, fn func(restate.RunContext) (T, error)) (T, error)
  func sleep(ctx restate.Context, d time.Duration) error
  func terminal(err error) error
  // pure helpers
  func lintFormatCmds(repo string) [][]string
  func failedRuns(runs []gh.WorkflowRun) []gh.WorkflowRun
  func allRunsTerminal(runs []gh.WorkflowRun) bool
  func tailLines(s string, n int) string
  func parseAnalysis(s string) ([]issue, bool)
  type issue struct { IssueKey, Category, Summary, Hint string; JobRefs []string }
  func uniqueRunIDsFromIssues(issues []issue) []int64  // parses run/<id>/job/<id> jobRefs
  func buildLogBundle(entries []logEntry) string
  type logEntry struct { RunID, JobID int64; Name, Conclusion, Log string }
  func splitJobRef(s string) (runID, jobID int64, ok bool)  // "run/<n>/job/<m>"
  ```

- [ ] **Step 1: Write the failing helper tests**

Create `.workflows/internal/workflows/fixprci/helpers_test.go`:

```go
package fixprci

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/trento-project/trento-workflows/internal/activities/gh"
)

func TestLintFormatCmds(t *testing.T) {
	tests := []struct {
		repo string
		want int // number of commands
	}{
		{"trento-project/web", 3},
		{"trento-project/wanda", 1},
		{"trento-project/agent", 2},
		{"trento-project/mcp-server", 2},
		{"trento-project/checks", 0},
		{"trento-project/contracts", 0},
		{"unknown/repo", 0},
	}
	for _, tc := range tests {
		got := lintFormatCmds(tc.repo)
		assert.Lenf(t, got, tc.want, "repo=%s", tc.repo)
	}
	assert.Equal(t, [][]string{{"mix", "format"}}, lintFormatCmds("trento-project/wanda"))
}

func TestFailedRuns(t *testing.T) {
	in := []gh.WorkflowRun{
		{ID: 1, Status: "completed", Conclusion: "success"},
		{ID: 2, Status: "completed", Conclusion: "failure"},
		{ID: 3, Status: "completed", Conclusion: "cancelled"},
		{ID: 4, Status: "completed", Conclusion: "timed_out"},
		{ID: 5, Status: "in_progress", Conclusion: ""},
	}
	got := failedRuns(in)
	ids := []int64{}
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []int64{2, 3, 4}, ids)
}

func TestAllRunsTerminal(t *testing.T) {
	assert.True(t, allRunsTerminal([]gh.WorkflowRun{
		{Status: "completed"}, {Status: "completed"},
	}))
	assert.False(t, allRunsTerminal([]gh.WorkflowRun{
		{Status: "completed"}, {Status: "in_progress"},
	}))
	assert.False(t, allRunsTerminal(nil)) // no runs = not terminal (caller decides what to do)
}

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne"
	assert.Equal(t, "d\ne", tailLines(in, 2))
	assert.Equal(t, in, tailLines(in, 10))
	assert.Equal(t, "", tailLines("", 5))
}

func TestParseAnalysisHappy(t *testing.T) {
	raw := "blah blah <analysis>[\n" +
		`  {"issueKey":"bug:web:x","category":"bug","summary":"y","hint":"do z","jobRefs":["run/1/job/2"]},` + "\n" +
		`  {"issueKey":"flaky:web:t","category":"flaky","summary":"f","hint":"","jobRefs":["run/1/job/3"]}` + "\n" +
		"]</analysis> trailing"
	got, ok := parseAnalysis(raw)
	assert.True(t, ok)
	assert.Len(t, got, 2)
	assert.Equal(t, "bug", got[0].Category)
	assert.Equal(t, []string{"run/1/job/3"}, got[1].JobRefs)
}

func TestParseAnalysisRejectsMalformed(t *testing.T) {
	_, ok := parseAnalysis("no tags here, just text")
	assert.False(t, ok)
	_, ok = parseAnalysis("<analysis>not json</analysis>")
	assert.False(t, ok)
	_, ok = parseAnalysis("<analysis>{}</analysis>") // object, not array
	assert.False(t, ok)
}

func TestSplitJobRef(t *testing.T) {
	runID, jobID, ok := splitJobRef("run/12/job/34")
	assert.True(t, ok)
	assert.Equal(t, int64(12), runID)
	assert.Equal(t, int64(34), jobID)
	_, _, ok = splitJobRef("garbage")
	assert.False(t, ok)
	_, _, ok = splitJobRef("run/12/job/")
	assert.False(t, ok)
}

func TestUniqueRunIDsFromIssues(t *testing.T) {
	in := []issue{
		{JobRefs: []string{"run/1/job/10", "run/1/job/11"}},
		{JobRefs: []string{"run/2/job/20"}},
		{JobRefs: []string{"run/1/job/12"}},
		{JobRefs: []string{"garbage"}},
	}
	got := uniqueRunIDsFromIssues(in)
	assert.ElementsMatch(t, []int64{1, 2}, got)
}

func TestBuildLogBundle(t *testing.T) {
	in := []logEntry{
		{RunID: 1, JobID: 2, Name: "test", Conclusion: "failure", Log: "line\n"},
		{RunID: 1, JobID: 3, Name: "build", Conclusion: "failure", Log: "boom\n"},
	}
	got := buildLogBundle(in)
	assert.Contains(t, got, "=== run/1 job/2 name=test conclusion=failure ===")
	assert.Contains(t, got, "line\n")
	assert.Contains(t, got, "=== run/1 job/3 name=build conclusion=failure ===")
}
```

- [ ] **Step 2: Run tests — expect compile failure**

Run: `cd .workflows && go test ./internal/workflows/fixprci/`
Expected: undefined errors for `lintFormatCmds`, `failedRuns`, etc.

- [ ] **Step 3: Create helpers.go with restate boilerplate + pure helpers**

Create `.workflows/internal/workflows/fixprci/helpers.go`:

```go
package fixprci

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/gh"
)

// --- restate boilerplate (mirrors patchrelease/testonobs helpers.go) ---

func runV(ctx restate.Context, label string, fn func(restate.RunContext) error) error {
	if err := restate.RunVoid(ctx, fn, restate.WithName(label)); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

func runT[T any](
	ctx restate.Context,
	label string,
	fn func(restate.RunContext) (T, error),
) (T, error) {
	v, err := restate.Run(ctx, fn, restate.WithName(label))
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%s: %w", label, err)
	}
	return v, nil
}

func sleep(ctx restate.Context, d time.Duration) error {
	if err := restate.Sleep(ctx, d); err != nil {
		return fmt.Errorf("sleep: %w", err)
	}
	return nil
}

func terminal(err error) error {
	return fmt.Errorf("terminal: %w", restate.TerminalError(err))
}

// --- pure helpers ---

// lintFormatCmds returns the format-then-lint commands to run in
// workDir before a commit. Auto-fix flavors first, lint check last. A
// non-zero exit from any command aborts the commit for this iteration
// (no-progress signal).
//
// Returns nil for repos with no configured toolchain — the commit step
// then proceeds directly.
func lintFormatCmds(repo string) [][]string {
	switch repo {
	case "trento-project/web":
		return [][]string{
			{"mix", "format"},
			{"npm", "--prefix", "assets", "run", "format"},
			{"npm", "--prefix", "assets", "run", "lint:fix"},
		}
	case "trento-project/wanda":
		return [][]string{
			{"mix", "format"},
		}
	case "trento-project/agent", "trento-project/mcp-server":
		return [][]string{
			{"make", "fmt"},
			{"make", "lint"},
		}
	case "trento-project/checks", "trento-project/contracts":
		return nil
	default:
		return nil
	}
}

// failedRuns returns the subset of runs that ended in a non-success
// terminal state. Cancelled and timed_out are treated as failures: from
// the workflow's perspective, the run did not pass and we want Claude to
// see whatever logs do exist.
func failedRuns(runs []gh.WorkflowRun) []gh.WorkflowRun {
	out := make([]gh.WorkflowRun, 0, len(runs))
	for _, r := range runs {
		if r.Status != "completed" {
			continue
		}
		switch r.Conclusion {
		case "failure", "cancelled", "timed_out":
			out = append(out, r)
		}
	}
	return out
}

// allRunsTerminal reports whether every run is in the "completed"
// status. Returns false for an empty slice — the caller decides what
// "no runs" means (in waitForRuns it means keep polling; in the loop's
// no-CI-configured early-exit it means green).
func allRunsTerminal(runs []gh.WorkflowRun) bool {
	if len(runs) == 0 {
		return false
	}
	for _, r := range runs {
		if r.Status != "completed" {
			return false
		}
	}
	return true
}

// tailLines returns the last n lines of s, or all of s if it has fewer.
func tailLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// issue is the parsed shape of one item from the analyze-logs prompt's
// JSON output. issueKey, category, summary, hint, jobRefs match the
// prompt's contract exactly.
type issue struct {
	IssueKey string   `json:"issueKey"`
	Category string   `json:"category"`
	Summary  string   `json:"summary"`
	Hint     string   `json:"hint"`
	JobRefs  []string `json:"jobRefs"`
}

// parseAnalysis extracts the JSON array between <analysis>...</analysis>
// tags from Claude's response and unmarshals it. Returns ok=false on
// any failure — workflow treats that as no-progress.
func parseAnalysis(s string) ([]issue, bool) {
	const open, close = "<analysis>", "</analysis>"
	i := strings.Index(s, open)
	j := strings.LastIndex(s, close)
	if i < 0 || j < 0 || j <= i+len(open) {
		return nil, false
	}
	body := strings.TrimSpace(s[i+len(open) : j])
	if !strings.HasPrefix(body, "[") || !strings.HasSuffix(body, "]") {
		return nil, false
	}
	var out []issue
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, false
	}
	return out, true
}

// splitJobRef parses "run/<runID>/job/<jobID>" into the two ints.
func splitJobRef(s string) (int64, int64, bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 4 || parts[0] != "run" || parts[2] != "job" {
		return 0, 0, false
	}
	runID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	jobID, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return runID, jobID, true
}

// uniqueRunIDsFromIssues collects the distinct run IDs across every
// issue's jobRefs. Malformed refs are silently skipped.
func uniqueRunIDsFromIssues(issues []issue) []int64 {
	seen := map[int64]struct{}{}
	out := []int64{}
	for _, iss := range issues {
		for _, ref := range iss.JobRefs {
			runID, _, ok := splitJobRef(ref)
			if !ok {
				continue
			}
			if _, dup := seen[runID]; dup {
				continue
			}
			seen[runID] = struct{}{}
			out = append(out, runID)
		}
	}
	return out
}

// logEntry is one job's log, formatted into the bundle the analyze-logs
// prompt receives.
type logEntry struct {
	RunID, JobID int64
	Name         string
	Conclusion   string
	Log          string
}

// buildLogBundle concatenates the entries, each preceded by a
// machine-readable header line for Claude to use as the jobRef.
func buildLogBundle(entries []logEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "=== run/%d job/%d name=%s conclusion=%s ===\n", e.RunID, e.JobID, e.Name, e.Conclusion)
		b.WriteString(e.Log)
		if !strings.HasSuffix(e.Log, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
```

- [ ] **Step 4: Run tests — expect PASS**

Run: `cd .workflows && go test ./internal/workflows/fixprci/`
Expected: `PASS  ok  github.com/trento-project/trento-workflows/internal/workflows/fixprci`.

- [ ] **Step 5: Build module**

Run: `cd .workflows && go build ./...`
Expected: exits 0.

- [ ] **Step 6: Commit**

```bash
git add .workflows/internal/workflows/fixprci/helpers.go \
        .workflows/internal/workflows/fixprci/helpers_test.go
git commit -m "fixprci: add helpers (boilerplate + pure functions)"
```

---

### Task 7: Add `fixprci/prompts/` (both prompt templates)

**Files:**
- Create: `.workflows/internal/workflows/fixprci/prompts/analyze-logs.md`
- Create: `.workflows/internal/workflows/fixprci/prompts/fix-bug.md`

These are templates with `{{placeholder}}` markers; rendering uses plain `strings.ReplaceAll` (no `text/template` to keep things readable for review).

- [ ] **Step 1: Create `prompts/analyze-logs.md`**

Create `.workflows/internal/workflows/fixprci/prompts/analyze-logs.md`:

```markdown
You are diagnosing CI failures on pull request #{{prNumber}} of {{repo}} at head SHA {{headSHA}}.

The block below contains the full logs of every failed job, separated by header lines:

```
=== run/<runID> job/<jobID> name=<jobName> conclusion=<conclusion> ===
<log text>
```

{{logBundle}}

# Task

Read all the logs together. Look for cross-job correlations — two jobs failing for the same underlying cause are ONE issue, not two. Group failures into distinct issues. Be conservative: when in doubt about whether two failures share a root cause, treat them as one issue and let the workflow's per-issue attempt counter expand them later if you were wrong.

For each issue, output an object with these fields:

- `issueKey` — STABLE identifier reused across iterations. The workflow uses it to count attempts.
  - Format: `<category>:<short-stable-discriminator>`.
  - Examples: `bug:web:undefined-var:foo.ex:42`, `flaky:agent:test_TestRegisterHost`, `infra:runner-disconnect`, `unfixable:upstream:hex-package-yanked`.
  - Rules: lowercase; no timestamps, no run/job IDs, no SHAs, no PIDs, no temp paths. The key MUST be byte-identical across iterations for the same root cause.
- `category` — one of `flaky`, `bug`, `infra`, `unfixable`.
  - `flaky`: test pass/fail is non-deterministic; a rerun should clear it.
  - `bug`: a real code defect in this repo, fixable by editing source files in the checkout.
  - `infra`: CI infrastructure failure (runner died, network blip, image pull failure, dependency-registry 5xx). A rerun should clear it.
  - `unfixable`: upstream dependency yanked, breaking change in a third-party API, requires a product decision, etc. Not actionable from inside this repo.
- `summary` — ≤200 chars, plain-language description of the failure.
- `jobRefs` — array of `run/<runID>/job/<jobID>` strings for every job exhibiting this issue. Copy these from the header lines verbatim.
- `hint` — ≤500 chars. For `bug` issues: actionable hint for the fix — file path/line if visible, suspected root cause, suggested edit. For `flaky` / `infra` / `unfixable`: can be empty string.

# Output rules

Output the JSON array wrapped in `<analysis>...</analysis>` tags. No prose outside the tags.

Example:

<analysis>[
  {"issueKey":"bug:web:undefined-var:user_controller.ex:34","category":"bug","summary":"UserController references undefined `current_user_id`","jobRefs":["run/8123/job/45100"],"hint":"In lib/web/controllers/user_controller.ex:34, the line uses `current_user_id` but the assigns key is `user_id`. Rename to `user_id`."},
  {"issueKey":"flaky:agent:test_TestRegisterHostRetries","category":"flaky","summary":"TestRegisterHostRetries fails intermittently on timeout","jobRefs":["run/8123/job/45101"],"hint":""}
]</analysis>

If no failures can be diagnosed from the logs (empty/garbled), output:

<analysis>[]</analysis>
```

- [ ] **Step 2: Create `prompts/fix-bug.md`**

Create `.workflows/internal/workflows/fixprci/prompts/fix-bug.md`:

```markdown
You are inside a fresh checkout of `{{repo}}` at the PR's head branch. The working tree is clean.

# Issue to fix

- **issueKey**: {{issueKey}}
- **summary**: {{summary}}
- **hint**: {{hint}}

# Relevant log excerpt

The failing jobs produced these logs (each truncated to the last 200 lines):

{{relevantLogExcerpt}}

# Goal

Make the **minimum** edit needed to fix the described issue.

# Rules

1. Use `Bash`, `Read`, `Edit`, `Write` freely to investigate and edit source files.
2. Do NOT run `git commit` or `git push`. The workflow handles staging, lint+format, committing as a `fixup!` commit, and pushing — your job is only to edit source files.
3. Do NOT run the test suite. Running tests locally is too slow; CI is the verification loop. The workflow re-triggers CI after you finish.
4. Keep changes minimal — one bug, one fix. Don't refactor surrounding code, fix unrelated lint, or expand scope. The PR author will review your edits.
5. If after investigation the issue is NOT addressable from inside this repo (it's in a dependency, it's CI infrastructure, it needs a product decision, or the hint is wrong and you can't find the real cause), output exactly:

   NOT_A_CODE_FIX

   on its own line, with no other text. Exit without editing. The workflow will reclassify this issue to `unfixable` and stop retrying it.

# Output

When done editing, exit. The workflow does not parse your stdout (other than the `NOT_A_CODE_FIX` sentinel). It diffs the working tree to see what you changed.
```

- [ ] **Step 3: Verify both files are valid markdown (just a sanity read)**

Run: `wc -l .workflows/internal/workflows/fixprci/prompts/*.md`
Expected: two files, line counts >10 each.

- [ ] **Step 4: Commit**

```bash
git add .workflows/internal/workflows/fixprci/prompts/
git commit -m "fixprci: add analyze-logs and fix-bug prompt templates"
```

---

### Task 8: Implement `waitForRuns` adaptive poller

**Files:**
- Modify: `.workflows/internal/workflows/fixprci/helpers.go`

**Interfaces:**
- Consumes: `gh.RunListForSHA`, `runT`, `sleep`, `allRunsTerminal`.
- Produces:
  ```go
  func waitForRuns(ctx restate.Context, repo, sha string) ([]gh.WorkflowRun, error)
  ```

Cadence (matches spec): 10s × 12 attempts (2 min), then 60s × 58 attempts (~58 min), 60 min total cap. Returns `nil, error` on hard timeout (caller decides what to do — for this workflow, `FinalStatus=aborted`).

This function calls `sleep` and `runT`, both of which only work inside a workflow's restate context. It is not unit-tested; correctness comes from the small, clearly-correct loop body.

- [ ] **Step 1: Append `waitForRuns` to helpers.go**

Append to `.workflows/internal/workflows/fixprci/helpers.go`:

```go
// waitForRuns polls gh.RunListForSHA until every run for `sha` is in a
// terminal status, or the hard cap is hit. Adaptive cadence: 10s for
// the first ~2 minutes, 60s after that. Hard cap ~60 minutes.
//
// Returns the final runs list on success. Returns a non-nil error when
// the hard cap is hit before convergence; the caller maps this to
// FinalStatus=aborted.
//
// An empty result on the very first poll (no runs ever reported for
// this SHA) is returned to the caller — the workflow treats that as
// "no CI configured for this repo" and exits green.
func waitForRuns(ctx restate.Context, repo, sha string) ([]gh.WorkflowRun, error) {
	const (
		fastInterval = 10 * time.Second
		fastAttempts = 12 // 2 minutes
		slowInterval = 60 * time.Second
		slowAttempts = 58 // ~58 minutes; total cap ~60 min
	)

	// First poll, no sleep — answer instantly when CI already finished
	// before the workflow started.
	first, err := pollRuns(ctx, repo, sha, 0)
	if err != nil {
		return nil, err
	}
	if len(first) == 0 {
		return nil, nil
	}
	if allRunsTerminal(first) {
		return first, nil
	}

	for i := 0; i < fastAttempts; i++ {
		runs, err := pollRuns(ctx, repo, sha, i+1)
		if err != nil {
			return nil, err
		}
		if allRunsTerminal(runs) {
			return runs, nil
		}
		if err := sleep(ctx, fastInterval); err != nil {
			return nil, err
		}
	}
	for i := 0; i < slowAttempts; i++ {
		runs, err := pollRuns(ctx, repo, sha, fastAttempts+i+1)
		if err != nil {
			return nil, err
		}
		if allRunsTerminal(runs) {
			return runs, nil
		}
		if err := sleep(ctx, slowInterval); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("waitForRuns %s/%s: hard cap exceeded", repo, sha)
}

// pollRuns is one labeled gh.RunListForSHA call. The label includes the
// attempt index so Restate journal entries are distinguishable.
func pollRuns(ctx restate.Context, repo, sha string, attempt int) ([]gh.WorkflowRun, error) {
	label := fmt.Sprintf("gh.RunListForSHA:%s:%d", sha, attempt)
	return runT(ctx, label, func(rctx restate.RunContext) ([]gh.WorkflowRun, error) {
		return gh.RunListForSHA(rctx, repo, sha)
	})
}
```

- [ ] **Step 2: Build**

Run: `cd .workflows && go build ./...`
Expected: exits 0.

- [ ] **Step 3: Re-run existing tests (no regressions)**

Run: `cd .workflows && go test ./...`
Expected: `PASS` for all packages with tests; `[no test files]` for the rest.

- [ ] **Step 4: Commit**

```bash
git add .workflows/internal/workflows/fixprci/helpers.go
git commit -m "fixprci: add waitForRuns adaptive poller"
```

---

### Task 9: Workflow scaffolding — `Register`, `Run` setup, early-return when no failures

**Files:**
- Create: `.workflows/internal/workflows/fixprci/workflow.go`

This task lands the skeleton so the workflow registers and runs end-to-end for the happy-path "no CI failures" case. Tasks 10-12 add the loop body.

**Interfaces:**
- Consumes: schema types, helpers, `gh.*`, `fs.MkTempDir`.
- Produces:
  ```go
  const WorkflowName = "trento.fix-pr-ci"
  func Register() restate.ServiceDefinition
  func Run(ctx restate.WorkflowContext, in Input) (Output, error)
  ```

- [ ] **Step 1: Create `workflow.go` with Register + Run skeleton**

Create `.workflows/internal/workflows/fixprci/workflow.go`:

```go
// Package fixprci implements the "fix-pr-ci" workflow: watch a PR's
// GitHub Actions CI, classify failures with Claude into flaky/bug/
// infra/unfixable categories, rerun flakies, fix bugs autonomously
// (commit + push as fixup), loop until green or exhausted.
//
// Design: docs/superpowers/specs/2026-06-17-fix-pr-ci-workflow-design.md
package fixprci

import (
	"fmt"
	"os"

	"github.com/go-playground/validator/v10"
	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/fs"
	"github.com/trento-project/trento-workflows/internal/activities/gh"
)

// WorkflowName is the Restate service identifier.
const WorkflowName = "trento.fix-pr-ci"

// Register returns the Restate workflow definition. The handler binary
// binds this into its endpoint.
func Register() restate.ServiceDefinition {
	return restate.NewWorkflow(WorkflowName).
		Handler("Run", restate.NewWorkflowHandler(Run))
}

// Run is the workflow entrypoint. Virtual-object key:
// fmt.Sprintf("%s#%d", in.Repo, in.PRNumber).
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

	// Task 10+ replaces this stub with the real loop. For now, just
	// poll once: if no failures, return green; otherwise, return
	// exhausted with no iterations attempted (the loop body lands next).
	sha, err := runT(ctx, "gh.PRHeadSHA", func(rctx restate.RunContext) (string, error) {
		return gh.PRHeadSHA(rctx, in.Repo, in.PRNumber)
	})
	if err != nil {
		out.FinalStatus = FinalStatusAborted
		return out, nil
	}
	runs, err := waitForRuns(ctx, in.Repo, sha)
	if err != nil {
		out.FinalStatus = FinalStatusAborted
		return out, nil
	}
	out.Iterations = 1
	if len(failedRuns(runs)) == 0 {
		out.FinalStatus = FinalStatusGreen
		return out, nil
	}
	out.FinalStatus = FinalStatusExhausted // placeholder until Task 10
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

// maybeCleanup removes workDir when in.CleanupOnExit is true. Best-effort —
// failures to remove the tree are logged via the journal but not surfaced
// as errors (the workflow has already returned its result by this point).
func maybeCleanup(ctx restate.Context, workDir string, do bool) {
	if !do || workDir == "" {
		return
	}
	_ = runV(ctx, "cleanup:"+workDir, func(_ restate.RunContext) error {
		return os.RemoveAll(workDir)
	})
}
```

- [ ] **Step 2: Build**

Run: `cd .workflows && go build ./...`
Expected: exits 0.

- [ ] **Step 3: Vet**

Run: `cd .workflows && go vet ./...`
Expected: exits 0.

- [ ] **Step 4: Commit**

```bash
git add .workflows/internal/workflows/fixprci/workflow.go
git commit -m "fixprci: workflow scaffold (Register, Run setup, early-return)"
```

---

### Task 10: Loop body — log collection, Claude analysis, attempt tracking

**Files:**
- Modify: `.workflows/internal/workflows/fixprci/workflow.go`

**Interfaces:**
- Consumes: `gh.JobsForRun`, `gh.JobLogs`, `claude.Invoke`, helpers from Task 6.
- Produces:
  ```go
  func collectFailedJobLogs(ctx restate.Context, repo string, runs []gh.WorkflowRun) ([]logEntry, error)
  func analyzeLogs(ctx restate.Context, in Input, sha string, bundle string) ([]issue, bool)
  ```

The loop is structured top-down: this task replaces the "placeholder" exit with the real iteration loop, but leaves the bug-fix / lint / commit / push body as a function the next task will fill in. By the end of Task 10 the workflow can already run iterations and decide on flakies-only reruns.

- [ ] **Step 1: Read the design's loop pseudo-code** (background — no code change)

Re-read `docs/superpowers/specs/2026-06-17-fix-pr-ci-workflow-design.md` § "Workflow shape" to keep the step numbering aligned with the spec.

- [ ] **Step 2: Embed the analyze-logs prompt template**

Replace the `import` block at the top of `.workflows/internal/workflows/fixprci/workflow.go` with:

```go
import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/go-playground/validator/v10"
	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/claude"
	"github.com/trento-project/trento-workflows/internal/activities/fs"
	"github.com/trento-project/trento-workflows/internal/activities/gh"
)

//go:embed prompts/analyze-logs.md
var analyzeLogsTmpl string

//go:embed prompts/fix-bug.md
var fixBugTmpl string
```

- [ ] **Step 3: Replace the placeholder loop with the real iteration loop**

Replace the entire body of the `Run` function with this new version:

```go
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

		issues, ok := analyzeLogs(ctx, in, sha, bundle)
		commitMade := false
		reranSomething := false

		if !ok {
			// unparseable analysis → no-progress iteration
		} else {
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

			// Bug fixes + lint/format + commit + push lands in Task 11.
			// For now this branch is a stub; until then the workflow
			// behaves like rerun-only.
			_ = bugs
			_ = workDir
			_ = headRef

			if !commitMade && len(rerunable) > 0 {
				runIDs := uniqueRunIDsFromIssues(rerunable)
				for _, runID := range runIDs {
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
```

- [ ] **Step 4: Add `collectFailedJobLogs`, `analyzeLogs`, `recordIssue`, `splitIssuesByAction`, `finalizeIssueStates`**

Append to `.workflows/internal/workflows/fixprci/workflow.go`:

```go
// collectFailedJobLogs walks each failed run, fetches its jobs, filters
// the ones that themselves ended in failure/cancelled/timed_out, and
// pulls each job's log. Returns the bundle entries in the order
// (run, then job) for determinism.
//
// Job-log fetches are NOT parallelized — Restate's workflow loop is
// single-coroutine; the parallel construct would require RunAsync +
// WaitFirst which complicates the journal for marginal latency wins on
// a typical "≤ a few dozen failed jobs" workload.
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
				// fall through
			default:
				continue
			}
			log, err := runT(ctx, fmt.Sprintf("gh.JobLogs:%d", j.ID), func(rctx restate.RunContext) (string, error) {
				return gh.JobLogs(rctx, repo, j.ID)
			})
			if err != nil {
				continue // best-effort: one missing log shouldn't abort the iteration
			}
			out = append(out, logEntry{
				RunID: r.ID, JobID: j.ID,
				Name: j.Name, Conclusion: j.Conclusion, Log: log,
			})
		}
	}
	return out, nil
}

// analyzeLogs renders the analyze-logs prompt and asks Claude to
// classify the failures.
func analyzeLogs(ctx restate.Context, in Input, sha, bundle string) ([]issue, bool) {
	prompt := strings.NewReplacer(
		"{{repo}}", in.Repo,
		"{{prNumber}}", fmt.Sprintf("%d", in.PRNumber),
		"{{headSHA}}", sha,
		"{{logBundle}}", bundle,
	).Replace(analyzeLogsTmpl)

	label := fmt.Sprintf("claude.analyzeLogs:%d", in.Iterations()) // see helper below
	_ = label
	resp, err := runT(ctx, "claude.analyzeLogs", func(rctx restate.RunContext) (claude.Response, error) {
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

// recordIssue creates or updates the per-key issue record. The Attempts
// field reflects the latest count; FinalState is filled in by the
// callers (loop body sets exhausted/unfixable; Task 11 sets fixed;
// finalizeIssueStates fills in defaults at the end).
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
// Bugs go through the fix-bug prompt path; rerunable (flaky + infra)
// trigger gh.RunRerunFailed when no bug fix was committed this round.
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
// were seen but never moved to a terminal state by the loop body, then
// copies the map's values into out.Issues in a stable order (sorted by
// IssueKey) so the Output is deterministic.
func finalizeIssueStates(out *Output, byKey map[string]*IssueOutcome) {
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	// stable sort without importing sort/slices into this file's surface:
	// inline insertion sort, fine for tiny N.
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
```

- [ ] **Step 5: Remove the broken `in.Iterations()` reference**

The previous step left a stub line `label := fmt.Sprintf(...in.Iterations())` and `_ = label` — that was a placeholder for a per-iteration label. Replace those two lines in `analyzeLogs` with a single-line static label (the journal will deduplicate-by-key correctly because Restate scopes labels per workflow invocation; iteration index isn't needed for replay safety, only for log readability — and the spec doesn't require it).

In `analyzeLogs`, replace:

```go
	label := fmt.Sprintf("claude.analyzeLogs:%d", in.Iterations()) // see helper below
	_ = label
	resp, err := runT(ctx, "claude.analyzeLogs", func(rctx restate.RunContext) (claude.Response, error) {
```

with:

```go
	resp, err := runT(ctx, "claude.analyzeLogs", func(rctx restate.RunContext) (claude.Response, error) {
```

- [ ] **Step 6: Build + vet + test**

Run: `cd .workflows && go build ./... && go vet ./... && go test ./...`
Expected: exits 0; tests still PASS.

- [ ] **Step 7: Commit**

```bash
git add .workflows/internal/workflows/fixprci/workflow.go
git commit -m "fixprci: implement iteration loop body (analysis + rerun)"
```

---

### Task 11: Bug fix dispatch + lint/format + commit + push (with one-shot rebase retry)

**Files:**
- Modify: `.workflows/internal/workflows/fixprci/workflow.go`

**Interfaces:**
- Consumes: `claude.Invoke`, `git.Dirty`, `git.Add`, `git.CommitFixup`, `git.PushBranch`, `git.FetchOrigin`, `lib.Sh`, `lintFormatCmds`, `fixBugTmpl`.
- Produces:
  ```go
  func applyBugFixes(ctx restate.Context, workDir string, bugs []issue, byKey map[string]*IssueOutcome) bool  // returns "any agent actually edited"
  func runLintFormat(ctx restate.Context, repo, workDir string) bool                                          // returns "all commands exit 0"
  func commitAndPush(ctx restate.Context, repo, workDir, headRef string, out *Output) bool                    // returns "push succeeded"
  ```

These three step-functions replace the `_ = bugs / _ = workDir / _ = headRef` stubs in Task 10's loop body.

- [ ] **Step 1: Replace the bug-fix stub in the loop**

In `Run`, replace this block:

```go
			// Bug fixes + lint/format + commit + push lands in Task 11.
			// For now this branch is a stub; until then the workflow
			// behaves like rerun-only.
			_ = bugs
			_ = workDir
			_ = headRef
```

with:

```go
			if len(bugs) > 0 {
				edited := applyBugFixes(ctx, workDir, bugs, issuesByKey)
				if edited {
					if runLintFormat(ctx, in.Repo, workDir) {
						if commitAndPush(ctx, in.Repo, workDir, headRef, &out) {
							commitMade = true
						}
					}
				}
			}
```

- [ ] **Step 2: Add the three new functions + imports**

Update the import block to add `git`:

```go
import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/go-playground/validator/v10"
	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/claude"
	"github.com/trento-project/trento-workflows/internal/activities/fs"
	"github.com/trento-project/trento-workflows/internal/activities/gh"
	"github.com/trento-project/trento-workflows/internal/activities/git"
	"github.com/trento-project/trento-workflows/internal/lib"
)
```

Append to `.workflows/internal/workflows/fixprci/workflow.go`:

```go
// applyBugFixes runs the fix-bug prompt for each bug sequentially.
// Returns true if any prompt left the working tree dirty (i.e. an agent
// actually edited something). NOT_A_CODE_FIX responses reclassify the
// issue to unfixable so they won't be retried.
func applyBugFixes(ctx restate.Context, workDir string, bugs []issue, byKey map[string]*IssueOutcome) bool {
	edited := false
	for _, b := range bugs {
		dirtyBefore, _ := runT(ctx, "git.Dirty:before:"+b.IssueKey, func(rctx restate.RunContext) (bool, error) {
			return git.Dirty(rctx, workDir)
		})
		prompt := strings.NewReplacer(
			"{{repo}}", "",
			"{{issueKey}}", b.IssueKey,
			"{{summary}}", b.Summary,
			"{{hint}}", b.Hint,
			"{{relevantLogExcerpt}}", "", // filled below
		).Replace(fixBugTmpl)
		// Replace {{repo}} and {{relevantLogExcerpt}} placeholders with the
		// values from the issue. {{relevantLogExcerpt}} would be the
		// tailed logs for this issue's jobRefs, but for v1 the hint
		// itself carries enough context — we keep the placeholder empty
		// to avoid re-fetching logs that the analyze prompt already saw.

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
		if strings.TrimSpace(resp.Text) == "NOT_A_CODE_FIX" {
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

// runLintFormat dispatches the per-repo lint+format commands in workDir
// sequentially. Returns true iff every command exits 0. A non-zero exit
// from any command short-circuits the rest and returns false (the
// commit is then skipped for this iteration).
func runLintFormat(ctx restate.Context, repo, workDir string) bool {
	cmds := lintFormatCmds(repo)
	for i, argv := range cmds {
		label := fmt.Sprintf("lintFormat:%d:%s", i, argv[0])
		ok, err := runT(ctx, label, func(rctx restate.RunContext) (bool, error) {
			_, _, code, err := lib.Sh(rctx, workDir, argv...)
			if err != nil {
				return false, err
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
func commitAndPush(ctx restate.Context, repo, workDir, headRef string, out *Output) bool {
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

	// One rebase-and-retry attempt for non-FF rejection.
	if err := runV(ctx, "git.FetchOrigin:"+headRef, func(rctx restate.RunContext) error {
		return git.FetchOrigin(rctx, workDir, headRef)
	}); err != nil {
		return false
	}
	if err := runV(ctx, "git.RebaseOrigin:"+headRef, func(rctx restate.RunContext) error {
		_, err := lib.MustSh(rctx, workDir, "git", "rebase", "origin/"+headRef)
		return err
	}); err != nil {
		return false
	}
	if pushErr := tryPush(ctx, workDir, headRef, "retry"); pushErr != nil {
		return false
	}
	out.Commits = append(out.Commits, sha)
	return true
}

// tryPush wraps git.PushBranch (force-with-lease=false; we want plain
// push so the rebase-retry path can distinguish a non-FF rejection
// from other failures via the activity error).
func tryPush(ctx restate.Context, workDir, headRef, tag string) error {
	return runV(ctx, "git.PushBranch:"+headRef+":"+tag, func(rctx restate.RunContext) error {
		return git.PushBranch(rctx, workDir, headRef, false)
	})
}
```

- [ ] **Step 3: Build + vet + test**

Run: `cd .workflows && go build ./... && go vet ./... && go test ./...`
Expected: exits 0; tests still PASS.

- [ ] **Step 4: Commit**

```bash
git add .workflows/internal/workflows/fixprci/workflow.go
git commit -m "fixprci: implement bug-fix + lint/format + commit/push step"
```

---

### Task 12: Register the workflow in the handler + Makefile target

**Files:**
- Modify: `.workflows/cmd/handler/main.go`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `fixprci.Register`.

- [ ] **Step 1: Update handler binding**

In `.workflows/cmd/handler/main.go`, replace this block:

```go
import (
	"context"
	"os"

	"github.com/restatedev/sdk-go/server"

	"github.com/trento-project/trento-workflows/internal/lib"
	"github.com/trento-project/trento-workflows/internal/workflows/patchrelease"
	"github.com/trento-project/trento-workflows/internal/workflows/testonobs"
)
```

with:

```go
import (
	"context"
	"os"

	"github.com/restatedev/sdk-go/server"

	"github.com/trento-project/trento-workflows/internal/lib"
	"github.com/trento-project/trento-workflows/internal/workflows/fixprci"
	"github.com/trento-project/trento-workflows/internal/workflows/patchrelease"
	"github.com/trento-project/trento-workflows/internal/workflows/testonobs"
)
```

In the same file, replace:

```go
	endpoint := server.NewRestate().
		Bind(testonobs.Register()).
		Bind(testonobs.RegisterSubmoduleService()).
		Bind(patchrelease.Register()).
		Bind(patchrelease.RegisterRepoService())
```

with:

```go
	endpoint := server.NewRestate().
		Bind(testonobs.Register()).
		Bind(testonobs.RegisterSubmoduleService()).
		Bind(patchrelease.Register()).
		Bind(patchrelease.RegisterRepoService()).
		Bind(fixprci.Register())
```

- [ ] **Step 2: Build handler**

Run: `cd .workflows && go build ./cmd/handler`
Expected: exits 0; binary `handler` produced (not tracked).

Run: `rm -f .workflows/handler`
Cleanup the local-build artifact.

- [ ] **Step 3: Add Makefile target**

Read the existing `Makefile` to find a good insertion point (after any other `wf-*` / workflow targets if present).

Run: `grep -n 'wf-\|workflows' Makefile || echo "no existing wf targets"`

Append a new target at the end of `Makefile`:

```make
fix-pr-ci:          ## Watch a PR's CI and fix iteratively. REPO=owner/name PR=N (defaults MaxAttempts=5 MaxIterations=15)
	@if [ -z "$(REPO)" ] || [ -z "$(PR)" ]; then \
		echo "usage: make fix-pr-ci REPO=owner/name PR=<number>"; exit 1; \
	fi
	@hack/workflows.sh run fix-pr-ci --json '{"repo":"$(REPO)","prNumber":$(PR)}'
```

Note: this target assumes `hack/workflows.sh` exists with a `run` subcommand. Per the design doc § "Make integration and day-to-day UX" of the parent dev-workflow orchestrator spec, `hack/workflows.sh` is the planned shim. If it does not yet exist in the repo, leave the target in place — running it will fail with a clear "command not found" error, and the script lands as part of the orchestrator's own rollout.

- [ ] **Step 4: Final whole-module build + vet + test**

Run: `cd .workflows && go build ./... && go vet ./... && go test ./...`
Expected: exits 0; all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add .workflows/cmd/handler/main.go Makefile
git commit -m "fixprci: register workflow in handler + add make target"
```

---

## Self-Review

**Spec coverage:**

| Spec section | Covered by |
|---|---|
| Input/Output schema | Task 5 (`schema.go`) |
| New gh activities (RunListForSHA, JobsForRun, JobLogs, RunRerunFailed, PRHeadSHA, PRHeadRef, PRCheckout) | Tasks 1, 2, 3 |
| New git activity (CommitFixup) | Task 4 |
| Workflow shape — setupCheckout + loop | Tasks 9 (setup), 10 (loop body), 11 (bug/commit/push step) |
| `waitForRuns` adaptive cadence (10s × 12, then 60s × 58, 60min cap) | Task 8 |
| Issue analysis prompt | Task 7 + Task 10 (rendering & dispatch) |
| Bug-fix prompt | Task 7 + Task 11 (rendering & dispatch) |
| `lintFormatCmds` table + hard-fail policy | Task 6 (table + tests), Task 11 (`runLintFormat` short-circuits on non-zero) |
| Edge case: PR closed mid-loop | Task 10 (`gh.PRHeadSHA` error → `FinalStatusAborted`) |
| Edge case: no CI runs for SHA | Task 10 (`len(runs) == 0` → `FinalStatusGreen`) |
| Edge case: poll hard timeout | Task 8 (`waitForRuns` returns error) → Task 10 maps to aborted |
| Edge case: push rejected | Task 11 (`commitAndPush` rebase-retry) |
| Edge case: fork PR | Task 3 (`gh.PRCheckout` handles transparently) |
| Edge case: Claude analysis unparseable | Task 10 (`ok==false` → falls through to no-progress) |
| Edge case: `NOT_A_CODE_FIX` | Task 11 (`applyBugFixes` reclassifies to unfixable) |
| Edge case: lint non-zero exit | Task 11 (`runLintFormat` returns false → commit skipped) |
| Edge case: MaxIterations hit | Task 10 (loop exits → `FinalStatusExhausted`) |
| Edge case: `CleanupOnExit=false` | Task 9 (`maybeCleanup` no-ops when false) |
| Handler registration | Task 12 |
| Makefile target | Task 12 |

**Placeholder scan:** no "TBD" / "TODO" / vague language; each step shows the exact code, the exact run command, and the expected outcome. The `{{relevantLogExcerpt}}` placeholder in the fix-bug prompt is left blank by design (documented in Task 11 — the hint carries the context).

**Type consistency:**
- `issue` (lowercase, internal) vs `IssueOutcome` (exported, report) — distinct on purpose; `recordIssue` copies fields from one to the other.
- `gh.WorkflowRun.ID` is `int64` everywhere it appears.
- `splitJobRef` / `uniqueRunIDsFromIssues` both deal in `int64` run IDs.
- `lintFormatCmds` returns `[][]string`; `runLintFormat` iterates with `for _, argv := range cmds`.
- `tryPush` uses `git.PushBranch` with `forceWithLease=false`; matches the signature of the existing `git.PushBranch`.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-06-17-fix-pr-ci-workflow.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

2. **Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
