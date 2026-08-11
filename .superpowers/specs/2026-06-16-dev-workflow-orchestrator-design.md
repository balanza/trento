# Dev workflow orchestrator — design

A durable-execution orchestrator on the developer's laptop that turns
multi-step development tasks (publish-to-OBS, poll, on-failure-fix, cleanup;
later: bring-up the k3d stack, cut a patch release; etc.) into pure-code
workflows backed by [Restate](https://restate.dev). Workflows compose three
kinds of activities — shell scripts, agent calls (`claude -p`), and
human-in-the-loop pauses — and survive shell exits, laptop reboots, and
handler-process restarts.

`make test-on-obs` is v1's only end-to-end workflow. It supersedes the
`hack/obs-test.sh publish + status + cleanup` cycle and adds an iterative
fix-and-retry loop driven by Claude with a per-patch human approval gate.

## Goals

- Provide a single conventional layout (`.workflows/`) and one entry point
  (`hack/workflows.sh`) for any future dev-loop automation that benefits
  from durability, suspend/resume, or HITL approvals.
- Migrate `obs-test.sh` end-to-end into a TypeScript workflow, preserving
  current behavior at parity *plus* adding the fix-iterate loop.
- Keep the runtime self-contained on the developer's laptop: one
  `restate-server` daemon + one Go handler binary, both started by a
  single `make wf-start`.
- Make every workflow re-attachable. Killing the foreground `make` does not
  cancel the workflow; rerunning the same target on the same branch attaches
  to the in-flight run.
- Enforce the repo's tooling-vs-product boundary: workflows can read from
  submodules freely, but any *edit* to a submodule's tree (e.g. patching a
  spec file) requires explicit human approval through the HITL gate.

## Non-goals (v1)

- Multi-user / shared Restate instance. Solo-laptop only.
- Secrets management beyond what the shell already provides
  (`OBS_USER`, `OSC_DISTROBOX`, etc. read from `.env` or environment).
- Cron-like scheduled invocations. The user always types `make X` or
  `hack/workflows.sh run`.
- Porting `start.sh`, `patch-release.sh`, or any other existing script
  beyond `obs-test.sh`. Each becomes its own brainstorm/plan/spec later.
- A bespoke TUI. The Restate web UI on `localhost:9070` plus the
  `hack/workflows.sh` subcommands are the v1 surface.
- Fully autonomous fix-iterate (`fixIterate: 'auto'`). Default and only
  supported mode in v1 is `'review'`; the `'auto'` value is reserved and
  rejected by the input schema.
- Anything inside `web/`, `wanda/`, `agent/`, `mcp-server/`, `checks/`,
  `contracts/`, or `helm-charts/` beyond the spec-file edits the
  HITL-approved fix step performs. No product refactors snuck in via
  workflows.

## Runtime topology

Three processes on the developer's laptop:

1. **`restate-server`** — single binary, persistent (user systemd unit;
   foreground fallback when systemd-user is unavailable). Embedded RocksDB
   at `~/.local/state/trento-workflows/restate`. Web UI on `localhost:9070`.
2. **`trento-workflows` handler service** — long-lived Go binary
   (`go run ./.workflows/cmd/handler` in dev; built into a static binary
   for the systemd unit) hosting workflow + activity code. On boot it
   registers its endpoint with the server over HTTP. Restart policy:
   managed by the same systemd target as the server.
3. **`restate` CLI** — invoked by Makefile targets and by humans
   interactively. Submits invocations, streams journals, lists in-flight
   runs, resolves awakeables.

Invocation path:

```
make test-on-obs
  └─ hack/workflows.sh run test-on-obs '{...input...}'
        └─ restate workflow run trento.test-on-obs --json '{...}'
              └─ HTTP POST to restate-server
                    └─ restate-server invokes the registered handler
                          └─ Go workflow handler runs, calls activities
```

Restart safety: if the laptop reboots mid-workflow, restate-server (under
systemd) comes back up, the handler service restarts, and any active
workflow replays from its last journaled step. No state is lost.

## Repo layout

Everything new lives under `.workflows/` at the repo root. Existing
tooling (`hack/`, `Makefile`, `Amakefile`) gets thin additions, not
restructured.

