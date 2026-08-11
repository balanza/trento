// Package testonobs implements the "test-on-obs" workflow: publish the
// current super-repo state to a personal OBS subproject, poll until every
// build reaches a terminal state, optionally drive a Claude-assisted
// fix-iterate loop on the failures, and clean up.
//
// In the v1 scaffold every activity is a no-op stub, so a run completes
// almost instantly with empty results and no failures.
package testonobs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	restate "github.com/restatedev/sdk-go"

	"github.com/trento-project/trento-workflows/internal/activities/claude"
	"github.com/trento-project/trento-workflows/internal/activities/container"
	"github.com/trento-project/trento-workflows/internal/activities/fs"
	"github.com/trento-project/trento-workflows/internal/activities/git"
	"github.com/trento-project/trento-workflows/internal/activities/human"
	"github.com/trento-project/trento-workflows/internal/activities/osc"
)

// WorkflowName is the Restate service identifier.
const WorkflowName = "trento.test-on-obs"

const (
	factoryProject = "devel:sap:trento:factory"
	defaultCDImage = "ghcr.io/trento-project/continuous-delivery:latest"
	obsUserHome    = "home:balanza"
	pollInterval   = 30 * time.Second
	pollTimeout    = 45 * time.Minute
	// publishSettle is how long to wait after publishAll before the
	// first GetResults call, giving OBS time to schedule the builds.
	publishSettle = 30 * time.Second

	// HITL choice strings the fix-iterate prompt offers.
	choiceApply  = "apply"
	choiceSkip   = "skip-package"
	choiceAbort  = "abort-workflow"
	defaultRepo  = "SLES15-Trento-SP3-SP7"
	defaultArch  = "x86_64"
	logTailLines = 80

	// pkgTrentoWebImage is the OBS package name for the web container.
	// It's the one image package that needs an extra VERSION_SHORT
	// replacement service in its _service XML.
	pkgTrentoWebImage = "trento-web-image"
)

// Register returns the Restate workflow definition. The handler binary
// binds this into its endpoint.
func Register() restate.ServiceDefinition {
	return restate.NewWorkflow(WorkflowName).
		Handler("Run", restate.NewWorkflowHandler(Run))
}

// Run is the workflow entrypoint. The flow per submodule is:
//
//  1. publish (tar + stage spec + bundle deps + osc commit)
//  2. wait `publishSettle` so OBS has time to schedule the build
//  3. poll until every (repo,arch) for that submodule's packages is
//     terminal
//  4. all green → next submodule
//  5. red → fetch the build log of each failed package, ask Claude to
//     rewrite the spec
//  6. if Claude can't (NOT_A_SPEC_FIX, unchanged spec, sanity-check
//     fail), bail out for this submodule with the failure recorded
//  7. otherwise, goto 1
//
// Each submodule has its own loop, capped at MaxAttempts. The publish
// step is identical for the initial publish and every retry — the
// previous attempt's Claude edit is now part of the submodule's spec
// on disk, so a fresh `publishOne` picks it up automatically.
func Run(ctx restate.WorkflowContext, in Input) (Output, error) {
	in = applyDefaults(in)
	if err := validator.New().Struct(in); err != nil {
		return Output{}, terminal(fmt.Errorf("input validation: %w", err))
	}

	repoRoot, err := resolveRepoRoot(ctx)
	if err != nil {
		return Output{}, err
	}
	if err := runPreflight(ctx); err != nil {
		return Output{}, err
	}
	project, sha, err := resolveProject(ctx, repoRoot)
	if err != nil {
		return Output{}, err
	}
	if err := runV(ctx, "osc.EnsureSubproject", func(rctx restate.RunContext) error {
		return osc.EnsureSubproject(rctx, project, factoryProject)
	}); err != nil {
		return Output{}, err
	}

	// Fan out: one Process call per submodule via the
	// trento.test-on-obs.submodule service. Each call runs in its own
	// coroutine on the handler side, so all submodules' publish/poll/fix
	// loops overlap. We collect the futures up front and await them in
	// submission order so the Output's results array is deterministic
	// even though the underlying work is parallel.
	type pending struct {
		sm  string
		fut restate.ResponseFuture[[]PackageResult]
	}
	futures := make([]pending, 0, len(in.Submodules))
	for _, sm := range in.Submodules {
		client := restate.Service[[]PackageResult](ctx, SubmoduleServiceName, "Process")
		futures = append(futures, pending{
			sm: sm,
			fut: client.RequestFuture(SubmoduleInput{
				Sm:          sm,
				Project:     project,
				RepoRoot:    repoRoot,
				SuperSha:    sha,
				MaxAttempts: in.MaxAttempts,
				FixIterate:  in.FixIterate,
			}),
		})
	}

	out := Output{Project: project, Results: nil}
	for _, p := range futures {
		results, err := p.fut.Response()
		if err != nil {
			results = []PackageResult{{
				Pkg:         "submodule:" + p.sm,
				FinalStatus: FinalStatusFailed,
				LogsRef:     "service-call:" + p.sm,
			}}
		}
		out.Results = append(out.Results, results...)
	}

	success := allOK(out.Results)
	maybeCleanup(ctx, project, success, in)
	return out, nil
}

