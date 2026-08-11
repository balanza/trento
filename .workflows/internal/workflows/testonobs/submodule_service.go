package testonobs

import (
	restate "github.com/restatedev/sdk-go"
)

// SubmoduleServiceName is the Restate service id the top-level
// test-on-obs workflow fans out to (one Process call per submodule).
// The service is stateless and parallel by default — each call gets
// its own goroutine on the handler side.
const SubmoduleServiceName = "trento.test-on-obs.submodule"

// SubmoduleInput is the wire-shape of one Process invocation. The
// top-level workflow has already done the per-run setup (project
// resolution, EnsureSubproject, preflight) and passes the derived
// state in here so the service handler doesn't repeat that work.
type SubmoduleInput struct {
	Sm          string     `json:"sm"`
	Project     string     `json:"project"`
	RepoRoot    string     `json:"repoRoot"`
	SuperSha    string     `json:"superSha"`
	MaxAttempts int        `json:"maxAttempts"`
	FixIterate  FixIterate `json:"fixIterate"`
}

// RegisterSubmoduleService returns the Restate service definition.
// The handler binary binds this alongside the test-on-obs workflow.
func RegisterSubmoduleService() restate.ServiceDefinition {
	return restate.NewService(SubmoduleServiceName).
		Handler("Process", restate.NewServiceHandler(ProcessSubmodule))
}

// ProcessSubmodule is the per-submodule handler. It delegates straight
// to processSubmodule, which knows nothing about Restate beyond the
// shared activity helpers.
//
// Returns nil error even on per-package failures — those are encoded
// in PackageResult.FinalStatus so the top-level workflow can keep
// processing other submodules. A non-nil error escalates to the
// caller's RequestFuture.
func ProcessSubmodule(ctx restate.Context, in SubmoduleInput) ([]PackageResult, error) {
	return processSubmodule(ctx, in.Sm, in.Project, in.RepoRoot, in.SuperSha, in.MaxAttempts, in.FixIterate)
}
