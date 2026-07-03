package dependabotsweep

import (
	"fmt"

	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/workflows/common/release"
)

// WorkflowName is the Restate service identifier.
const WorkflowName = "trento.dependabot-sweep"

// Register returns the Restate workflow definition. The handler
// binary binds this into its endpoint.
func Register() restate.ServiceDefinition {
	return restate.NewWorkflow(WorkflowName).
		Handler("Run", restate.NewWorkflowHandler(Run))
}

// Run is the workflow entrypoint. No virtual-object key on this
// outer workflow — only the per-repo service (RepoService) is keyed,
// matching the patch-release pattern. The outer invocation is plain:
//
//  1. Apply defaults to in.
//  2. Resolve the target repos (filtered by in.Submodules).
//  3. Fan out one Process call per repo to RepoService.
//  4. Await all futures, aggregate into Output.
//
// Per-repo failures are captured in RepoReport.Errors; per-PR failures
// are captured in PROutcome.State/Note. The top-level workflow only
// returns an error for input-validation or repo-resolution failures
// (which prevent the sweep from running at all).
func Run(ctx restate.WorkflowContext, in Input) (Output, error) {
	in = applyDefaults(in)

	repoRoot, err := release.ResolveRepoRoot(ctx)
	if err != nil {
		return Output{}, err
	}

	repos, err := release.ResolveRepos(ctx, repoRoot, in.Submodules)
	if err != nil {
		return Output{}, fmt.Errorf("resolve repos: %w", err)
	}

	type pending struct {
		repo string
		fut  restate.ResponseFuture[RepoReport]
	}
	futures := make([]pending, 0, len(repos))
	for _, repo := range repos {
		client := restate.Service[RepoReport](ctx, RepoServiceName, "Process")
		futures = append(futures, pending{
			repo: repo,
			fut:  client.RequestFuture(RepoInput{
				Repo:               repo,
				MaxIterationsPerPR: in.MaxIterationsPerPR,
				DryRun:             in.DryRun,
			}),
		})
	}

	var out Output
	for _, p := range futures {
		report, ferr := p.fut.Response()
		if ferr != nil {
			// Synthesize a failed report so the caller still sees
			// something for this repo in the output.
			report = RepoReport{
				Repo:   p.repo,
				Errors: []string{fmt.Sprintf("service call failed: %s", ferr.Error())},
			}
		}
		out.Reports = append(out.Reports, report)
		for _, pr := range report.PRs {
			switch pr.State {
			case PRStateMergedClean, PRStateMergedAfterFix:
				out.Merged = append(out.Merged, pr.URL)
			case PRStateLeftForHuman, PRStateSkippedNotReady, PRStateFailedInternal:
				out.LeftOpen = append(out.LeftOpen, pr.URL)
			}
		}
	}
	return out, nil
}