// processSubmodule runs the publish/wait/poll/fix loop for a single
// submodule. Returns one PackageResult per produced package (RPM,
// possibly image). Never returns a non-nil error for build/Claude
// failures — those are encoded in PackageResult.FinalStatus so the
// outer loop can keep processing the other submodules. A non-nil
// error means the workflow itself can't continue (e.g. ctx cancel,
// transient infra failure that exhausted Restate retries).
func processSubmodule(
	ctx restate.Context,
	sm, project, repoRoot, superSha string,
	maxAttempts int,
	mode FixIterate,
) ([]PackageResult, error) {
	var results []PackageResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 1+2+3. publish → wait → poll
		fresh, err := publishWaitPoll(ctx, sm, project, repoRoot, superSha)
		if err != nil {
			return nil, err
		}
		results = fresh
		for i := range results {
			results[i].Attempts = attempt
		}
		// 4. all OK → done
		if allOK(results) {
			return results, nil
		}
		// fix-iterate disabled → return with current failures
		if mode == FixIterateOff {
			return results, nil
		}
		// 5+6+7. attempt to fix each failed package. If NONE produced a
		// real spec change, no further progress is possible — bail out.
		if !attemptFixes(ctx, sm, project, repoRoot, results, mode) {
			return results, nil
		}
		// 8. goto 1
	}
	return results, nil
}

// publishWaitPoll runs steps 1-3 of the per-submodule loop: publish,
// wait for OBS to schedule, poll until terminal.
func publishWaitPoll(
	ctx restate.Context,
	sm, project, repoRoot, superSha string,
) ([]PackageResult, error) {
	pkgs, err := publishOne(ctx, sm, project, repoRoot, superSha)
	if err != nil {
		return nil, err
	}
	if err := sleep(ctx, publishSettle); err != nil {
		return nil, err
	}
	return pollSubmodulePkgs(ctx, sm, project, pkgs), nil
}

// attemptFixes asks Claude to rewrite the spec for every failed
// package and writes the new spec on disk. Returns true if at least
// one package made real progress (so the outer loop should try
// another publish/poll cycle), false if no progress was possible.
func attemptFixes(
	ctx restate.Context,
	sm, project, repoRoot string,
	results []PackageResult,
	mode FixIterate,
) bool {
	progress := false
	for i := range results {
		if results[i].FinalStatus != FinalStatusFailed {
			continue
		}
		if proposeAndApplyFix(ctx, sm, project, repoRoot, results[i].Pkg, mode) {
			progress = true
		}
	}
	return progress
}

// pollSubmodulePkgs polls per-package OBS results for the submodule's
// pkgs until each reaches a terminal state, or pollTimeout elapses.
// Updates PackageResult.FinalStatus in place.
//
// Each iteration fires one osc.GetPkgResults activity per package so
// we only fetch the rows we care about, instead of pulling the whole
// project's results and filtering client-side. Cheaper on the wire
// when several submodules are polling the same project concurrently.
func pollSubmodulePkgs(
	ctx restate.Context,
	sm, project string,
	pkgs []PackageResult,
) []PackageResult {
	_ = sm // currently only used for activity labels below
	for elapsed := time.Duration(0); elapsed < pollTimeout; elapsed += pollInterval {
		stillWaiting := false
		for i := range pkgs {
			raw, err := runT(ctx, "osc.GetPkgResults:"+pkgs[i].Pkg, func(rctx restate.RunContext) ([]osc.Result, error) {
				return osc.GetPkgResults(rctx, project, pkgs[i].Pkg)
			})
			if err != nil {
				pkgs[i].FinalStatus = FinalStatusFailed
				continue
			}
			switch aggregateStatus(pkgs[i].Pkg, raw) {
			case bucketOK:
				pkgs[i].FinalStatus = FinalStatusOK
			case bucketFail:
				pkgs[i].FinalStatus = FinalStatusFailed
			case bucketPending, bucketAwaiting, bucketUnknown:
				stillWaiting = true
			}
		}
		if !stillWaiting {
			return pkgs
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return pkgs
		}
	}
	for i := range pkgs {
		if pkgs[i].FinalStatus == "" {
			pkgs[i].FinalStatus = FinalStatusFailed
		}
	}
	return pkgs
}

// proposeAndApplyFix dispatches on the package type: image packages
// get a Dockerfile rewrite, everything else gets a spec rewrite.
// Returns true iff a real change was applied (so the outer loop should
// try another publish/poll cycle).
func proposeAndApplyFix(
	ctx restate.Context,
	sm, project, repoRoot, pkg string,
	mode FixIterate,
) bool {
	if isImagePackage(pkg) {
		return runFixCycle(ctx, sm, project, repoRoot, pkg, mode, imageFixer())
	}
	return runFixCycle(ctx, sm, project, repoRoot, pkg, mode, specFixer())
}

// fileFixer captures everything that differs between the spec-fix and
// image-fix paths: where the source file lives, how to prompt Claude
// for a rewrite, and a sanity-check that the response looks plausible.
type fileFixer struct {
	Kind       string // for activity labels + HITL prompt titles
	PathFor    func(repoRoot, pkg string) (string, error)
	PromptFor  func(pkg, log, body string) string
	LooksValid func(s string) bool
}

func specFixer() fileFixer {
	return fileFixer{
		Kind:       "spec",
		PathFor:    specPathFor,
		PromptFor:  fixPromptForPackage,
		LooksValid: looksLikeSpec,
	}
}

func imageFixer() fileFixer {
	return fileFixer{
		Kind:       "dockerfile",
		PathFor:    dockerfilePathFor,
		PromptFor:  fixPromptForImagePackage,
		LooksValid: looksLikeDockerfile,
	}
}

