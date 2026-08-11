# OBS local-test publisher — design

A single bash script, `hack/obs-test.sh`, that publishes the current Trento
super-repo's submodule contents (including uncommitted changes) to a personal
OBS subproject so the developer can verify their local code builds end-to-end
through OBS without going through the regular release flow.

## Goals

- Publish the working-tree state of every (or a filtered subset of) submodule
  to a personal OBS subproject under `home:balanza:`, with the same build
  targets as the official rolling project `devel:sap:trento:factory`.
- Provide a stable, branch-keyed subproject that can be re-published many times
  on the same branch — each publish creating a new RPM version that OBS rebuilds.
- Report aggregated per-package build status.
- Provide a one-shot cleanup that removes the subproject.

## Non-goals

- Submitting changes upstream — this is a personal test sandbox.
- Packaging anything that is NOT a Trento submodule (factory packages like
  `ansible-trento`, `supportutils-plugin-trento`, `trento-server-helm` are out
  of scope; they have no `/packaging` folder in any submodule).
- Working with the helm-charts submodule (no `/packaging` folder).
- Reproducing OBS source services exactly — we sidestep them by shipping
  static tarballs.

## CLI

```
hack/obs-test.sh publish [-f|--filter <list>]
hack/obs-test.sh status  [-f|--filter <list>]
hack/obs-test.sh cleanup [-y|--yes]
```

`<list>` is a comma-separated set of submodule names from the in-scope set
(`web`, `wanda`, `agent`, `mcp-server`, `checks`). Filter operates on
submodules — each submodule expands to all its packages.

## Scope: submodule → packages map

Hard-coded in the script:

| Submodule    | RPM package          | Image package              |
|--------------|----------------------|----------------------------|
| `web`        | `trento-web`         | `trento-web-image`         |
| `wanda`      | `trento-wanda`       | `trento-wanda-image`       |
| `agent`      | `trento-agent`       | *(none — agent has no image package)* |
| `mcp-server` | `mcp-server-trento`  | `mcp-server-trento-image`  |
| `checks`     | `trento-checks`      | `trento-checks-image`      |

Total: 9 packages across 5 submodules.

## Subproject naming and version synthesis

- **Subproject:** `home:balanza:trento.<branch>` where `<branch>` is the
  super-repo branch from `git symbolic-ref --short HEAD`, sanitized by
  replacing every character outside `[a-zA-Z0-9._-]` with `_`. On a detached
  HEAD, fall back to short SHA (`trento.<short-sha>`) with a printed warning.
