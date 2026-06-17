# trento-workflows

Durable-execution orchestrator for Trento dev-loop tasks. Built on
[Restate](https://restate.dev). See the design spec at
`docs/superpowers/specs/2026-06-16-dev-workflow-orchestrator-design.md`
for the full rationale.

## Scaffold status

This is the v1 scaffold. Every activity is a no-op stub (returns zero
values). The orchestration is wired end-to-end so the workflow runs
through cleanly with empty data. Activity bodies arrive package-by-package
per the migration plan.

## Layout

```
.workflows/
├── cmd/handler/         # boots the Restate handler HTTP service
├── internal/
│   ├── activities/      # one Go package per surface (osc, container, git, ...)
│   ├── workflows/       # one folder per workflow
│   └── lib/             # logger, exec wrapper, shared types
└── tests/               # `go test` integration suite; build tag `obs_live`
                         # gates live osc calls
```

## Conventions for workflow code

Restate replays workflow code from the journal. **Anything
non-deterministic must live inside an activity** (a closure passed to
`restate.Run` / `restate.RunVoid`). In particular, in `workflow.go`:

- No `time.Now()` — use `restate.After(ctx, d)` for timers.
- No `rand.Intn`, `os.Getenv`, or anything reading filesystem/network.
- No goroutines — concurrency goes through `restate.RunAsync` + `WaitFirst`.
- Loops and conditionals are fine; they replay deterministically.

A future custom `go vet` analyzer will enforce this. Until then: code
review.

## Running locally

Not yet — `hack/workflows.sh` and the Makefile integration land in the
next slice. For now, manual:

```
cd .workflows
go run ./cmd/handler &      # starts handler on :9080
# in another terminal:
restate-server &
restate deployments register http://localhost:9080
restate workflow run trento.test-on-obs --json '{"fixIterate":"off","maxAttempts":3}'
```