// runFixCycle is the shared fix loop body: fetch log → read source →
// claude → validate → apply. Returns false on any no-progress signal
// (NOT_A_SPEC_FIX, sanity-fail, byte-identical reply, HITL declined,
// activity error).
func runFixCycle(
	ctx restate.Context,
	sm, project, repoRoot, pkg string,
	mode FixIterate,
	f fileFixer,
) bool {
	// Image builds run in different (repo, arch) combos than RPMs
	// (typically the `images` repo for containers vs the base
	// `SLES15-…` repo for RPMs). Discover the actual failed target
	// before fetching the log — using defaultRepo/defaultArch
	// blindly returns 404 "no logfile" when the build wasn't
	// scheduled there.
	repo, arch, ok := findLogTarget(ctx, project, pkg)
	if !ok {
		repo, arch = defaultRepo, defaultArch
	}
	logTxt, err := runT(ctx, "osc.RemoteBuildLog:"+pkg, func(rctx restate.RunContext) (string, error) {
		return osc.RemoteBuildLog(rctx, project, pkg, repo, arch)
	})
	if err != nil {
		return false
	}
	sourcePath, err := f.PathFor(repoRoot, pkg)
	if err != nil {
		return false
	}
	oldBody, err := runT(ctx, "fs.ReadFile:"+f.Kind+":"+pkg, func(rctx restate.RunContext) (string, error) {
		return fs.ReadFile(rctx, sourcePath)
	})
	if err != nil {
		return false
	}
	resp, err := runT(ctx, "claude.Invoke:"+f.Kind+":"+pkg, func(rctx restate.RunContext) (claude.Response, error) {
		return claude.Invoke(rctx, claude.Request{
			Prompt:       f.PromptFor(pkg, logTxt, oldBody),
			Files:        []string{sourcePath},
			AllowedTools: []string{"Read"},
			Cwd:          submodulePathFor(repoRoot, pkg),
		})
	})
	if err != nil {
		return false
	}
	newBody := stripDiffFences(resp.Text)
	if strings.TrimSpace(newBody) == notASpecFixSentinel {
		return false
	}
	if !f.LooksValid(newBody) {
		return false
	}
	if newBody == oldBody {
		return false
	}
	if mode == FixIterateReview {
		choice, err := human.Confirm(ctx, human.ConfirmReq{
			Title:   fmt.Sprintf("%s: review proposed %s rewrite", pkg, f.Kind),
			Body:    renderReviewBody(pkg, logTxt, newBody),
			Choices: []string{choiceApply, choiceSkip, choiceAbort},
		})
		if err != nil || choice != choiceApply {
			return false
		}
	}
	if err := runV(ctx, "fs.WriteFile:"+f.Kind+":"+sm+":"+pkg, func(rctx restate.RunContext) error {
		return fs.WriteFile(rctx, sourcePath, newBody)
	}); err != nil {
		return false
	}
	return true
}

// findLogTarget queries osc results to find a concrete (repo, arch)
// where pkg has produced a log. Prefers failed targets — that's the
// log Claude needs to read — falling back to any non-skipped result.
// Returns ok=false when no usable target is found.
//
// Why: image packages don't necessarily build in the same repo/arch
// as RPMs (containers typically build under `images` or
// `containers_*` repos). Hardcoding (defaultRepo, defaultArch)
// produces 404 "package … has no logfile" because the build never
// happened in that specific target.
func findLogTarget(ctx restate.Context, project, pkg string) (string, string, bool) {
	raw, err := runT(ctx, "osc.GetPkgResults:locate:"+pkg, func(rctx restate.RunContext) ([]osc.Result, error) {
		return osc.GetPkgResults(rctx, project, pkg)
	})
	if err != nil {
		return "", "", false
	}
	// First pass: prefer rows that actually failed (Claude needs the
	// failure context, not a succeeded-elsewhere log).
	for _, r := range raw {
		if r.Package != pkg {
			continue
		}
		switch r.Code {
		case "failed", "unresolvable", "broken":
			return r.Repository, r.Arch, true
		}
	}
	// Second pass: any non-terminally-skipped target so we at least
	// fetch *some* log instead of bailing.
	for _, r := range raw {
		if r.Package != pkg {
			continue
		}
		switch r.Code {
		case "excluded", "disabled", "locked":
			continue
		}
		return r.Repository, r.Arch, true
	}
	return "", "", false
}

// isImagePackage reports whether pkg is one of the container image
// packages (those whose name ends in "-image"). They're committed via
// Dockerfile + _service rather than spec + sources, so their fix path
// rewrites the Dockerfile, not a spec.
func isImagePackage(pkg string) bool {
	return strings.HasSuffix(pkg, "-image")
}

// dockerfilePathFor returns the Dockerfile path for an image package.
// All image packages live in `<sm>/packaging/suse/container/Dockerfile`.
func dockerfilePathFor(repoRoot, pkg string) (string, error) {
	sm := submoduleForPkg(pkg)
	if sm == "" {
		return "", fmt.Errorf("dockerfilePathFor: unknown package %s", pkg)
	}
	p := repoRoot + "/" + sm + "/packaging/suse/container/Dockerfile"
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("dockerfilePathFor %s at %s: %w", pkg, p, err)
	}
	return p, nil
}

// looksLikeDockerfile is the image-package equivalent of looksLikeSpec.
// A real Dockerfile must contain at least one FROM directive.
func looksLikeDockerfile(s string) bool {
	const minDockerfileLen = 30
	if len(s) < minDockerfileLen {
		return false
	}
	head := s
	if len(head) > 2048 {
		head = head[:2048]
	}
	// Match FROM at the start of any line, with optional whitespace.
	for _, line := range strings.Split(head, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "FROM ") || strings.HasPrefix(trimmed, "FROM\t") {
			return true
		}
	}
	return false
}