- **Per-package version (set in the spec's `Version:` field):**
  `<submodule-describe>+local.<super-short-sha>[.dirty]`
  - `<submodule-describe>` = `git -C <submodule> describe --tags --always`
  - `<super-short-sha>` = `git rev-parse --short HEAD` in the super-repo
  - `.dirty` suffix added when `git -C <submodule> status --porcelain` is
    non-empty
  - Normalize for RPM: replace `-` with `.` (`Version:` does not accept `-`).
    Example: `git describe` output `3.1.0-30-g048633810` becomes
    `3.1.0.30.g048633810+local.bc6d725.dirty`.

The subproject is stable across commits on a branch (so re-publishes are
incremental); the package version churns every commit (so OBS rebuilds).

## Approach: local tarball assembly + `osc commit`

We do NOT use OBS source services. Instead, the script builds every tarball
the spec file expects, on the local filesystem (with dependency bundling
inside the CI container), then commits them as static sources via
`osc commit`. Rationale:

1. The user explicitly wants uncommitted working-tree changes included.
   `tar_scm` is git-based and cannot capture that.
2. Avoids any remote git push (no fork required, no SSH plumbing).
3. We only need to reimplement the output of five source services
   (`elixir_mix_deps`, `node_modules`, `go_modules`, `cargo_vendor`,
   `regex_replace` for `%%GTM_ID%%`).

## Subproject lifecycle

### Creation (idempotent on every `publish`)

If the subproject does not exist, create it by deriving its meta from
`devel:sap:trento:factory`:

1. `osc meta prj devel:sap:trento:factory > /tmp/meta.xml`
2. Rewrite in place (using `python3 -c` with `xml.etree`):
   - `<project name="...">` attribute → `home:balanza:trento.<branch>`
   - Drop all `<person userid="...">` entries except `balanza`
   - Rewrite **every** `<path project="devel:sap:trento:factory" .../>` to
     `<path project="home:balanza:trento.<branch>" .../>` so the `containers`
     and `charts` repositories pull from THIS subproject, not from factory.
   - Keep the `<build><disable repository="SLES15-SP5"/></build>` block.
3. `osc meta prj home:balanza:trento.<branch> -F /tmp/meta.xml`
4. Copy project config verbatim: `osc meta prjconf devel:sap:trento:factory |
   osc meta prjconf home:balanza:trento.<branch> -F -`

If the subproject already exists, this step is skipped (we don't re-apply
meta — a developer who manually edited their subproject's meta keeps their
edits).

### Per-package publish

Per package, in a clean staging directory under
`<super-repo>/.obs-test/work/<package>/`:

1. **Source tarball** (`<package>-<version>.tar.gz`):
   - `tar -czf … -C <submodule> .` with these excludes (mirroring each
     submodule's existing `_service` `<exclude>` rules plus build outputs):
     `.git`, `.github`, `.obs`, `assets/node_modules`, `deps`, `_build`,
     `vendor`, `target`, `cover`.
   - Use `--transform "s|^|<package>-<version>/|"` so the tarball unpacks
     into a single top-level directory (matching `%setup -q` expectations).

2. **Dependency tarballs (inside `ghcr.io/trento-project/continuous-delivery`):**

   The container is invoked with the staging directory bind-mounted at
   `/work`. The script `cd`s into the extracted source tree inside the
   container and runs:

   | Package family              | Commands                                                                                                  | Output(s)                                          |
   |-----------------------------|-----------------------------------------------------------------------------------------------------------|----------------------------------------------------|
   | `trento-web`                | `mix local.hex --force && mix local.rebar --force && mix deps.get` → `tar -czf deps.tar.gz deps`; then in `assets/`: `npm ci --ignore-scripts` → cpio of `node_modules` matching the `node_modules` source service: `find … \| cpio -o -H newc > node_modules.obscpio`. Generate `node_modules.spec.inc` with the `Source10000…` lines the spec includes. | `deps.tar.gz`, `node_modules.obscpio`, `node_modules.spec.inc` |
   | `trento-wanda`              | `mix deps.get` → `deps.tar.gz`. Then `cd deps/rhai_rustler/native/rhai_rustler && cargo vendor` → `vendor-rhai_rustler.tar.gz`. | `deps.tar.gz`, `vendor-rhai_rustler.tar.gz`       |
   | `trento-agent`, `mcp-server-trento` | `go mod vendor` → `vendor.tar.gz`                                                                  | `vendor.tar.gz`                                    |
   | `trento-checks`             | (nothing — no deps)                                                                                       | —                                                  |

3. **Spec file:**
   - Copy `<submodule>/packaging/suse/rpm/<spec>` (or
     `<submodule>/packaging/suse/<spec>` for the agent, which has no `rpm/`
     subdir) into the staging dir.
   - `sed -i 's|^Version: .*|Version:        <version>|'`
   - For `trento-web` only: also `sed -i 's|%%GTM_ID%%|GTM-N3JHF5M6|g'`
     (mirrors the `regex_replace` source service in factory).

4. **Auxiliary spec sources:** None. Every spec references its systemd
   units (and agent config files) via paths inside the source tarball
   (e.g. `packaging/suse/rpm/systemd/trento-web.service`), not via
   `Source:` declarations, so they ride along automatically.

5. **OBS commit:**
   ```
   osc co home:balanza:trento.<branch> <package> -o .obs-test/co/<package>
   rm -f .obs-test/co/<package>/*.tar.gz .obs-test/co/<package>/*.obscpio \
         .obs-test/co/<package>/_service
   cp .obs-test/work/<package>/* .obs-test/co/<package>/
   (cd .obs-test/co/<package> && osc addremove && \
    osc commit -m "local test <super-short-sha>")
   ```
   If the package doesn't exist yet (first publish), `osc co` will fail;
   handle that by creating it first with `osc mkpac home:balanza:trento.<branch> <package>`.
   No `_service` file is written — we ship static tarballs.

### Image-package publish

Image packages (`trento-web-image`, `trento-wanda-image`,
`trento-checks-image`, `mcp-server-trento-image`) are simpler — they bundle
a Dockerfile that `zypper install`s the corresponding RPM, and rely on
**buildtime** OBS services for label/version substitution.

1. Copy `<submodule>/packaging/suse/container/Dockerfile` and
   `<submodule>/packaging/suse/container/README.md` into staging.
2. Write a minimal `_service` file containing **only** the buildtime
   services from factory:
   - `docker_label_helper` (mode=buildtime)
   - `kiwi_metainfo_helper` (mode=buildtime)
   - `replace_using_package_version` for `%%VERSION%%` (mode=buildtime)
   - For `trento-web-image` only: also for `%%VERSION_SHORT%%` (parse-version=patch)
   - All `replace_using_package_version` rules end with the
     `\+git\.` → `-git.` rewrite to keep Docker-tag-legal version strings.
3. No tarballs, no `tar_scm`. The Dockerfile RUNs `zypper -n install --no-recommends <rpm-package>`; OBS resolves `<rpm-package>` against this subproject via the `containers` repo path we rewrote earlier.
4. `osc commit` as for RPM packages.

### Concurrency

Submodules are processed serially, and within a submodule the RPM package
publishes before its image package. Both choices are for log readability
only — OBS schedules and dependency-resolves on its own once everything is
committed.

## `status` command

1. Resolve `<branch>`; verify subproject exists (else exit 1 with message).
2. `osc results --xml home:balanza:trento.<branch>` (one call covers all
   packages, all repos, all arches).
3. Parse XML with `python3 -c` (xml.etree). For each package, aggregate
   per-result codes:
   - `succeeded` → SUCCEEDED
   - `failed`, `unresolvable`, `broken` → FAILED
   - `scheduled`, `building`, `blocked`, `finished`, `signing`, `dispatching` → PENDING
   - anything else → OTHER (rare, e.g. `disabled` — surface for visibility)
4. Print a fixed-width table:
   ```
   PACKAGE                    SUCCEEDED  FAILED  PENDING  OTHER  TOTAL
   trento-web                         5       1        1      0      7
   trento-web-image                   1       0        0      0      1
   ...
   ```
5. Exit code:
   - 0 if FAILED == 0 and PENDING == 0 and OTHER == 0 (all green)
   - 1 if FAILED > 0
   - 2 otherwise (still working)

`--filter` restricts the table rows shown but does not change the underlying
`osc results` call (the rest is filtered client-side).

## `cleanup` command

1. Resolve `<branch>` and the subproject name.
2. Prompt for confirmation unless `-y/--yes` is given:
   `About to delete OBS project home:balanza:trento.<branch>. Continue? [y/N]`
3. `osc rdelete -r -m "cleanup local test" home:balanza:trento.<branch>`
4. Remove the local `.obs-test/` directory under the super-repo.

No filter on cleanup — it's all-or-nothing per subproject.

## Script structure

- **Language:** bash 5+, `set -euo pipefail`.
- **Location:** `hack/obs-test.sh`.
- **Runtime deps:** `git`, `osc` (configured against `api.opensuse.org` as
  user `balanza`), `docker` or `podman` (auto-detect, prefer `docker`),
  `tar`, `python3`.
- **Container image:** `ghcr.io/trento-project/continuous-delivery:latest`
  (no version pin initially; we can pin later if reproducibility bites).
- **Function layout:**
  ```
  main                         # arg parse and dispatch
  cmd_publish / cmd_status / cmd_cleanup
  resolve_subproject_name
  ensure_subproject            # publish path only
  derive_meta_from_factory     # invokes python3 inline for XML rewrite
  publish_submodule            # dispatch to per-submodule function
  publish_web / publish_wanda / publish_agent / publish_mcp_server / publish_checks
  publish_image_package        # shared helper for *_image packages
  run_in_container             # docker/podman run wrapper
  synth_version
  patch_spec_version
  osc_commit_package
  parse_status_xml             # invokes python3 inline
  ```

## Implementation note: invoke the same service binaries

Rather than reimplement what `elixir_mix_deps`, `node_modules`, `go_modules`,
`cargo_vendor` produce, we invoke the same binaries OBS itself runs:
`/usr/lib/obs/service/elixir_mix_deps`, `node_modules`, `go_modules`,
`cargo_vendor`. They live in the `obs-service-*` packages shipped by the
`ghcr.io/trento-project/continuous-delivery` image. Each takes an
`--outdir` argument and writes its tarballs/files there. This collapses
"we reimplement source services" to "we call the binaries source services
call," preserving byte-for-byte parity with factory output (including the
`node_modules.spec.inc` generation with `source-offset=10000`).

## Known limitations

- First run pulls the continuous-delivery container image — ~1-2 GB.
- `osc` and its SSH config are assumed already in place. The script verifies
  `osc whoami` returns `balanza` before doing anything destructive.
- Subproject meta is derived from factory only on creation. If you manually
  edit your subproject's meta later, subsequent `publish` runs won't touch
  it (this is intentional).
