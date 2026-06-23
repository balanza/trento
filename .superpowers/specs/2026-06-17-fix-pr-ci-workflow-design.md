# Fix-PR-CI workflow — design

A durable-execution workflow on the developer's laptop that, given an open
PR on one of the Trento submodule repos, watches its GitHub Actions CI to
completion, classifies failures into actionable categories using a single
Claude correlation prompt, reruns flaky/infra jobs automatically, and asks
Claude to fix any real bugs — committing autofixes as `fixup!` commits and
pushing them back to the PR head. Loops until CI is green, all remaining
issues are unfixable/exhausted, or the iteration cap is reached.

The workflow is autonomous (no human-in-the-loop gate) and is invoked as
`trento.fix-pr-ci` against the same Restate runtime that already hosts
`trento.test-on-obs` and `trento.patch-release`.

## Goals

- Automate the most tedious part of "drive a PR to green": polling CI,
  distinguishing flakes from real bugs, rerunning what only needs to be
  rerun, and fixing what can be fixed with a focused agent call.
- Reuse the existing `.workflows/` Restate orchestrator and its activity
  packages (`gh`, `git`, `claude`, `fs`) with the minimum number of
  additions (three new `gh` functions, one new `git` function).
- Keep the workflow safe to re-run on the same PR: the virtual-object key
  is `<repo>#<prNumber>` so concurrent invocations serialize; resumed runs
  pick up where the previous attempt left off via Restate's journal.
- Stay within the codebase convention that anything non-deterministic
  lives in an activity (no `time.Now()` / `rand` / goroutines in workflow
  code).

## Non-goals (v1)

- **Human-in-the-loop approval.** Run is fully autonomous. The user
  supervises via the streaming output and can `hack/workflows.sh cancel`.
  HITL gating can be added later (mirror `testonobs.FixIterate`'s
  `auto|review|off` enum) if autonomy proves too aggressive.
- **Operating on the user's working checkout.** Workflow always clones the
  PR head into a fresh dir under `.workflows/.runs/<id>/work` so the
  user's local submodule checkouts are never touched.
- **Repo-level concurrency beyond one PR at a time.** Different PRs on
  the same submodule run in parallel; concurrent invocations on the same
  `(repo, prNumber)` pair queue.
- **Cross-PR coordination.** Bug fixes for PR A do not consider PR B.
- **Multi-submodule input.** One PR per invocation. A wrapper that
  enumerates open PRs and fans out can come later if needed.
- **Local test execution.** Bug-fix prompts must not run the test suite —
  too slow on the developer's laptop. CI is the verification loop.

## Repo layout

Everything new lives under
`.workflows/internal/workflows/fixprci/`, plus a handful of additions
to existing activity packages.

```
.workflows/
├── internal/
│   ├── activities/
│   │   ├── gh/gh.go            # +RunListForSHA, JobsForRun, JobLogs,
│   │   │                       #  RunRerunFailed, PRHeadSHA, PRHeadRef,
│   │   │                       #  PRCheckout
│   │   └── git/git.go          # +CommitFixup
│   └── workflows/
│       └── fixprci/            # NEW workflow
│           ├── workflow.go     # the loop
│           ├── schema.go       # Input / Output / IssueOutcome
│           ├── helpers.go      # adaptive poll, log bundling,
│           │                   #  lintFormatCmds, parse helpers
│           └── prompts/
│               ├── analyze-logs.md
│               └── fix-bug.md
```

The handler in `cmd/handler/main.go` registers the new workflow next to
the existing two.

## Input / Output schema