```
.workflows/
├── go.mod                    # module github.com/trento-project/trento/.workflows
├── go.sum                    # deps: github.com/restatedev/sdk-go,
│                             # github.com/go-playground/validator/v10,
│                             # github.com/stretchr/testify
├── README.md                 # how to start / register / run; conventions doc
├── cmd/
│   └── handler/
│       └── main.go           # boots the handler HTTP service (Restate endpoint)
├── internal/
│   ├── activities/           # reusable, side-effectful units (one Go package each)
│   │   ├── osc/              # OBS operations
│   │   ├── container/        # docker/podman + distrobox shell-out
│   │   ├── git/              # submodule sha/describe/dirty
│   │   ├── claude/           # `claude -p` invocations with prompt + context
│   │   ├── human/            # awakeable-based pauses
│   │   └── fs/               # spec/file read+edit + tar helpers
│   ├── workflows/
│   │   └── testonobs/        # exposed as workflow id "trento.test-on-obs"
│   │       ├── workflow.go   # the workflow function (pure orchestration)
│   │       ├── schema.go     # input/output structs + validate tags
│   │       └── prompts/      # claude prompt templates (.md, versioned)
│   └── lib/                  # logger, exec wrapper, shared types
├── tests/                    # `go test` integration suite; build tag `obs_live`
│                             # gates the live osc calls
├── .env.example              # OBS_USER, OSC_DISTROBOX, ...
└── .runs/                    # per-invocation stdout/stderr from shell-outs;
                              # gitignored; pruned by `workflows.sh prune`

hack/
└── workflows.sh              # NEW: start | stop | status | register |
                              #      run <wf> [json] | ask | resolve | cancel | kill | ui

Makefile                      # NEW targets: wf-start, wf-stop, wf-status,
                              # test-on-obs, test-on-obs-quick
```

Conventions:

- **Activities live in a flat namespace under `activities/`**, grouped by
  surface. Each module exports small functions; activities are registered
  with Restate inside the workflow file that consumes them — not all-at-once
  globally. This keeps the registered surface minimal and makes "what does
  this workflow touch" obvious from one file.
- **Workflows are folders, not files.** `testonobs/workflow.go` is the
  pure-code orchestration; `schema.go` carries the input/output structs
  with `validator` tags; `prompts/` keeps Claude prompts as `.md` files
  (versionable, reviewable). Future workflows get their own folder under
  `internal/workflows/`.
- **`obs-test.sh` stays in place** for the duration of the migration (see
  Migration plan). It is only deleted after the workflow has handled a full
  release-train cycle without divergence.

## Activity inventory for `test-on-obs`

Each row is one function; all are deterministic from inputs + side-effects
on the listed targets. Rows marked **NEW** do not exist in `obs-test.sh`.

