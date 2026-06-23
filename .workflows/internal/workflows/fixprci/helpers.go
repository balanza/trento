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
// (no-progress signal). Returns nil for repos with no configured
// toolchain — the commit step then proceeds directly.
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
	default:
		// trento-project/checks, trento-project/contracts: no lint/format pipeline.
		return nil
	}
}

// failedRuns returns the subset of runs that ended in a non-success
// terminal state. Cancelled and timed_out are treated as failures: the
// run did not pass and Claude needs to see whatever logs exist.
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

// allRunsTerminal reports whether every run is in the "completed" status.
// Returns false for an empty slice — callers decide what "no runs" means.
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

// tailLines returns the last n lines of s, or all of s if fewer.
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
// JSON output. The fields match the prompt's contract exactly.
type issue struct {
	IssueKey string   `json:"issueKey"`
	Category string   `json:"category"`
	Summary  string   `json:"summary"`
	Hint     string   `json:"hint"`
	JobRefs  []string `json:"jobRefs"`
}

// parseAnalysis extracts the JSON array between <analysis>...</analysis>
// tags from Claude's response and unmarshals it. Returns ok=false on
// any failure — the workflow treats that as no-progress.
func parseAnalysis(s string) ([]issue, bool) {
	const open, closeTag = "<analysis>", "</analysis>"
	i := strings.Index(s, open)
	j := strings.LastIndex(s, closeTag)
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

// buildLogBundle concatenates the entries, each preceded by a header
// line Claude uses to populate jobRefs in its output.
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

// excerptForIssue returns the bundle entries for the jobRefs of one
// issue, with each log tailed to maxLines. Used by the fix-bug prompt
// to give the agent the relevant slice of logs without re-feeding the
// whole bundle.
func excerptForIssue(entries []logEntry, iss issue, maxLines int) string {
	wanted := map[int64]struct{}{}
	for _, ref := range iss.JobRefs {
		if _, jobID, ok := splitJobRef(ref); ok {
			wanted[jobID] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return ""
	}
	picked := make([]logEntry, 0, len(wanted))
	for _, e := range entries {
		if _, hit := wanted[e.JobID]; !hit {
			continue
		}
		picked = append(picked, logEntry{
			RunID: e.RunID, JobID: e.JobID, Name: e.Name,
			Conclusion: e.Conclusion, Log: tailLines(e.Log, maxLines),
		})
	}
	return buildLogBundle(picked)
}

// --- waitForRuns: adaptive poller ---

// waitForRuns polls gh.RunListForSHA until every run for `sha` is in a
// terminal status, or the hard cap is hit. Adaptive cadence: 10s for
// the first ~2 minutes, 60s after that. Hard cap ~60 minutes.
//
// Returns the final runs list on success. Returns an empty slice when
// no runs exist for this SHA (the workflow then treats it as "no CI
// configured" and exits green). Returns a non-nil error when the hard
// cap is hit before convergence; the caller maps that to aborted.
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
		if err := sleep(ctx, fastInterval); err != nil {
			return nil, err
		}
		runs, err := pollRuns(ctx, repo, sha, i+1)
		if err != nil {
			return nil, err
		}
		if allRunsTerminal(runs) {
			return runs, nil
		}
	}
	for i := 0; i < slowAttempts; i++ {
		if err := sleep(ctx, slowInterval); err != nil {
			return nil, err
		}
		runs, err := pollRuns(ctx, repo, sha, fastAttempts+i+1)
		if err != nil {
			return nil, err
		}
		if allRunsTerminal(runs) {
			return runs, nil
		}
	}
	return nil, fmt.Errorf("waitForRuns %s/%s: hard cap exceeded", repo, sha)
}

// pollRuns is one labeled gh.RunListForSHA call. The attempt index is
// part of the label so Restate journal entries are distinguishable on
// replay.
func pollRuns(ctx restate.Context, repo, sha string, attempt int) ([]gh.WorkflowRun, error) {
	label := fmt.Sprintf("gh.RunListForSHA:%s:%d", sha, attempt)
	return runT(ctx, label, func(rctx restate.RunContext) ([]gh.WorkflowRun, error) {
		return gh.RunListForSHA(rctx, repo, sha)
	})
}