```go
package fixprci

type Input struct {
    // Repo as owner/name, e.g. "trento-project/web".
    Repo string `json:"repo" validate:"required,contains=/"`

    // PR number on Repo.
    PRNumber int `json:"prNumber" validate:"required,min=1"`

    // Per-issueKey attempt cap before an issue is marked exhausted.
    // Default 5.
    MaxAttempts int `json:"maxAttempts" validate:"min=1,max=20"`

    // Total iteration hard cap (safety net for the outer loop).
    // Default 15.
    MaxIterations int `json:"maxIterations" validate:"min=1,max=50"`

    // If true, drop the temp clone after the workflow ends. Default false
    // (state kept for inspection; user prunes via hack/workflows.sh prune
    // or manually under .workflows/.runs/).
    CleanupOnExit bool `json:"cleanupOnExit"`
}

type FinalStatus string

const (
    FinalStatusGreen     FinalStatus = "green"      // every CI run terminal-success
    FinalStatusExhausted FinalStatus = "exhausted"  // attempts hit / no-progress
    FinalStatusAborted   FinalStatus = "aborted"    // PR closed, timeout, push conflict
)

type Output struct {
    Repo        string         `json:"repo"`
    PRNumber    int            `json:"prNumber"`
    FinalStatus FinalStatus    `json:"finalStatus"`
    Iterations  int            `json:"iterations"`
    Commits     []string       `json:"commits"` // fixup SHAs we pushed
    Issues      []IssueOutcome `json:"issues"`  // every issue seen across the run
}

type IssueOutcome struct {
    IssueKey   string   `json:"issueKey"`
    Category   string   `json:"category"`   // flaky|bug|infra|unfixable
    Summary    string   `json:"summary"`
    Attempts   int      `json:"attempts"`
    FinalState string   `json:"finalState"` // fixed|exhausted|unfixable|still-flaky|rerun-only
    JobRefs    []string `json:"jobRefs"`    // "run/<runID>/job/<jobID>"
}
```

**Virtual-object key**: `fmt.Sprintf("%s#%d", in.Repo, in.PRNumber)`.

**Defaults applied in `applyDefaults`**: `MaxAttempts=5`, `MaxIterations=15`,
`CleanupOnExit=false`.

## New activities

### `gh` package additions

```go
type WorkflowRun struct {
    ID         int64
    Name       string
    Status     string  // queued | in_progress | completed
    Conclusion string  // success | failure | cancelled | timed_out | "" until terminal
    HeadSHA    string
    URL        string
}

type WorkflowJob struct {
    ID         int64
    RunID      int64
    Name       string
    Status     string
    Conclusion string
    Steps      []JobStep
}

type JobStep struct {
    Name       string
    Number     int
    Conclusion string  // success | failure | skipped
}

// RunListForSHA lists workflow runs whose head SHA matches.
// Wraps `gh api repos/<repo>/actions/runs?head_sha=<sha>` + jq.
func RunListForSHA(ctx context.Context, repo, sha string) ([]WorkflowRun, error)

// JobsForRun returns the jobs of one run, including step-level details.
// Wraps `gh api repos/<repo>/actions/runs/<runID>/jobs`.
func JobsForRun(ctx context.Context, repo string, runID int64) ([]WorkflowJob, error)

// JobLogs returns the raw log text for one job. gh follows the
// presigned-URL redirect that the GitHub API returns transparently.
// Wraps `gh api repos/<repo>/actions/jobs/<jobID>/logs`.
func JobLogs(ctx context.Context, repo string, jobID int64) (string, error)

// RunRerunFailed reruns only the failed jobs of one run.
// Wraps `gh api -X POST repos/<repo>/actions/runs/<runID>/rerun-failed-jobs`.
func RunRerunFailed(ctx context.Context, repo string, runID int64) error

// PRHeadSHA returns the current head SHA of the PR. Refreshed each
// iteration since we push new commits.
func PRHeadSHA(ctx context.Context, repo string, prNumber int) (string, error)

// PRHeadRef returns the head branch name of the PR (the local ref name
// to push to). For fork PRs, this is still the branch name on the fork;
// gh pr checkout sets up the remote so `git push origin <ref>` works.
func PRHeadRef(ctx context.Context, repo string, prNumber int) (string, error)

// PRCheckout checks out the PR's head into the cloned repo. Wraps
// `gh pr checkout <num>` which handles fork PRs transparently.
func PRCheckout(ctx context.Context, repoDir string, prNumber int) error
```

### `git` package addition

```go
// CommitFixup runs `git commit --fixup=HEAD --no-verify` so the user
// can autosquash later. Returns the new commit short SHA.
// --no-verify because we want to skip the user's pre-commit hooks: the
// workflow already ran lint+format in lintFormatCmds.
func CommitFixup(ctx context.Context, repoPath string) (string, error)
```

## Workflow shape

**The loop** lives in a single workflow function (no sub-services — one
PR per invocation, no fanout).

