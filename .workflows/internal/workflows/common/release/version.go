// Package release contains pure helpers shared by every workflow that
// touches the Trento release-process state machine (version parsing,
// submodule enumeration, VERSION-file reads). The package is
// deliberately small and side-effect-free: any I/O lives behind an
// activity closure the caller passes in.
package release

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"

	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/gh"
	"github.com/trento-project/trento-workflows/internal/activities/git"
)

// VersionParts is the number of dot-separated components a Trento
// VERSION has. Used by BumpPatch and its tests.
const VersionParts = 3

// SplitOwnerName splits "owner/name" into its parts. Returns ok=false
// when the input lacks a single slash separator.
func SplitOwnerName(repo string) (string, string, bool) {
	idx := strings.Index(repo, "/")
	if idx <= 0 || idx == len(repo)-1 {
		return "", "", false
	}
	return repo[:idx], repo[idx+1:], true
}

// BumpPatch bumps the patch component of a semver X.Y.Z by one.
// Returns an error when v is not a 3-part numeric version.
func BumpPatch(v string) (string, error) {
	parts := strings.Split(v, ".")
	if len(parts) != VersionParts {
		return "", fmt.Errorf("not a semver: %q", v)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", fmt.Errorf("invalid patch in %q: %w", v, err)
	}
	return fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch+1), nil
}

// DecodeBase64Trim decodes a base64 (possibly with surrounding
// whitespace) string and trims the result. Used for the JSON `content`
// field GitHub returns for the VERSION file.
func DecodeBase64Trim(s string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// RunT wraps restate.Run with a label and tags any error with that
// label. Shared across workflow packages so each can keep its own
// helpers.go trivial.
func RunT[T any](
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

// ResolveVersions reads the release-branch VERSION file for `repo`
// and returns (current, next). On failure returns "", "", false.
func ResolveVersions(ctx restate.Context, repo string) (string, string, bool) {
	current, err := RunT(ctx, "readVersion:"+repo, func(rctx restate.RunContext) (string, error) {
		raw, ghErr := gh.APIGet(rctx, "repos/"+repo+"/contents/VERSION?ref=release", ".content")
		if ghErr != nil {
			return "", fmt.Errorf("gh api VERSION: %w", ghErr)
		}
		return DecodeBase64Trim(raw)
	})
	if err != nil {
		return "", "", false
	}
	next, err := BumpPatch(current)
	if err != nil {
		return "", "", false
	}
	return current, next, true
}

// ResolveRepoRoot returns the path to the super-repo checkout the
// workflow should read .gitmodules from. Reads TRENTO_REPO_ROOT for
// override; defaults to ".." (the parent of .workflows/, which is the
// super-repo root when the handler is started via the Makefile).
func ResolveRepoRoot(ctx restate.Context) (string, error) {
	return RunT(ctx, "resolveRepoRoot", func(_ restate.RunContext) (string, error) {
		if rr := os.Getenv("TRENTO_REPO_ROOT"); rr != "" {
			return rr, nil
		}
		return "..", nil
	})
}

// ResolveRepos returns the list of `owner/name` submodule repos. If
// `filter` is non-empty, only submodules whose short name is in
// `filter` are returned. The `repoRoot` argument is the path to the
// local super-repo (parent of .gitmodules).
func ResolveRepos(ctx restate.Context, repoRoot string, filter []string) ([]string, error) {
	all, err := RunT(ctx, "submodulesFromGitmodules", func(rctx restate.RunContext) ([]string, error) {
		return git.SubmodulesFromGitmodules(rctx, repoRoot+"/.gitmodules")
	})
	if err != nil {
		return nil, err
	}
	if len(filter) == 0 {
		return all, nil
	}
	want := make(map[string]struct{}, len(filter))
	for _, f := range filter {
		want[f] = struct{}{}
	}
	out := make([]string, 0, len(filter))
	for _, repo := range all {
		_, name, ok := SplitOwnerName(repo)
		if !ok {
			continue
		}
		if _, hit := want[name]; hit {
			out = append(out, repo)
		}
	}
	return out, nil
}
