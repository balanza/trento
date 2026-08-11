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