```
clone PR head into .runs/<id>/work
loop iter = 1..MaxIterations:
  1. sha = gh.PRHeadSHA                 — refreshed each iter (we push)
  2. runs = waitForRuns(sha)            — adaptive poll until terminal
  3. failed = filterFailedRuns(runs)
  4. if len(failed) == 0 → FinalStatus=green, return
  5. for each failed run: gh.JobsForRun → filter failed jobs → gh.JobLogs
     (job fetches parallelized across the failed-run set)
     logBundle = concatenated, each entry prefixed with
     `=== run/<runID> job/<jobID> name=<jobName> conclusion=<conclusion> ===`
  6. analysis = claude.Invoke(analyze-logs.md, logBundle)
  7. parse analysis → []Issue. If unparseable: skip to step 13 with
     commitMade=false, reranSomething=false (counts as no-progress).
  8. for each issue:
       attempts[issueKey]++
       record into issuesByKey
       if attempts > MaxAttempts → mark exhausted, drop from active
       if category == unfixable  → mark unfixable, drop from active
  9. if active is empty → FinalStatus=exhausted, break
  10. split active into { bugs, rerunable (flaky+infra) }
  11. if bugs:
        for each bug (sequential): claude.Invoke(fix-bug.md) in workDir
        if git.Dirty:
          runLintFormat(in.Repo)        — non-zero exit aborts this commit
          if still dirty after lint:
            git.Add(.), git.CommitFixup → push to PRHeadRef
            commitMade = true
  12. if rerunable AND !commitMade:
        — a push creates a new SHA whose CI re-runs everything anyway, so
          explicit reruns are only needed when NOTHING was pushed.
        unique = uniqueRunIDsFromIssues(rerunable)
        for each runID: gh.RunRerunFailed
        reranSomething = true
  13. if !commitMade && !reranSomething:
        noProgress++
        if noProgress >= 2 → FinalStatus=exhausted, break
      else:
        noProgress = 0
finalize issue states, return Output
```

**`waitForRuns` adaptive cadence**: poll every 10s for the first 2 minutes
(12 attempts), then every 60s up to a 60-minute hard cap (58 attempts).
60 minutes covers the slowest known CI runs by a comfortable margin and
matches the spirit of `testonobs`'s timeouts. Implemented with
`restate.After` so the workflow suspends between polls (zero CPU).

**Failure semantics**:

- Activity-level failures use Restate's default retry policy (exponential
  with cap) for transient `gh`/`git` errors. Claude calls get one shot —
  retrying is expensive and rarely useful. Lint/format commands likewise
  get one shot.
- Workflow-level failures (PR closed, push rejected after one rebase
  attempt, poll timeout) set `FinalStatus=aborted` and return cleanly,
  not as an error.

## Issue analysis prompt

`prompts/analyze-logs.md` — single Claude call per iteration.

**Input substitutions**: `{{repo}}`, `{{prNumber}}`, `{{headSHA}}`,
`{{logBundle}}`. The bundle is the concatenated logs of every failed job,
each prefixed with:

```
=== run/<runID> job/<jobID> name=<jobName> conclusion=<conclusion> ===
<log text>
```

**Prompt body** (paraphrased; actual template in the file):

- You are diagnosing CI failures on PR #{{prNumber}} of {{repo}}, head
  {{headSHA}}.
- Read all logs together. Look for cross-job correlations — two jobs
  failing for the same underlying cause are one issue, not two.
- Group failures into distinct issues. For each issue, output:
  - **`issueKey`** — stable identifier reused across iterations to count
    attempts. Format: `<category>:<short-stable-discriminator>`. Examples:
    `bug:web:undefined-var:foo.ex:42`,
    `flaky:agent:test_TestRegisterHost`,
    `infra:runner-disconnect`,
    `unfixable:upstream:hex-package-yanked`.
    Rules: lowercase; no timestamps, no run/job IDs, no SHAs, no PIDs;
    must be byte-identical for the same root cause across iterations.
  - **`category`** — one of `flaky | bug | infra | unfixable`.
    - `flaky`: test pass/fail is non-deterministic; rerun should clear it.
    - `bug`: a real code defect in this repo, fixable by editing source.
    - `infra`: CI infrastructure failure (runner died, network blip,
      image pull failure, dependency-registry 5xx). Rerun should clear it.
    - `unfixable`: upstream dependency yanked, breaking change in a
      third-party API, requires a product decision, etc. — not actionable
      from inside this repo.
  - **`summary`** — ≤200 chars, plain-language description.
  - **`jobRefs`** — array of `run/<runID>/job/<jobID>` strings for every
    job exhibiting this issue.
  - **`hint`** — ≤500 chars actionable hint for a fix prompt: file/line
    if visible, suspected cause, suggested edit. For `flaky`/`infra`/
    `unfixable` categories `hint` can be empty.
