#!/usr/bin/env bash
# patch-release.sh — prepare a patch release across Trento repositories
#
# For each repo:
#   1. Reads the current version from the 'release' branch VERSION file
#   2. Finds merged PRs on main whose milestone title matches the next patch version
#   3. Opens a backport PR against 'release' for each, cherry-picking the commit
#   4. Creates a 'version-<next>' branch from 'release' with the bumped VERSION
#
# Usage: ./hack/patch-release.sh [--filter <mod1,mod2,...>]
# --filter  comma-separated submodule names to process (default: all submodules)
#
# Requirements: gh (authenticated), git, jq

set -euo pipefail

# ── Configuration ─────────────────────────────────────────────────────────────

REPO_ROOT=$(git rev-parse --show-toplevel)

FILTER=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --filter|-f)
      IFS=',' read -ra FILTER <<< "$2"
      shift 2
      ;;
    *)
      echo "Unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

mapfile -t ALL_REPOS < <(
  git config --file "$REPO_ROOT/.gitmodules" --get-regexp 'submodule\..*\.url' \
    | awk '{print $2}' \
    | sed -E 's|.*[:/]([^/]+/[^/]+)\.git$|\1|'
)

if [ ${#FILTER[@]} -eq 0 ]; then
  REPOS=("${ALL_REPOS[@]}")
else
  REPOS=()
  for repo in "${ALL_REPOS[@]}"; do
    for name in "${FILTER[@]}"; do
      if [ "${repo#*/}" = "$name" ]; then
        REPOS+=("$repo")
        break
      fi
    done
  done
fi

WORK_DIR=$(mktemp -d)
REPORT_FILE=$(mktemp)
FAILED_FILE=$(mktemp)
trap 'rm -rf "$WORK_DIR" "$REPORT_FILE" "$FAILED_FILE"' EXIT

# ── Helpers ───────────────────────────────────────────────────────────────────

bump_patch() {
  local major minor patch
  IFS='.' read -r major minor patch <<< "$1"
  echo "$major.$minor.$((patch + 1))"
}

ensure_labels() {
  local repo="$1"
  gh label create "backport-as-hotfix" \
    --repo "$repo" \
    --color "e4e669" \
    --description "Cherry-pick backport to release branch" \
    --force 2>/dev/null
  gh label create "skip-release-notes" \
    --repo "$repo" \
    --color "ffffff" \
    --description "Exclude this PR from the generated changelog" \
    --force 2>/dev/null
}

# ── Per-repo logic ────────────────────────────────────────────────────────────

process_repo() {
  local repo="$1"
  local repo_name="${repo#*/}"

  echo ""
  echo "════════════════════════════════════════"
  echo " $repo"
  echo "════════════════════════════════════════"

  # Current version from release branch
  local current next
  current=$(gh api "repos/$repo/contents/VERSION?ref=release" --jq '.content' \
    | base64 -d | tr -d '[:space:]')
  next=$(bump_patch "$current")
  echo "Version: $current → $next"

  ensure_labels "$repo"

  # Merged PRs on main with milestone = next version
  # gh pr list handles pagination internally; --search uses GitHub search syntax
  local prs pr_count
  prs=$(gh pr list \
    --repo "$repo" \
    --state merged \
    --base main \
    --search "milestone:\"$next\"" \
    --json number,title,mergeCommit \
    --limit 1000)
  pr_count=$(echo "$prs" | jq 'length')

  if [ "$pr_count" -eq 0 ]; then
    echo "No merged PRs found for milestone '$next' — skipping."
    return
  fi
  echo "Found $pr_count PR(s) to backport."

  # Clone repo into temp dir
  local dir="$WORK_DIR/$repo_name"
  echo "Cloning $repo into $dir..."
  gh repo clone "$repo" "$dir" -- --quiet
  cd "$dir"
  git fetch origin release --quiet

  # ── Backport each PR ────────────────────────────────────────────────────────

  local backport_pr_numbers=()
  while IFS= read -r -u 3 pr; do
    local num title sha branch
    num=$(echo "$pr" | jq -r '.number')
    title=$(echo "$pr" | jq -r '.title')
    sha=$(echo "$pr" | jq -r '.mergeCommit.oid')
    branch="backport-pr-${num}-to-release"

    echo ""
    echo "  → #$num: $title"

    local already_merged_url
    already_merged_url=$(gh pr list \
      --repo "$repo" \
      --base release \
      --head "$branch" \
      --state merged \
      --json url \
      --limit 1 \
      | jq -r '.[0].url // empty')

    if [ -n "$already_merged_url" ]; then
      echo "  → #$num already backported ($already_merged_url) — skipping."
      continue
    fi

    git checkout -B "$branch" origin/release --quiet

    local cherry_pick_ok=false
    if git cherry-pick "$sha"; then
      cherry_pick_ok=true
    else
      echo "    CONFLICT — invoking AI to resolve..."
      PATH="$REPO_ROOT/hack:$PATH" amake run resolve-conflict \
        -f "$REPO_ROOT/Amakefile" \
        --var repo="$repo" \
        --var sha="$sha" \
        --var num="$num" \
        --var title="$title" \
        --var branch="$branch" || true

      if [ ! -f ".git/CHERRY_PICK_HEAD" ]; then
        echo "    Conflict resolved by AI."
        cherry_pick_ok=true
      else
        echo "    AI could not resolve the conflict. Resolve #$num manually then push '$branch' and open the PR."
        git cherry-pick --abort 2>/dev/null || true
        echo "$repo #$num ($branch)" >> "$FAILED_FILE"
      fi
    fi

    if [ "$cherry_pick_ok" = true ]; then
      git push --force-with-lease origin "$branch" --quiet

      local existing_url pr_url
      existing_url=$(gh pr list \
        --repo "$repo" \
        --base release \
        --head "$branch" \
        --state open \
        --json url \
        --limit 1 \
        | jq -r '.[0].url // empty')

      if [ -n "$existing_url" ]; then
        echo "    PR already open — $existing_url"
        pr_url="$existing_url"
      else
        pr_url=$(gh pr create \
          --repo "$repo" \
          --title "$title" \
          --body "Backport of #$num for the $next patch release." \
          --base release \
          --head "$branch" \
          --label "backport-as-hotfix")
        echo "    Backport PR created — $pr_url"
      fi
      echo "$pr_url" >> "$REPORT_FILE"
      backport_pr_numbers+=("${pr_url##*/}")
    fi

    git checkout --detach origin/release --quiet

  done 3< <(echo "$prs" | jq -c '.[]')

  # ── Version bump branch ──────────────────────────────────────────────────────

  echo ""
  echo "  Creating branch version-$next..."
  git checkout -B "version-$next" origin/release --quiet
  printf '%s\n' "$next" > VERSION
  git add VERSION
  git commit -m "Bump version to $next" --quiet
  git push --force-with-lease origin "version-$next" --quiet
  echo "  Pushed 'version-$next'."

  local version_bump_body=""
  for n in "${backport_pr_numbers[@]}"; do
    version_bump_body+="Depends on #$n"$'\n'
  done

  local existing_version_url version_pr_url
  existing_version_url=$(gh pr list \
    --repo "$repo" \
    --base release \
    --head "version-$next" \
    --state open \
    --json url \
    --limit 1 \
    | jq -r '.[0].url // empty')

  if [ -n "$existing_version_url" ]; then
    echo "  Version bump PR already open — $existing_version_url"
    gh pr edit "$existing_version_url" --body "$version_bump_body" 2>/dev/null
    version_pr_url="$existing_version_url"
  else
    version_pr_url=$(gh pr create \
      --repo "$repo" \
      --title "Trigger release $next" \
      --body "$version_bump_body" \
      --base release \
      --head "version-$next" \
      --label "skip-release-notes")
    echo "  Version bump PR created — $version_pr_url"
  fi
  echo "$version_pr_url" >> "$REPORT_FILE"

  cd - > /dev/null
}

# ── Run ───────────────────────────────────────────────────────────────────────

for repo in "${REPOS[@]}"; do
  ( process_repo "$repo" ) || echo "⚠ Error processing $repo — skipping."
done

echo ""
echo "════════════════════════════════════════"
echo " Summary"
echo "════════════════════════════════════════"

mapfile -t REPORT < "$REPORT_FILE"
mapfile -t FAILED < "$FAILED_FILE"

if [ ${#REPORT[@]} -gt 0 ]; then
  echo "PRs:"
  for url in "${REPORT[@]}"; do
    echo "  $url"
  done
fi

if [ ${#FAILED[@]} -gt 0 ]; then
  echo ""
  echo "Cherry-pick conflicts (resolve manually):"
  for item in "${FAILED[@]}"; do
    echo "  - $item"
  done
fi

echo ""
echo "Next step for each repo:"
echo "  1. Wait for the backport PRs to pass CI and merge them."
echo "  2. Merge the 'Trigger release <version>' PR to kick off CI."