// fixPromptForImagePackage is the prompt used when the failing
// package is a container image. Same contract as fixPromptForPackage
// (full-file rewrite, NOT_A_SPEC_FIX sentinel for opt-out) but tuned
// for Dockerfiles.
func fixPromptForImagePackage(pkg, logTxt, dockerfileBody string) string {
	// The placeholder markers are wrapped in two percent signs each.
	// Passing the literal as a %s arg avoids fmt.Sprintf reinterpreting
	// the percents in the prompt template.
	const placeholderMarkers = "%%VERSION%% / %%VERSION_SHORT%%"
	return fmt.Sprintf(`The OBS image build for container package %q failed.

Build log (truncated to last %d lines):
%s

Current Dockerfile (also passed to OBS as the build input):
%s

Task: rewrite the Dockerfile to fix the build failure.

Output rules — IMPORTANT, the workflow writes your response verbatim
to the Dockerfile.

  1. Output the COMPLETE, NEW Dockerfile. Not a diff. Not a snippet.
  2. No Markdown fences (no `+"```"+`dockerfile / `+"```"+` blocks).
     No prose. No commentary before or after.
  3. The first non-blank line should be a directive (FROM, ARG, LABEL,
     etc.) or a comment (#).
  4. Make the CHANGE minimal — one missing package, one wrong path,
     one bad RUN command. Preserve every other line verbatim (same
     order, same whitespace, same comments).
  5. Do NOT change the %s placeholder markers — OBS fills them in at
     build time via _service replace_using_package_version directives.
     Keep them exactly as-is.
  6. If the failure is NOT addressable via a Dockerfile edit (e.g. the
     base image is broken, or the failure is in an upstream RPM
     package this image installs), output exactly:
     %s
     and nothing else.
`,
		pkg, logTailLines, tailLines(logTxt, logTailLines), dockerfileBody,
		placeholderMarkers,
		notASpecFixSentinel,
	)
}

// allOK reports whether every PackageResult has FinalStatusOK.
func allOK(results []PackageResult) bool {
	for _, r := range results {
		if r.FinalStatus != FinalStatusOK {
			return false
		}
	}
	return len(results) > 0
}

// runPreflight verifies osc auth and container runtime before any real work.
func runPreflight(ctx restate.Context) error {
	if err := runV(ctx, "osc.APIAuthProbe", func(rctx restate.RunContext) error {
		return osc.APIAuthProbe(rctx)
	}); err != nil {
		return err
	}
	return runV(ctx, "container.Preflight", func(rctx restate.RunContext) error {
		return container.Preflight(rctx, defaultCDImage)
	})
}

// resolveProject derives the personal OBS subproject name from the
// current branch and returns it along with the super-repo short SHA.
func resolveProject(ctx restate.Context, repoRoot string) (string, string, error) {
	branch, err := runT(ctx, "git.Branch", func(rctx restate.RunContext) (string, error) {
		return git.Branch(rctx, repoRoot)
	})
	if err != nil {
		return "", "", err
	}
	sha, err := runT(ctx, "git.Head", func(rctx restate.RunContext) (string, error) {
		return git.Head(rctx, repoRoot)
	})
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("%s:trento.%s", obsUserHome, sanitizeBranch(branch)), sha, nil
}

// maybeCleanup conditionally drops the OBS subproject after the run.
func maybeCleanup(ctx restate.Context, project string, success bool, in Input) {
	if (success && in.CleanupOnSuccess) || (!success && in.CleanupOnFailure) {
		_ = runV(ctx, "osc.RDelete", func(rctx restate.RunContext) error {
			return osc.RDelete(rctx, project)
		})
	}
}

// publishOne builds the RPM (and optional image) packages for a single
// submodule. Returns one PackageResult per produced package.
//
// The pipeline mirrors obs-test.sh's per-package publish:
//  1. compute version from `git describe`
//  2. tar the submodule sources into staging
//  3. copy the spec into staging and patch its Version: line
//  4. bundle per-language dependencies inside the CD container
//  5. ensure the OBS package exists, check it out, copy staged files
//     into the checkout, commit.
func publishOne(
	ctx restate.Context,
	sm, project, repoRoot, superSha string,
) ([]PackageResult, error) {
	version, err := runT(ctx, "synthVersion:"+sm, func(rctx restate.RunContext) (string, error) {
		return synthVersion(rctx, repoRoot, sm, superSha)
	})
	if err != nil {
		return nil, err
	}
	rpmPkg := rpmPackageName(sm)
	staging := stagingDir(rpmPkg)

	if err := runV(ctx, "fs.TarCreate:"+sm, func(rctx restate.RunContext) error {
		return fs.TarCreate(rctx, repoRoot+"/"+sm, stagingPath(rpmPkg, version), fs.TarOpts{
			Excludes:  []string{".git", "_build", "deps", "node_modules", "vendor", "target"},
			Transform: fmt.Sprintf("s,^./,%s-%s/,", rpmPkg, version),
		})
	}); err != nil {
		return nil, err
	}

	if err := runV(ctx, "stageSpec:"+sm, func(rctx restate.RunContext) error {
		return stageSpec(rctx, repoRoot, sm, rpmPkg, version)
	}); err != nil {
		return nil, err
	}

	if err := runV(ctx, "bundleSubmoduleDeps:"+sm, func(rctx restate.RunContext) error {
		return bundleDeps(rctx, sm, staging, version)
	}); err != nil {
		return nil, err
	}

	// Web needs a _service file declaring the node_modules cpio and the
	// package-lock.json that the obs-service-node_modules binary consumed.
	if sm == SMWeb {
		if err := runV(ctx, "stageWebAux:"+sm, func(_ restate.RunContext) error {
			return stageWebAux(repoRoot, staging, rpmPkg)
		}); err != nil {
			return nil, err
		}
	}

	if err := pushPackage(ctx, project, rpmPkg, staging, superSha); err != nil {
		return nil, err
	}
	results := []PackageResult{{Pkg: rpmPkg, FinalStatus: "", Attempts: 0, LogsRef: "rpm/" + rpmPkg}}

	if imagePkg := imagePackageName(sm); imagePkg != "" {
		if err := runV(ctx, "stageImagePackage:"+sm, func(rctx restate.RunContext) error {
			return stageImagePackage(rctx, repoRoot, sm, imagePkg, rpmPkg)
		}); err != nil {
			return nil, err
		}
		if err := pushPackage(ctx, project, imagePkg, stagingDir(imagePkg), superSha); err != nil {
			return nil, err
		}
		results = append(results, PackageResult{
			Pkg: imagePkg, FinalStatus: "", Attempts: 0, LogsRef: "image/" + imagePkg,
		})
	}
	return results, nil
}