- Output: a JSON array wrapped in `<analysis>...</analysis>` tags. No
  prose outside the tags. Unparseable output is treated as a no-progress
  iteration.
- Tool surface: `Read` only (logs are passed inline; no need for Edit).

## Bug-fix prompt

`prompts/fix-bug.md` — one Claude call per bug, in `workDir`.

**Input substitutions**: `{{repo}}`, `{{issueKey}}`, `{{summary}}`,
`{{hint}}`, `{{relevantLogExcerpt}}` (the logs of jobs in `jobRefs`,
each truncated to last 200 lines).

**Prompt body** (paraphrased):

- You are inside a checkout of `{{repo}}` at the PR's head branch.
- Goal: make the minimum edit needed to fix the described issue.
- Use `Bash`, `Read`, `Edit`, `Write` freely.
- Do NOT run `git commit` or `git push`. The workflow handles staging,
  committing, and pushing after lint+format pass.
- Do NOT run the test suite. CI is the verification loop; running tests
  locally just wastes wall-clock.
- If after investigation the issue is NOT addressable from inside this
  repo (it's in a dependency, in CI infrastructure, or needs a product
  decision), output exactly `NOT_A_CODE_FIX` and exit without editing.
  The workflow will reclassify the issue to `unfixable` and stop retrying.
- Tool surface: `Bash, Read, Edit, Write`.

**Safety rails around each fix call**:

1. Workflow snapshots `git rev-parse HEAD` and the dirty-file list before
   each prompt; diffs after. If the agent touched files outside the repo
   root or changed >500 lines, the issue's `FinalState` is annotated
   `large-edit-warning` but the change is not blocked.
2. `NOT_A_CODE_FIX` output reclassifies the issue to `unfixable`.

## Lint and format

Before each `git.CommitFixup`, the workflow dispatches the
`(repo → []argv)` table below in `workDir`. Sequential execution; any
non-zero exit aborts the commit for this iteration (the iteration counts
as no-progress).

```go
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
        return nil // no lint/format pipeline configured; skip step
    default:
        return nil
    }
}
```

**Rationale**:

- `web` is Phoenix/Elixir + JS assets, so it runs `mix format` plus the
  assets' npm `format` (Prettier) and `lint:fix` (ESLint with --fix).
- `wanda` has no Makefile and no JS assets — `mix format` only.
- `agent` and `mcp-server` have full Makefile pipelines (`make fmt`
  wraps `gofmt`; `make lint` wraps `golangci-lint` + license/shellcheck/
  yamllint/asciidoc).
- `checks` (YAML catalog) and `contracts` (protobuf-generation only)
  have no obvious format/lint surface.

**Toolchain assumption**: `mix`, `npm`, `make`, `gh`, `git` must be on
PATH — same trust model as `testonobs` (`mix`, `osc`, `distrobox`) and
`patchrelease` (`gh`, `git`).

**Hard-fail policy**: a non-zero exit from any command prevents the
commit/push, increments `noProgress`. The lint output is not fed back to
Claude — the next iteration's CI logs will surface the same lint
failures, at which point the analyze-logs prompt can categorize them.

## Edge cases

| Case | Behavior |
|---|---|
| PR closed/merged mid-loop | `gh.PRHeadSHA` returns empty/error → `FinalStatus=aborted`, log "PR no longer open" |
| No CI runs for the SHA | `gh.RunListForSHA` returns empty → `FinalStatus=green`, exit cleanly |
| Poll hard timeout (60 min) | `waitForRuns` gives up → `FinalStatus=aborted` |
| Push rejected (non-FF) | One `git fetch origin <ref>` + `git rebase origin/<ref>` + retry push. On 2nd rejection: `FinalStatus=aborted` |
| Fork PR | `gh pr checkout` sets up remote + tracking; push uses the same remote |
| Claude analysis unparseable | Iteration produces no commit/rerun → contributes to `noProgress` (2-strike bail) |
| `fix-bug.md` returns `NOT_A_CODE_FIX` | Issue reclassified to `unfixable`, not retried |
| Lint/format non-zero exit | No commit this iteration. Counts as no-progress only if no rerun also triggered (mixed bug+flaky iter where lint blocks the bug fix still benefits from the rerun) |
| MaxIterations hit while issues active | `FinalStatus=exhausted` |
| `CleanupOnExit=false` after any exit | workDir preserved under `.workflows/.runs/<id>/work` for inspection |