| Module | Activity | Replaces / new | Notes |
|---|---|---|---|
| `osc/` | `APIAuthProbe(ctx)` | `osc api /person/$USER` | startup check |
| | `GetPrjMeta(ctx, proj)` / `SetPrjMeta(ctx, proj, xml)` | `osc meta prj` | XML stays as string; mutated in `fs.XMLMutateProjectMeta` |
| | `GetPrjConf(ctx, proj)` / `SetPrjConf(ctx, proj, txt)` | `osc meta prjconf` | |
| | `GetPkgMeta(ctx, proj, pkg)` / `SetPkgMeta(...)` | `osc meta pkg` | |
| | `EnsureSubproject(ctx, proj, fromFactory)` | `ensure_subproject` | composes the above; idempotent |
| | `CheckoutPackage(ctx, proj, pkg, dir)` | `osc co --output-dir` | |
| | `CommitPackage(ctx, dir, msg)` | `osc addremove && osc commit` | |
| | `GetResults(ctx, proj)` → `[]Result` | `osc results --xml` | XML parsed into typed `Result` slice |
| | `RemoteBuildLog(ctx, proj, pkg, repo, arch)` → `string` | **NEW** (`osc remotebuildlog`) | for fix-iterate |
| | `RDelete(ctx, proj)` | `osc rdelete -r` | cleanup |
| `container/` | `Runtime(ctx)` → `Runtime` | `container_runtime` | typed enum: `Docker` \| `Podman` |
| | `RunOneShot(ctx, image, opts RunOpts, script string)` | `run_in_container` | streams log to `.runs/<id>/<activity>.log`; returns exit code + captured stdout |
| | `Preflight(ctx, image)` | `container_preflight` | startup check |
| | `DistroboxRun(ctx, name, argv)` | `osc_proxy` wrapping | osc-in-distrobox is just this |
| `git/` | `Head(ctx, path)`, `Branch(ctx, path)`, `Describe(ctx, path)`, `Dirty(ctx, path)` | `super_sha`, `resolve_branch`, `synth_version` parts | tiny pure shell-out wrappers |
| `fs/` | `TarCreate(ctx, srcDir, destFile, opts TarOpts)` | `build_source_tarball` | `TarOpts` carries `Excludes []string`, `Transform string` |
| | `ReadSpec(ctx, path)` / `WriteSpec(ctx, path, body)` | sed/cp lines | |
| | `SetSpecVersion(body, ver) string` | `sed -i 's\|^Version:.*\|...\|'` | pure string fn |
| | `XMLMutateProjectMeta(xml string, opts ProjectMetaMutation) string` | inline Python in `ensure_subproject` | pure fn using `encoding/xml`; opts: `NewProj`, `KeepUser`, `OldProj` |
| | `ApplyPatch(ctx, submodulePath, diff)` | **NEW** | HITL-approval-gated; rejects edits outside the submodule's tree |
| `claude/` | `Invoke(ctx, req Request) Response` | **NEW** | wraps `claude -p`; `Request` carries `Prompt`, `Files`, `AllowedTools`, `Cwd`; `Response` carries `Text`, `SessionID`; same shape as `amake-claude` |
| `human/` | `Confirm(ctx, req ConfirmReq) string` | **NEW** | Restate awakeable; `ConfirmReq`: `Title`, `Body`, `Choices`; returns the chosen string; resolved via UI or CLI |
| | `Await(ctx, req AwaitReq)` | **NEW** | "tell me when you're ready"; same primitive |

One composite that stays an activity to keep it cheap rather than promoting
to a sub-workflow:

- **`BundleSubmoduleDeps(ctx, sm, staging, version)`** — encapsulates the
  per-submodule deps bundling (`web` = mix + npm; `wanda` = mix + cargo;
  `agent`/`mcp-server` = go vendor; `checks` = noop). Pure dispatch on `sm`
  → one `container.RunOneShot` call with the right script. The bash has
  this as one function (`bundle_deps_in_container`); same shape in Go.

## `test-on-obs` workflow shape

**Input** (validated via `go-playground/validator` struct tags):

```go
type Input struct {
    // default: all five submodules with packaging:
    //   web, wanda, agent, mcp-server, checks
    //   (contracts and helm-charts excluded)
    Submodules       []string   `json:"submodules,omitempty"`
    // 'auto' reserved; rejected in v1
    FixIterate       FixIterate `json:"fixIterate"        validate:"required,oneof=off review"`
    // per failing package; default 3
    MaxAttempts      int        `json:"maxAttempts"       validate:"min=1,max=10"`
    // default true
    CleanupOnSuccess bool       `json:"cleanupOnSuccess"`
    // default false (keep state for inspection)
    CleanupOnFailure bool       `json:"cleanupOnFailure"`
}

type FixIterate string
const (
    FixIterateOff    FixIterate = "off"
    FixIterateReview FixIterate = "review"
)
```

**Output**:

```go
type Output struct {
    Project string         `json:"project"` // e.g. home:balanza:trento.poc-check-predicate
    Results []PackageResult `json:"results"`
}

type PackageResult struct {
    Pkg         string       `json:"pkg"`
    FinalStatus FinalStatus  `json:"finalStatus"` // ok | failed-after-N | skipped | aborted
    Attempts    int          `json:"attempts"`
    LogsRef     string       `json:"logsRef"`     // relative path under .workflows/.runs/<id>/
}
```

**Virtual-object key**: branch name. This means two concurrent
`test-on-obs` runs on the *same* branch are serialized (the second attaches
to or queues behind the first); runs on *different* branches proceed in
parallel.

**Orchestration**:

