You are inside the git repository for {{repo}}.

A cherry-pick of commit {{sha}} (PR #{{num}}: '{{title}}') onto branch
'{{branch}}' has produced conflicts.

Please:

1. Run `git status` to identify the conflicted files.
2. Examine each conflict and resolve it, favouring the incoming changes
   unless they semantically clash with release-branch-specific code.
3. Stage the resolved files with `git add`.
4. Complete the cherry-pick with `git cherry-pick --continue --no-edit`.

Do NOT run `git push`.
