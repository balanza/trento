The OBS build for the named package failed. You have:

- The build log (last 80 lines).
- The current RPM spec file body.

Propose a minimal unified diff against the spec that addresses the
failure. Do not apply any changes; return only the patch text inside a
fenced ```diff block.

Constraints:

- Only modify the spec file.
- Keep the change minimal — add a single `BuildRequires`, fix one path,
  bump one version, etc. Avoid drive-by edits.
- If the failure root cause is not in the spec (e.g., it's in upstream
  source code), say so explicitly in one sentence and emit no patch.