```
1. preflight: osc.APIAuthProbe + container.Preflight
2. resolve: branch, project = home:balanza:trento.<branch>, super_sha
3. osc.EnsureSubproject(project, fromFactory="devel:sap:trento:factory")  // idempotent
4. for each requested submodule, bounded parallelism N=3:
     a. compute version (git.Describe + git.Dirty + super_sha)
     b. fs.TarCreate(staging source tarball)
     c. BundleSubmoduleDeps(sm, staging, version)
     d. ensure package exists (osc.SetPkgMeta if missing)
     e. osc.CheckoutPackage → copy staging files → osc.CommitPackage
     f. if image package present: same for the -image package
5. poll loop: every 30s call osc.GetResults(proj); break when no PEND remains;
   hard timeout 45min (configurable via env)
6. partition results: { ok, fail, other }
7. if len(fail) == 0: success path → conditional cleanup → return
8. else fix-iterate loop (see below) → conditional cleanup → return
```

**Fix-iterate loop** (the part absent from `obs-test.sh` today). Failed
packages are processed **sequentially** (one at a time across the fail
set), so HITL prompts queue in a predictable order rather than the human
juggling several diff reviews at once:

```
for each failed pkg, until ok OR attempts == maxAttempts:
  log   := osc.RemoteBuildLog(ctx, proj, pkg, repo, arch)
  spec  := fs.ReadSpec(ctx, specPathFor(pkg))            // in upstream submodule
  patch := claude.Invoke(ctx, claude.Request{
    Prompt:       embeddedPrompt("fix-build-failure.md"),
    Files:        []string{spec, logExcerpt},
    AllowedTools: []string{"Read"},                      // NO Edit; Claude proposes
    Cwd:          submodulePathFor(pkg),
  })
  // fixIterate is always 'review' in v1:
  choice := human.Confirm(ctx, human.ConfirmReq{
    Title:   fmt.Sprintf("%s attempt %d/%d: review proposed patch", pkg, n, max),
    Body:    rendered diff + claude diagnosis + last 40 log lines,
    Choices: []string{"apply", "skip-package", "abort-workflow"},
  })
  switch choice {
  case "apply":
    fs.ApplyPatch(ctx, submodulePathFor(pkg), patch)
    // re-run steps 4d–4e for THIS pkg only, then poll only this pkg until terminal
  case "skip-package":
    record finalStatus = "skipped"; break inner loop
  case "abort-workflow":
    return error — workflow ends; cleanup decision still honored
  }
  attempts++
```

**Spec file locations** (same as `obs-test.sh`):

- `<sm>/packaging/suse/rpm/<pkg>.spec`, or
- `<sm>/packaging/suse/<pkg>.spec` (fallback).

**Failure semantics**:

- Activity-level failures use the Restate retry policy (fast exponential
  with cap; default for shell-out activities is 3 attempts then surface).
- Workflow-level failure on a package (e.g. all `maxAttempts` exhausted)
  does NOT crash the workflow; it records `finalStatus: 'failed-after-N'`
  and continues processing the rest, then cleanup.
- The only path that aborts the whole workflow is the human picking
  `abort-workflow` in the HITL gate.

## HITL mechanism

Built on Restate **awakeables**. The workflow calls
`restate.Awakeable[T](ctx)`, gets back an `(id, promise)` pair, persists
`id` in metadata the human can see, then blocks on the promise. The
workflow is suspended (zero CPU, durable) until someone calls
`restate.ResolveAwakeable(id, value)`. On resolve, replay continues from
that line with the value bound. Crash-safe both sides.

**Two surfaces for resolving an awakeable** (workflow indifferent):

1. **Restate Web UI** (`localhost:9070`) — every suspended awakeable lists
   with its `title`/`body` metadata. Click a button → POST to the resolve
   endpoint. Good for the patch-review use case with a rendered diff.
2. **CLI** — `hack/workflows.sh ask` lists pending awakeables for the
   current branch; `hack/workflows.sh resolve <id> --choice apply` resolves
   one. Good for terminal-native work.

**Two activity shapes built on top:**

- `human.Confirm(ctx, ConfirmReq{Title, Body, Choices []string}) string`
  — multi-choice prompt. Used in fix-iterate's
  `apply | skip-package | abort-workflow`.
- `human.Await(ctx, AwaitReq{Title, Body})` — blocking ack. Reserved for
  future workflows; not used in `test-on-obs`.