## Make integration

One new Make target, delegating to `hack/workflows.sh`:

```make
fix-pr-ci:          ## Watch a PR's CI and fix iteratively. REPO=owner/name PR=N
	@hack/workflows.sh run fix-pr-ci --json '{"repo":"$(REPO)","prNumber":$(PR)}'
```

Typical session:

```
$ make fix-pr-ci REPO=trento-project/web PR=2412
==> Workflow trento.fix-pr-ci/abc started (repo=trento-project/web prNumber=2412)
==> iter 1: cloning + checking out PR head
==> iter 1: polling 4 CI runs for sha 7f3c... (adaptive)
==> iter 1: 3 ok, 1 failed (run/8123 mix-test)
==> iter 1: analysis → 1 issue (bug:web:undefined-var:user_controller.ex:34)
==> iter 1: fixing bug:web:undefined-var:user_controller.ex:34 ...
==> iter 1: mix format ok | npm --prefix assets run format ok | ... 
==> iter 1: pushed fixup commit 3a9bc12 to fix/user-prefs
==> iter 2: polling 4 CI runs for sha 3a9b... 
==> iter 2: all green
==> workflow completed in 14m22s
==> Output: { finalStatus: green, iterations: 2, commits: ["3a9bc12"], issues: [...] }
```

## Migration / rollout

Single landing step (no obs-test.sh-style parallel-run period needed —
this workflow has no preexisting bash equivalent to diverge from):

1. Land the new activity functions (`gh.*`, `git.CommitFixup`) with
   unit tests where applicable (parse helpers, dispatch tables).
2. Land the `fixprci/` package with the workflow, prompts, helpers, and
   integration tests (gated by build tag `fixprci_live` for the ones
   that hit real GitHub).
3. Register in `cmd/handler/main.go`.
4. Add the `fix-pr-ci` Make target.
5. Update `.workflows/README.md` with a short usage example.

## Risk register

- **Claude `issueKey` instability across iterations.** If Claude varies
  the discriminator wording, the same root cause looks like two issues
  and the per-issue attempt cap doesn't bite. Mitigation: explicit rules
  in the prompt (no timestamps/SHAs/PIDs, lowercase, examples given);
  worst case the outer `MaxIterations` still terminates the loop.
- **`mix`/`npm`/`make` not on PATH.** A user on a fresh machine gets a
  cryptic lint failure that aborts every commit. Mitigation: same as
  `testonobs` — preflight could be added later if pain accumulates;
  v1 just relies on the install instructions.
- **Push rejected race.** If the PR author pushes from another machine
  while the workflow is running, the rebase-and-retry handles the
  one-commit case. A churning PR can outpace the workflow; aborting
  cleanly is the correct behavior.
- **Lint loop fighting Claude.** If Claude consistently introduces lint
  violations that auto-fix cannot resolve, the workflow ships no commit
  and the same bug recurs next iteration. The 2-strike no-progress
  detector + per-ishsue attempt cap eventually terminates this.
- **Autonomous commit and push.** No HITL gate means a misclassified
  flake getting "fixed" can introduce real changes silently. Mitigation:
  fixup commits are easy to identify (`git log --oneline | grep '^fixup!'`)
  and easy to drop (`git reset --hard HEAD~N` or `--autosquash`-then-edit).

## Open questions deferred to implementation

- Whether the analyze-logs prompt should also emit `relevantLines` per
  issue (a tighter excerpt than the full job log) to keep the fix-bug
  prompt focused. Easy to add if hits show the fix prompts arreen" false positive).e too
  context-heavy.
- Whether to add a `RestartPolicy` input that decides between
  `rerun-failed-jobs` (current default) vs `rerun` (whole run). Reruns
  of the whole run might be needed when GitHub Actions reports the
  failure outside any specific job. v1 starts with `rerun-failed-jobs`
  only.
- Whether `lintFormatCmds` should later be data-driven (e.g. read from a
  per-repo manifest file under `.workflows/configs/`) rather than
  hardcoded in Go. v1 hardcodes; revisit if a third workflow needs the
  same table.
