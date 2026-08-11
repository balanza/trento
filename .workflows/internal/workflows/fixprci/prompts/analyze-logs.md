You are diagnosing CI failures on pull request #{{prNumber}} of {{repo}} at head SHA {{headSHA}}.

The block below contains the full logs of every failed job, separated by header lines of the form:

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
- `hint` — ≤500 chars. For `bug` issues: actionable hint for the fix — file path/line if visible, suspected root cause, suggested edit. For `flaky` / `infra` / `unfixable`: empty string is fine.

# Output rules

Output the JSON array wrapped in `<analysis>...</analysis>` tags. No prose outside the tags.

Example:

<analysis>[
  {"issueKey":"bug:web:undefined-var:user_controller.ex:34","category":"bug","summary":"UserController references undefined `current_user_id`","jobRefs":["run/8123/job/45100"],"hint":"In lib/web/controllers/user_controller.ex:34, the line uses `current_user_id` but the assigns key is `user_id`. Rename to `user_id`."},
  {"issueKey":"flaky:agent:test_TestRegisterHostRetries","category":"flaky","summary":"TestRegisterHostRetries fails intermittently on timeout","jobRefs":["run/8123/job/45101"],"hint":""}
]</analysis>

If no failures can be diagnosed from the logs (empty, garbled, or unreadable), output:

<analysis>[]</analysis>