**What the human sees** for the fix-iterate review case:

```
Title: trento-wanda attempt 2/3: review proposed patch
Body:
  Failure: rpmbuild error in %install — missing BuildRequires: cargo
  Build log (last 40 lines): <...>
  Claude's diagnosis: <one paragraph>
  Proposed patch:
    --- a/wanda/packaging/suse/rpm/trento-wanda.spec
    +++ b/wanda/packaging/suse/rpm/trento-wanda.spec
    @@ -12,6 +12,7 @@
     BuildRequires: ...
    +BuildRequires: cargo
  Choices: apply | skip-package | abort-workflow
```

The patch is rendered *but not yet applied* on the filesystem. The
workflow only invokes `fs.applyPatch(...)` after the human picks `apply`.
This is what enforces the CLAUDE.md tooling-vs-product rule: the workflow
cannot sneak edits into a submodule without an explicit click.

**Abandoned-awakeable reaping**: a soft timeout (default 24h, configurable
per call) resolves the awakeable with `{ choice: 'timeout' }` so workflows
do not dangle forever. The fix-iterate loop treats `timeout` as
`skip-package`.

**Operational gotcha**: if the handler service is rebuilt while an
awakeable is pending, the awakeable survives (it is server-side state); the
workflow continues on the new handler version when resolved. The
deterministic-replay constraint still applies — workflow code may not be
edited in a way that changes the awakeable order before its resolution
lands. The README codifies this as "don't edit pending workflows".

## Make integration and day-to-day UX

**`hack/workflows.sh` subcommands** (one entry point shared by Make and
humans):

| Subcommand | Behavior |
|---|---|
| `start` | Boots `restate-server` (user systemd unit; foreground fallback) and the Go handler (built binary under the systemd unit; `go run ./.workflows/cmd/handler` in dev). Waits until the service registers before returning. Idempotent. |
| `stop` | Stops both. |
| `status` | Prints: server reachable? handler registered? pending workflows for current branch? |
| `register` | Re-registers the handler service after a code change. Auto-called by `start`. |
| `run <wf> [--json ...]` | Submits a workflow invocation, streams progress + activity logs, exits with the workflow's exit code. Blocks. |
| `ask` | Lists awakeables pending for the current branch. |
| `resolve <id> [--choice X]` | Resolves an awakeable. With no `--choice`, opens `$EDITOR` on the body. |
| `cancel <invocation>` | Cancels a running workflow (workflow gets a chance to clean up). |
| `kill <invocation>` | Force-kills, no cleanup. Last resort. |
| `ui` | Opens `localhost:9070` in `$BROWSER`. |
| `prune` | Drops completed-and-older-than-N-days runs from `.workflows/.runs/`. |

**Makefile additions** (all delegate to the script):

```make
## --- workflows ---

wf-start:           ## Start restate-server + handler service
	@hack/workflows.sh start

wf-stop:            ## Stop both
	@hack/workflows.sh stop

wf-status:          ## Where are we
	@hack/workflows.sh status

test-on-obs:        ## Publish current branch state to OBS, poll, fix-iterate, cleanup
	@hack/workflows.sh run test-on-obs --json '{"fixIterate":"review","maxAttempts":3}'

test-on-obs-quick:  ## Same, no fix-iterate (just publish + poll + report)
	@hack/workflows.sh run test-on-obs --json '{"fixIterate":"off"}'
```

**Typical session** (happy + interrupted):

```
$ make wf-start                       # one-time, or on demand
$ make test-on-obs
==> Workflow trento.test-on-obs/abc started (branch: poc-check-predicate)
==> preflight ok | subproject ensured
==> publishing 5 submodules ...
==> poll attempt 12/90: 4 ok, 1 fail (trento-wanda)
==> fix-iterate: trento-wanda attempt 1/3
==> SUSPENDED — awakeable awk_7f3c pending: "trento-wanda: review proposed patch"
==> resolve with: hack/workflows.sh resolve awk_7f3c
                  or open http://localhost:9070/invocations/abc

# (in another terminal, or after lunch)
$ hack/workflows.sh ask
awk_7f3c  trento-wanda attempt 1/3: review proposed patch  (pending 4m)
$ hack/workflows.sh resolve awk_7f3c --choice apply
==> resumed
==> republishing trento-wanda
==> poll ... ok
==> all 5 packages green. cleanup? (cleanupOnSuccess=true)
==> workflow trento.test-on-obs/abc completed in 18m22s
```

