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
