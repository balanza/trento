You are inside a git repository at the PR's head branch, mid-rebase. A `git rebase {{ontoRef}}` produced merge conflicts that block the workflow from pushing the latest fixup commit.

# Context

- **Working dir**: your `cwd`
- **Onto ref**: `{{ontoRef}}` (this is what we're rebasing onto)
- **State**: the rebase is paused at the first conflicting commit; `git status` will list the conflicted files

# Task

1. Run `git status` to see the conflicted files.
2. For each conflicted file:
   - Read it; understand both sides of the conflict (`<<<<<<<` / `=======` / `>>>>>>>` markers).
   - Resolve the conflict. Default heuristic: prefer the INCOMING (upstream) changes when the conflict is purely structural (whitespace, imports, generated code) and prefer OUR local changes when the conflict involves the bugfix the previous fixup commits were trying to introduce. When in doubt, combine semantically.
   - Stage the resolved file with `git add <path>`.
3. Once every conflicted file is staged, complete the rebase with `git rebase --continue`. If git asks for editor input, set `GIT_EDITOR=:` in front of the command (`GIT_EDITOR=: git rebase --continue`) so it accepts the default message and exits.
4. Verify the rebase finished by running `git status` again — it should report "nothing to commit, working tree clean" and NOT mention any in-progress rebase.

# Rules

1. Do NOT run `git push` or `git commit` directly — only `git add` and `git rebase --continue`. The workflow handles the push.
2. Keep edits MINIMAL: only resolve conflict markers; do not refactor surrounding code, fix unrelated lint, or expand scope.
3. Do NOT touch files outside the working dir.
4. If you cannot resolve the conflicts (e.g. the upstream changes contradict the bugfix the fixup commits made, requiring product knowledge to reconcile), run `git rebase --abort` and exit. The workflow detects the abort and bails out gracefully.

# Output

When done, exit. The workflow does not parse your stdout. It inspects `.git/rebase-merge` and `.git/rebase-apply` to decide whether you succeeded.
