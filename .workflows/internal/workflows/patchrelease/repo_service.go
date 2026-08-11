package patchrelease

import (
	restate "github.com/restatedev/sdk-go"
)

// RepoServiceName is the Restate service identifier used by the
// top-level patch-release workflow to fan out one Process call per
// repo. The service is stateless and parallel by default — each call
// gets its own goroutine on the handler side.
const RepoServiceName = "trento.patch-release.repo"

// RepoInput is the wire-shape of one Process invocation.
type RepoInput struct {
	Repo  string `json:"repo"`  // owner/name
	Agent string `json:"agent"` // "claude" (default) or "pi"
}

// RegisterRepoService returns the Restate service definition. The
// handler binary binds this alongside the patch-release workflow.
func RegisterRepoService() restate.ServiceDefinition {
	return restate.NewService(RepoServiceName).
		Handler("Process", restate.NewServiceHandler(Process))
}

// Process is the per-repo handler. It delegates to processRepo with
// the service Context (which satisfies the same interface as
// WorkflowContext for restate.Run / RunVoid).
//
// Process never returns an error: per-repo failures are captured in
// the RepoReport's Failed field so the top-level workflow can keep
// processing other repos. A non-nil error here would propagate to the
// caller's RequestFuture and we'd lose the partial report.
func Process(ctx restate.Context, in RepoInput) (RepoReport, error) {
	return processRepo(ctx, in.Repo, in.Agent), nil
}