// stageWebAux copies package-lock.json and writes the _service file
// into the web RPM package staging dir. The _service declares the
// node_modules cpio so OBS handles extraction the same way it does for
// the SCM-synced factory package.
func stageWebAux(repoRoot, staging, pkg string) error {
	// Copy assets/package-lock.json from the submodule into staging.
	src := repoRoot + "/" + SMWeb + "/assets/package-lock.json"
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("stageWebAux: package-lock.json not found at %s: %w", src, err)
	}
	if err := copyFile(src, staging+"/package-lock.json"); err != nil {
		return fmt.Errorf("stageWebAux copy package-lock.json: %w", err)
	}
	// Write _service XML referencing the cpio produced by bundleWebDeps.
	svc := fmt.Sprintf(`<services>
  <service name="node_modules" mode="manual">
    <param name="cpio">node_modules.obscpio</param>
    <param name="output">node_modules.spec.inc</param>
    <param name="source-offset">10000</param>
  </service>
</services>
`)
	if err := os.WriteFile(staging+"/_service", []byte(svc), 0o644); err != nil { //nolint:gosec // non-secret build config
		return fmt.Errorf("stageWebAux write _service: %w", err)
	}
	return nil
}

// stageImagePackage copies the submodule's Dockerfile (and optional
// README.md) into the image package's staging dir and writes the OBS
// `_service` XML that drives the version-stamping at build time.
//
// Mirrors obs-test.sh's publish_image_package: no source tarball, no
// vendor archive — image packages are pure container builds whose
// version metadata comes from `kiwi_metainfo_helper` +
// `replace_using_package_version` services.
func stageImagePackage(_ restate.RunContext, repoRoot, sm, imagePkg, rpmPkg string) error {
	srcDir := repoRoot + "/" + sm + "/packaging/suse/container"
	staging := stagingDir(imagePkg)
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("stageImagePackage clean %s: %w", staging, err)
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return fmt.Errorf("stageImagePackage mkdir: %w", err)
	}
	if err := copyFile(srcDir+"/Dockerfile", staging+"/Dockerfile"); err != nil {
		return fmt.Errorf("stageImagePackage Dockerfile: %w", err)
	}
	if _, err := os.Stat(srcDir + "/README.md"); err == nil {
		// README is optional; best-effort copy.
		_ = copyFile(srcDir+"/README.md", staging+"/README.md")
	}
	if err := os.WriteFile(staging+"/_service", []byte(buildImageService(imagePkg, rpmPkg)), 0o644); err != nil { //nolint:gosec // non-secret build config
		return fmt.Errorf("stageImagePackage _service: %w", err)
	}
	return nil
}

// imageServiceTmpl is the OBS service XML that stamps the container
// image's version metadata at build time. The trento-web-image
// variant additionally injects a parse-version/patch block that we
// splice into __VERSION_SHORT_BLOCK__.
//
// Raw string keeps the `%%VERSION%%` literals intact (no fmt.Sprintf
// percent-escaping headaches); placeholders are token-substituted.
const imageServiceTmpl = `<services>
  <service name="docker_label_helper" mode="buildtime"/>
  <service name="kiwi_metainfo_helper" mode="buildtime"/>
  <service name="replace_using_package_version" mode="buildtime">
    <param name="file">Dockerfile</param>
    <param name="regex">%%VERSION%%</param>
    <param name="package">__RPM_PKG__</param>
  </service>__VERSION_SHORT_BLOCK__
  <service name="replace_using_package_version" mode="buildtime">
    <param name="file">Dockerfile</param>
    <param name="regex">\+git\.</param>
    <param name="replacement">-git.</param>
  </service>
</services>
`

const versionShortBlockTmpl = `
  <service name="replace_using_package_version" mode="buildtime">
    <param name="file">Dockerfile</param>
    <param name="regex">%%VERSION_SHORT%%</param>
    <param name="parse-version">patch</param>
    <param name="package">__RPM_PKG__</param>
  </service>`

func buildImageService(imagePkg, rpmPkg string) string {
	versionShort := ""
	// Only pkgTrentoWebImage needs the VERSION_SHORT replacement — its
	// Dockerfile has both VERSION and VERSION_SHORT placeholders.
	if imagePkg == pkgTrentoWebImage {
		versionShort = strings.ReplaceAll(versionShortBlockTmpl, "__RPM_PKG__", rpmPkg)
	}
	out := strings.ReplaceAll(imageServiceTmpl, "__RPM_PKG__", rpmPkg)
	out = strings.ReplaceAll(out, "__VERSION_SHORT_BLOCK__", versionShort)
	return out
}

// pushPackage ensures the package exists in the project, checks it out,
// copies the staged files into the checkout, and commits.
func pushPackage(ctx restate.Context, project, pkg, staging, superSha string) error {
	if err := runV(ctx, "osc.SetPkgMeta:"+pkg, func(rctx restate.RunContext) error {
		return osc.SetPkgMeta(rctx, project, pkg, minimalPkgMeta(project, pkg))
	}); err != nil {
		return err
	}
	if err := runV(ctx, "osc.CheckoutPackage:"+pkg, func(rctx restate.RunContext) error {
		return osc.CheckoutPackage(rctx, project, pkg, checkoutDir(pkg))
	}); err != nil {
		return err
	}
	if err := runV(ctx, "copyStaged:"+pkg, func(rctx restate.RunContext) error {
		return copyStagedToCheckout(rctx, staging, checkoutDir(pkg))
	}); err != nil {
		return err
	}
	return runV(ctx, "osc.CommitPackage:"+pkg, func(rctx restate.RunContext) error {
		return osc.CommitPackage(rctx, checkoutDir(pkg), "local test "+superSha)
	})
}

