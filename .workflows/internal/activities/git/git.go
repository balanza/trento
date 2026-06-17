// Package git wraps the small set of git operations the workflows need.
// Every function shells out to the local `git` binary via lib.Sh.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trento-project/trento-workflows/internal/lib"
)

// ErrCherryPickConflict is the sentinel returned by CherryPick when the
// pick stopped on a merge conflict (the working tree is left in the
// conflicted state for the caller to resolve or abort).
var ErrCherryPickConflict = errors.New("git cherry-pick: merge conflict")

// Head returns the short SHA of HEAD in repoPath.
func Head(ctx context.Context, repoPath string) (string, error) {
	out, err := lib.MustSh(ctx, repoPath, "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git.Head: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Branch returns the current branch name, or empty string if HEAD is
// detached.
func Branch(ctx context.Context, repoPath string) (string, error) {
	out, _, code, err := lib.Sh(ctx, repoPath, "git", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git.Branch: %w", err)
	}
	if code != 0 {
		return "", nil // detached HEAD
	}
	return strings.TrimSpace(out), nil
}

// Describe returns `git describe --tags --always`.
func Describe(ctx context.Context, repoPath string) (string, error) {
	out, err := lib.MustSh(ctx, repoPath, "git", "describe", "--tags", "--always")
	if err != nil {
		return "", fmt.Errorf("git.Describe: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Dirty reports whether the working tree has uncommitted changes.
func Dirty(ctx context.Context, repoPath string) (bool, error) {
	out, err := lib.MustSh(ctx, repoPath, "git", "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git.Dirty: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// FetchOrigin runs `git fetch origin <ref> --quiet` inside repoPath.
func FetchOrigin(ctx context.Context, repoPath, ref string) error {
	if _, err := lib.MustSh(ctx, repoPath, "git", "fetch", "origin", ref, "--quiet"); err != nil {
		return fmt.Errorf("git.FetchOrigin %s: %w", ref, err)
	}
	return nil
}

// CheckoutNewBranch runs `git checkout -B <branch> <fromRef> --quiet`,
// defensively aborting any pending cherry-pick/merge/rebase first so a
// previous failed activity can't poison this one.
func CheckoutNewBranch(ctx context.Context, repoPath, branch, fromRef string) error {
	clearInProgressOps(ctx, repoPath)
	if _, err := lib.MustSh(ctx, repoPath, "git", "checkout", "-B", branch, fromRef, "--quiet"); err != nil {
		return fmt.Errorf("git.CheckoutNewBranch %s from %s: %w", branch, fromRef, err)
	}
	return nil
}

// CheckoutDetach runs `git checkout --detach <ref> --quiet`, with the
// same defensive cleanup as CheckoutNewBranch.
func CheckoutDetach(ctx context.Context, repoPath, ref string) error {
	clearInProgressOps(ctx, repoPath)
	if _, err := lib.MustSh(ctx, repoPath, "git", "checkout", "--detach", ref, "--quiet"); err != nil {
		return fmt.Errorf("git.CheckoutDetach %s: %w", ref, err)
	}
	return nil
}

// clearInProgressOps best-effort aborts any cherry-pick / merge /
// rebase so a follow-up checkout doesn't trip on `you need to resolve
// your current index first`. Failures are intentionally ignored —
// these all exit non-zero when nothing is pending, which is the common
// case.
func clearInProgressOps(ctx context.Context, repoPath string) {
	for _, op := range []string{"cherry-pick", "merge", "rebase"} {
		_, _, _, _ = lib.Sh(ctx, repoPath, "git", op, "--abort")
	}
}

// CherryPick runs `git cherry-pick <sha>`. Returns:
//   - nil on success.
//   - ErrCherryPickConflict on a merge conflict (working tree left
//     mid-pick; .git/CHERRY_PICK_HEAD exists).
//   - any other error for execution failures.
func CherryPick(ctx context.Context, repoPath, sha string) error {
	_, _, code, err := lib.Sh(ctx, repoPath, "git", "cherry-pick", sha)
	if err != nil {
		return fmt.Errorf("git.CherryPick %s: %w", sha, err)
	}
	if code == 0 {
		return nil
	}
	// Non-zero exit. Distinguish conflict from real failure by checking
	// the CHERRY_PICK_HEAD marker.
	inProgress, statErr := CherryPickInProgress(ctx, repoPath)
	if statErr != nil {
		return fmt.Errorf("git.CherryPick %s exited %d (probe failed): %w", sha, code, statErr)
	}
	if inProgress {
		return ErrCherryPickConflict
	}
	return fmt.Errorf("git.CherryPick %s exited %d", sha, code)
}

// CherryPickAbort runs `git cherry-pick --abort` (best-effort: returns
// an error only when git itself fails to start).
func CherryPickAbort(ctx context.Context, repoPath string) error {
	// Use Sh (not MustSh) — `--abort` can exit non-zero if no pick is
	// pending, which is fine for a cleanup call.
	_, _, _, err := lib.Sh(ctx, repoPath, "git", "cherry-pick", "--abort")
	if err != nil {
		return fmt.Errorf("git.CherryPickAbort: %w", err)
	}
	return nil
}

// CherryPickInProgress reports whether <repoPath>/.git/CHERRY_PICK_HEAD
// exists. Used to detect whether an AI tool has actually completed a
// pending cherry-pick.
func CherryPickInProgress(_ context.Context, repoPath string) (bool, error) {
	path := filepath.Join(repoPath, ".git", "CHERRY_PICK_HEAD")
	switch _, err := os.Stat(path); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("git.CherryPickInProgress stat %s: %w", path, err)
	}
}

// CherryPickContinue runs `git cherry-pick --continue --no-edit`. Used
// when an AI has staged resolutions but did not finalize the pick.
func CherryPickContinue(ctx context.Context, repoPath string) error {
	if _, err := lib.MustSh(ctx, repoPath, "git", "cherry-pick", "--continue", "--no-edit"); err != nil {
		return fmt.Errorf("git.CherryPickContinue: %w", err)
	}
	return nil
}

// Add runs `git add` for the given paths inside repoPath.
func Add(ctx context.Context, repoPath string, paths []string) error {
	cmd := append([]string{"git", "add", "--"}, paths...)
	if _, err := lib.MustSh(ctx, repoPath, cmd...); err != nil {
		return fmt.Errorf("git.Add %v: %w", paths, err)
	}
	return nil
}

// Commit runs `git commit -m <message> --quiet`.
func Commit(ctx context.Context, repoPath, message string) error {
	if _, err := lib.MustSh(ctx, repoPath, "git", "commit", "-m", message, "--quiet"); err != nil {
		return fmt.Errorf("git.Commit: %w", err)
	}
	return nil
}

// PushBranch pushes <branch> to origin. When forceWithLease is true,
// uses `--force-with-lease`.
func PushBranch(ctx context.Context, repoPath, branch string, forceWithLease bool) error {
	args := []string{"git", "push"}
	if forceWithLease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, "origin", branch, "--quiet")
	if _, err := lib.MustSh(ctx, repoPath, args...); err != nil {
		return fmt.Errorf("git.PushBranch %s: %w", branch, err)
	}
	return nil
}

// gitmodulesURLRegexp matches the trailing "<owner>/<name>.git" part of
// the URL in a .gitmodules submodule.url line. Handles both SSH
// (git@github.com:owner/name.git) and HTTPS forms.
var gitmodulesURLRegexp = regexp.MustCompile(`[:/]([^/]+/[^/]+?)(?:\.git)?\s*$`)

// SubmodulesFromGitmodules parses the given .gitmodules and returns the
// "owner/name" identifiers for each submodule URL. Mirrors the bash:
//
//	git config --file .gitmodules --get-regexp 'submodule\..*\.url' \
//	  | awk '{print $2}' \
//	  | sed -E 's|.*[:/]([^/]+/[^/]+)\.git$|\1|'
func SubmodulesFromGitmodules(ctx context.Context, gitmodulesPath string) ([]string, error) {
	out, err := lib.MustSh(ctx, "", "git", "config", "--file", gitmodulesPath, "--get-regexp", `submodule\..*\.url`)
	if err != nil {
		return nil, fmt.Errorf("git.SubmodulesFromGitmodules: %w", err)
	}
	var repos []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		m := gitmodulesURLRegexp.FindStringSubmatch(fields[1])
		if len(m) == 2 {
			repos = append(repos, m[1])
		}
	}
	return repos, nil
}