**Ctrl-C `make test-on-obs`**: the CLI streamer disconnects; the workflow
keeps running on the server. Re-invoking `make test-on-obs` on the same
branch attaches to the existing run (virtual-object keyed by branch) and
resumes streaming. To actually abort: `hack/workflows.sh cancel <id>`.

**Where logs live**:

- Per-invocation journal: in `restate-server` (UI shows it).
- Per-activity stdout/stderr for long shell-outs (container runs, osc):
  `.workflows/.runs/<invocation_id>/<activity>.log`. Kept until the next
  `workflows.sh prune`. The Restate journal records a *reference* to these
  files, not their bytes, so the journal stays small.

## Migration plan

`obs-test.sh` stays for the whole transition. Six steps:

1. **Land the framework with a no-op workflow.** `.workflows/` scaffold,
   `hack/workflows.sh`, `wf-*` Make targets, one `hello` workflow that
   calls `git.head()` and returns. Commit. Verifies infra without touching
   OBS.
2. **Port activities bottom-up, with unit tests.** Start with the cheapest,
   side-effect-free ones (`git.*`, `fs.setSpecVersion`,
   `fs.xmlMutateProjectMeta`). Then shell-out wrappers (`container.*`,
   `distroboxRun`). Then `osc.*` against a throwaway personal subproject.
   `go test` + `testify` with fixture XMLs for parser code; an integration
   suite gated by Go build tag `obs_live` (`go test -tags obs_live ./...`)
   for live osc calls.
3. **Compose `test-on-obs` workflow with `fixIterate: 'off'`.** Publish +
   poll + report + cleanup, no AI loop. At this point both `obs-test.sh
   publish/status/cleanup` and `make test-on-obs` work; run them in
   parallel on real branches for a week and diff their outputs.
4. **Add the fix-iterate loop in `'review'` mode.** No `'auto'`.
5. **Cut over `make test-on-obs`** as the recommended path. `obs-test.sh`
   gets a deprecation banner pointing at the workflow.
6. **Delete `obs-test.sh`** only after the workflow has handled a full
   release-train cycle without divergence.

## Risk register

- **Reimplementing ~500 lines of subtle bash.** Biggest risk. Mitigation:
  `obs-test.sh` runnable side-by-side; parallel runs at step 3 before
  cutover; table-driven tests for the XML manipulation (the inline Python
  in `ensure_subproject`).
- **`distrobox enter --root -- osc` quirks** (TTY, exit codes, env
  propagation). Mitigation: `container.distroboxRun` is one tiny activity;
  smoke-tested first thing in step 2.
- **Restate replay determinism.** Every non-deterministic side-effect must
  be inside an activity — no `time.Now()`, `rand.Intn`, `os.Getenv`, or
  goroutine-based concurrency in workflow code. The Go SDK provides
  `restate.Run(ctx, "label", fn)` and `restate.After(ctx, d)`. Easy to
  follow, real footgun if a contributor adds a bare `time.Now()`.
  Mitigation: a custom `go vet` analyzer (or rigorous code review until
  one exists) + a one-page conventions doc in `.workflows/README.md`.
- **Submodule edits from a workflow.** Covered by the `'review'` default
  (HITL section). Stated in `.workflows/README.md` so it is not buried in
  design history.
- **State growth on disk.** `~/.local/state/trento-workflows/restate` and
  `.workflows/.runs/` both grow with use. Mitigation: `workflows.sh prune`
  drops completed-and-older-than-N-days runs. Not v1 critical.

## Open questions deferred to implementation

- Exact `restate-server` version pinning + how to keep it in sync across
  the team if/when this leaves single-laptop scope. v1 pins the latest
  stable in `.workflows/README.md`; revisit if Restate ships a breaking
  storage change.
- Whether `bundleSubmoduleDeps` should later split into per-language
  activities. Left as one composite for v1; revisit if a second workflow
  wants `bundleGoDeps(sm)` independently.
- Polling cadence and timeout knobs (currently 30s / 45min hard cap).
  Defaults set in code; exposed as workflow input only if a real need
  appears.