// notASpecFixSentinel is the literal Claude is told to output when the
// failure can't be addressed by a spec edit. We treat it as "skip" so
// the outer loop stops re-asking with the same context.
const notASpecFixSentinel = "NOT_A_SPEC_FIX"

// minPlausibleSpecLen guards against accepting a Claude reply that's
// way too short to be a real spec (e.g. the agent returned an apology
// line). Real specs are hundreds of bytes minimum.
const minPlausibleSpecLen = 100

// looksLikeSpec is a cheap heuristic to reject obviously-bad Claude
// outputs (apologies, single-line replies, totally unrelated text)
// without overwriting the real spec. Looks for at least one RPM
// directive in the first few lines.
func looksLikeSpec(s string) bool {
	if len(s) < minPlausibleSpecLen {
		return false
	}
	head := s
	if len(head) > 2048 {
		head = head[:2048]
	}
	for _, marker := range []string{"Name:", "Version:", "Summary:", "%description", "%prep", "%build", "%install"} {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

// --- helpers (all pure or deterministic given inputs) ---

func resolveRepoRoot(ctx restate.Context) (string, error) {
	return runT(ctx, "resolveRepoRoot", func(_ restate.RunContext) (string, error) {
		if rr := os.Getenv("TRENTO_REPO_ROOT"); rr != "" {
			return rr, nil
		}
		return "..", nil
	})
}

func sanitizeBranch(b string) string {
	return regexp.MustCompile(`[^a-zA-Z0-9._-]+`).ReplaceAllString(b, "_")
}

// rpmPackageName maps a submodule to its RPM package per the spec table.
func rpmPackageName(sm string) string {
	switch sm {
	case SMWeb:
		return "trento-web"
	case SMWanda:
		return "trento-wanda"
	case SMAgent:
		return "trento-agent"
	case SMMCPServer:
		return "mcp-server-trento"
	case SMChecks:
		return "trento-checks"
	}
	return ""
}

// imagePackageName returns the kiwi image package or "" when N/A.
func imagePackageName(sm string) string {
	switch sm {
	case SMWeb:
		return pkgTrentoWebImage
	case SMWanda:
		return "trento-wanda-image"
	case SMMCPServer:
		return "mcp-server-trento-image"
	case SMChecks:
		return "trento-checks-image"
	}
	return ""
}

// stagingDir is the per-package staging directory (split per package so
// the RPM and the image package don't trample each other's files).
func stagingDir(pkg string) string {
	return ".runs/stage/" + pkg
}

// stagingPath is the source tarball path inside the RPM package's
// staging dir. Spec/vendor archives land alongside it.
func stagingPath(pkg, version string) string {
	return stagingDir(pkg) + "/" + pkg + "-" + version + ".tar.gz"
}

func checkoutDir(pkg string) string {
	return ".runs/co/" + pkg
}

func minimalPkgMeta(proj, pkg string) string {
	return fmt.Sprintf(`<package name="%s" project="%s"><title/><description/></package>`, pkg, proj)
}

// synthVersion mirrors the obs-test.sh synth_version function: combines
// the submodule's `git describe` with the super-repo's short SHA and a
// .dirty suffix when the working tree is unclean.
func synthVersion(rctx restate.RunContext, repoRoot, sm, superSha string) (string, error) {
	desc, err := git.Describe(rctx, repoRoot+"/"+sm)
	if err != nil {
		return "", fmt.Errorf("git.Describe: %w", err)
	}
	dirty, err := git.Dirty(rctx, repoRoot+"/"+sm)
	if err != nil {
		return "", fmt.Errorf("git.Dirty: %w", err)
	}
	v := strings.ReplaceAll(desc, "-", ".") + "+local." + superSha
	if dirty {
		v += ".dirty"
	}
	return v, nil
}

// bundleDeps dispatches on the submodule to bundle its language-specific
// dependency closure inside the CD container, producing the auxiliary
// archives the spec file expects (vendor.tar.gz for Go, deps.tar.gz for
// Elixir, node_modules.obscpio for web, etc.).
func bundleDeps(rctx restate.RunContext, sm, staging, version string) error {
	pkg := rpmPackageName(sm)
	srcDir := pkg + "-" + version
	switch sm {
	case SMChecks:
		return nil
	case SMAgent, SMMCPServer:
		script := fmt.Sprintf(`set -euo pipefail
cd /work
rm -rf %s
tar xzf %s.tar.gz
cd %s
go mod vendor
tar czf /work/vendor.tar.gz vendor
cd /work
rm -rf %s
`, srcDir, srcDir, srcDir, srcDir)
		res, err := container.RunOneShot(rctx, defaultCDImage, container.RunOpts{
			StagingDir: staging,
			LogPath:    staging + "/.container.log",
			Env:        nil,
		}, script)
		if err != nil {
			return fmt.Errorf("bundleDeps %s: %w", sm, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("bundleDeps %s exit %d: %s", sm, res.ExitCode, res.Stdout)
		}
		return nil
	case SMWeb:
		return bundleWebDeps(rctx, staging, srcDir)
	case SMWanda:
		return bundleWandaDeps(rctx, staging, srcDir)
	}
	return fmt.Errorf("bundleDeps: unknown submodule %q", sm)
}

// bundleWebDeps runs inside the CD container to bundle Elixir mix deps
// and npm node_modules for the web submodule. Produces deps.tar.gz,
// node_modules.obscpio, and node_modules.spec.inc in the staging dir.
func bundleWebDeps(rctx restate.RunContext, staging, srcDir string) error {
	script := fmt.Sprintf(`set -euo pipefail
cd /work
rm -rf %[1]s
tar xzf %[1]s.tar.gz
cd %[1]s
export MIX_HOME=/tmp/.mix MIX_REBAR3=/usr/bin/rebar3
mix local.hex --force >/dev/null
mix local.rebar --force >/dev/null
mix deps.get
tar czf /work/deps.tar.gz deps
cd /work
# obs-service-node_modules expects package-lock.json in CWD; copy it from
# the extracted source tree so we don't need a host-side pre-copy step.
cp %[1]s/assets/package-lock.json ./package-lock.json
/usr/lib/obs/service/node_modules \
  --input package-lock.json \
  --output node_modules.spec.inc \
  --cpio node_modules.obscpio \
  --source-offset 10000 \
  --outdir /work \
  --download
rm -rf %[1]s
`, srcDir)
	res, err := container.RunOneShot(rctx, defaultCDImage, container.RunOpts{
		StagingDir: staging,
		LogPath:    staging + "/.container.log",
		Env:        nil,
	}, script)
	if err != nil {
		return fmt.Errorf("bundleWebDeps: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("bundleWebDeps exit %d: %s", res.ExitCode, res.Stdout)
	}
	for _, f := range []string{"deps.tar.gz", "node_modules.obscpio", "node_modules.spec.inc"} {
		if _, err := os.Stat(staging + "/" + f); err != nil {
			return fmt.Errorf("bundleWebDeps: missing %s after container run: %w", f, err)
		}
	}
	return nil
}

// bundleWandaDeps runs inside the CD container to bundle Elixir mix deps
// and Rust vendor for the wanda submodule. Produces deps.tar.gz and
// vendor-rhai_rustler.tar.gz in the staging dir.
func bundleWandaDeps(rctx restate.RunContext, staging, srcDir string) error {
	script := fmt.Sprintf(`set -euo pipefail
cd /work
rm -rf %[1]s
tar xzf %[1]s.tar.gz
cd %[1]s
export MIX_HOME=/tmp/.mix MIX_REBAR3=/usr/bin/rebar3
mix local.hex --force >/dev/null
mix local.rebar --force >/dev/null
mix deps.get
tar czf /work/deps.tar.gz deps
cd deps/rhai_rustler/native/rhai_rustler
cargo vendor vendor
tar czf /work/vendor-rhai_rustler.tar.gz vendor Cargo.lock
cd /work
rm -rf %[1]s
`, srcDir)
	res, err := container.RunOneShot(rctx, defaultCDImage, container.RunOpts{
		StagingDir: staging,
		LogPath:    staging + "/.container.log",
		Env:        nil,
	}, script)
	if err != nil {
		return fmt.Errorf("bundleWandaDeps: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("bundleWandaDeps exit %d: %s", res.ExitCode, res.Stdout)
	}
	for _, f := range []string{"deps.tar.gz", "vendor-rhai_rustler.tar.gz"} {
		if _, err := os.Stat(staging + "/" + f); err != nil {
			return fmt.Errorf("bundleWandaDeps: missing %s after container run: %w", f, err)
		}
	}
	return nil
}

// stageSpec finds the submodule's spec file, copies it into the
// package's staging dir, and patches the `Version:` line.
func stageSpec(_ restate.RunContext, repoRoot, sm, pkg, version string) error {
	src, err := findSpec(repoRoot, sm, pkg)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("stageSpec read %s: %w", src, err)
	}
	patched := fs.SetSpecVersion(string(body), version)
	// trento-web spec uses %%GTM_ID%% which OBS normally replaces via a
	// regex_replace source service. Since we ship static tarballs, we
	// patch it here instead.
	if pkg == "trento-web" {
		patched = strings.ReplaceAll(patched, "%%GTM_ID%%", "GTM-N3JHF5M6")
	}
	dest := stagingDir(pkg) + "/" + pkg + ".spec"
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("stageSpec mkdir: %w", err)
	}
	if err := os.WriteFile(dest, []byte(patched), 0o644); err != nil { //nolint:gosec // spec is non-secret
		return fmt.Errorf("stageSpec write %s: %w", dest, err)
	}
	return nil
}

// findSpec returns the path to the spec file for pkg under sm.
// Mirrors obs-test.sh's two-candidate lookup: rpm/ first, then the
// bare packaging/suse dir.
func findSpec(repoRoot, sm, pkg string) (string, error) {
	candidates := []string{
		repoRoot + "/" + sm + "/packaging/suse/rpm/" + pkg + ".spec",
		repoRoot + "/" + sm + "/packaging/suse/" + pkg + ".spec",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("findSpec: %s not found for %s under %s", pkg, sm, repoRoot)
}

// copyStagedToCheckout flat-copies every top-level non-dotfile from
// staging into checkout, mirroring obs-test.sh's
//
//	find $staging -maxdepth 1 -type f -exec cp -t $checkout {} +
//
// Dotfiles (e.g. `.container.log`) are skipped so they don't leak into
// the OBS commit.
func copyStagedToCheckout(_ restate.RunContext, staging, checkout string) error {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("copyStaged read %s: %w", staging, err)
	}
	if err := os.MkdirAll(checkout, 0o750); err != nil {
		return fmt.Errorf("copyStaged mkdir %s: %w", checkout, err)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		src := staging + "/" + e.Name()
		dst := checkout + "/" + e.Name()
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copyStaged %s -> %s: %w", src, dst, err)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// statusBucket is the aggregated state we derive from the raw osc result
// codes for one package. Matches the buckets in obs-test.sh's
// cmd_status, with one addition: bucketAwaiting distinguishes "no
// results yet for this package" (right after publish, OBS hasn't
// scheduled it) from "no result rows at all" (the project is empty).
type statusBucket int

const (
	bucketUnknown  statusBucket = iota
	bucketOK                    // all (repo,arch) are succeeded or terminal-non-build (excluded/disabled/locked)
	bucketFail                  // any (repo,arch) failed/unresolvable/broken
	bucketPending               // any (repo,arch) is scheduled/building/blocked/finished/signing/dispatching
	bucketAwaiting              // no rows present for this package — OBS hasn't scheduled it yet
)

// aggregateStatus mirrors obs-test.sh cmd_status's bucketing:
//
//	succeeded                                 → OK
//	failed | unresolvable | broken            → FAIL
//	scheduled | building | blocked | finished
//	  | signing | dispatching                 → PEND
//	excluded | disabled | locked              → soft-OK (terminal, not built, not a failure)
//	(anything else)                           → bucketUnknown (treat as pending so we keep polling)
//
// When no rows reference the package, returns bucketAwaiting so the
// caller keeps polling — common right after publish, before OBS
// schedules the build.
func aggregateStatus(pkg string, raw []osc.Result) statusBucket {
	hasFail, hasPend, hasOK, hasAny := false, false, false, false
	for _, r := range raw {
		if r.Package != pkg {
			continue
		}
		hasAny = true
		switch r.Code {
		case "succeeded":
			hasOK = true
		case "failed", "unresolvable", "broken":
			hasFail = true
		case "scheduled", "building", "blocked", "finished", "signing", "dispatching":
			hasPend = true
		case "excluded", "disabled", "locked":
			// terminal non-build; doesn't contribute to OK/FAIL/PEND
		default:
			// codes osc may add in the future — better to keep polling
			// than silently terminate. Once observed, add to the
			// matching case above and bump osc.IsKnownCode.
			hasPend = true
		}
	}
	switch {
	case !hasAny:
		return bucketAwaiting
	case hasFail:
		return bucketFail
	case hasPend:
		return bucketPending
	case hasOK:
		return bucketOK
	}
	// All rows were excluded/disabled/locked — package is intentionally
	// not built for any target, treat as OK.
	return bucketOK
}

// specPathFor returns the spec file location for a package. Delegates
// to findSpec for the two-candidate lookup (rpm/ subdir vs bare
// packaging/suse dir — different submodules use different layouts).
func specPathFor(repoRoot, pkg string) (string, error) {
	sm := submoduleForPkg(pkg)
	if sm == "" {
		return "", fmt.Errorf("unknown package: %s", pkg)
	}
	return findSpec(repoRoot, sm, pkg)
}

// submodulePathFor returns the submodule root for a given package.
func submodulePathFor(repoRoot, pkg string) string {
	sm := submoduleForPkg(pkg)
	if sm == "" {
		return repoRoot
	}
	return repoRoot + "/" + sm
}

func submoduleForPkg(pkg string) string {
	switch pkg {
	case "trento-web", pkgTrentoWebImage:
		return SMWeb
	case "trento-wanda", "trento-wanda-image":
		return SMWanda
	case "trento-agent":
		return SMAgent
	case "mcp-server-trento", "mcp-server-trento-image":
		return SMMCPServer
	case "trento-checks", "trento-checks-image":
		return SMChecks
	}
	return ""
}

func fixPromptForPackage(pkg, logTxt, specBody string) string {
	return fmt.Sprintf(`The OBS build for package %q failed.

Build log (truncated to last %d lines):
%s

Current spec file:
%s

Task: rewrite the spec to fix the build failure.

Output rules — IMPORTANT, the workflow writes your response verbatim
to the spec file. Diffs are too fragile for this loop; we need the
whole spec back.

  1. Output the COMPLETE, NEW spec file. Not a diff. Not a snippet.
     Every Source:, Patch:, %%setup, %%build, etc. line that should be
     in the final spec must be present in your output.
  2. No Markdown fences (no `+"```"+`spec / `+"```"+` blocks).
     No prose. No commentary before or after.
  3. The first line should be a comment (#) or a directive
     (Name:, Version:, BuildRequires:, etc.).
  4. Make the CHANGE minimal — one BuildRequires line, one path fix,
     one macro typo. Preserve every other line of the existing spec
     verbatim (same order, same whitespace, same comments).
  5. Do NOT modify the Version: line — the workflow re-stamps it on
     every publish.
  6. If the failure is NOT addressable via a spec edit (e.g. it's in
     upstream source code or in the build container), output exactly:
     %s
     and nothing else. The workflow will record that and stop retrying.
`, pkg, logTailLines, tailLines(logTxt, logTailLines), specBody, notASpecFixSentinel)
}

// stripDiffFences removes Markdown code fences from a diff if Claude
// added them despite the prompt. Tolerant of variations:
//
//	```diff\n...\n```      → ...
//	```patch\n...\n```     → ...
//	```\n...\n```          → ...
//
// Returns the input unchanged if no fences are present.
func stripDiffFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the first line (opening fence with optional language tag).
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	// Drop a trailing closing fence on its own line.
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = strings.TrimRightFunc(s[:i], func(r rune) bool { return r == '\n' || r == ' ' })
	}
	return s
}

func renderReviewBody(pkg, logTxt, proposal string) string {
	return fmt.Sprintf(`Package: %s

Build log (last 40 lines):
%s

Proposed patch:
%s`, pkg, tailLines(logTxt, 40), proposal)
}

func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
