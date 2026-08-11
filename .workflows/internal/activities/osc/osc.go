// Package osc wraps Open Build Service operations performed via the
// `osc` CLI running inside the configured distrobox. All commands flow
// through container.DistroboxRun so that osc's auth+toolchain live in
// the box, not on the host.
package osc

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"strings"

	"github.com/trento-project/trento-workflows/internal/activities/container"
	"github.com/trento-project/trento-workflows/internal/activities/fs"
	"github.com/trento-project/trento-workflows/internal/lib"
)

// Result mirrors one row of `osc results --xml` parsed into typed form.
type Result struct {
	Repository string
	Arch       string
	Package    string
	Code       string // succeeded | failed | scheduled | building | blocked | ...
}

const (
	defaultDistrobox = "osc"
	defaultObsUser   = "balanza"
)

// box returns the distrobox container name where osc is installed.
func box() string {
	if v := os.Getenv("OSC_DISTROBOX"); v != "" {
		return v
	}
	return defaultDistrobox
}

// obsUser returns the OBS user the workflows authenticate as.
func obsUser() string {
	if v := os.Getenv("OBS_USER"); v != "" {
		return v
	}
	return defaultObsUser
}

// runOsc runs `osc <args...>` inside the distrobox and returns stdout.
func runOsc(ctx context.Context, args ...string) (string, error) {
	out, err := container.DistroboxRun(ctx, box(), append([]string{"osc"}, args...))
	if err != nil {
		return "", fmt.Errorf("osc: %w", err)
	}
	return out, nil
}

// runOscIn runs `osc <args...>` inside the distrobox with cwd=dir on
// the host (distrobox enter passes the host cwd through to the box).
// Stdout is discarded — used for commit/addremove where we only care
// about success.
func runOscIn(ctx context.Context, dir string, args ...string) error {
	cmd := append([]string{"distrobox", "enter", "--name", box(), "--root", "--", "osc"}, args...)
	if _, err := lib.MustSh(ctx, dir, cmd...); err != nil {
		return fmt.Errorf("osc in %s: %w", dir, err)
	}
	return nil
}

// projectExists returns true if `osc meta prj <proj>` succeeds.
func projectExists(ctx context.Context, proj string) bool {
	cmd := []string{"distrobox", "enter", "--name", box(), "--root", "--", "osc", "meta", "prj", proj}
	_, _, code, _ := lib.Sh(ctx, "", cmd...)
	return code == 0
}

// packageExists returns true if `osc meta pkg <proj> <pkg>` succeeds.
func packageExists(ctx context.Context, proj, pkg string) bool {
	cmd := []string{"distrobox", "enter", "--name", box(), "--root", "--", "osc", "meta", "pkg", proj, pkg}
	_, _, code, _ := lib.Sh(ctx, "", cmd...)
	return code == 0
}

// withTempFile writes contents to a fresh temp file and invokes fn
// with its path; the file is removed afterwards.
func withTempFile(prefix, contents string, fn func(path string) error) error {
	f, err := os.CreateTemp("", prefix)
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(contents); err != nil {
		_ = f.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	return fn(f.Name())
}

// APIAuthProbe verifies the OBS user can reach /person/$OBS_USER.
func APIAuthProbe(ctx context.Context) error {
	if _, err := runOsc(ctx, "api", "/person/"+obsUser()); err != nil {
		return fmt.Errorf("osc.APIAuthProbe %s: %w", obsUser(), err)
	}
	return nil
}

// GetPrjMeta returns the project meta XML as a string.
func GetPrjMeta(ctx context.Context, proj string) (string, error) {
	out, err := runOsc(ctx, "meta", "prj", proj)
	if err != nil {
		return "", fmt.Errorf("osc.GetPrjMeta %s: %w", proj, err)
	}
	return out, nil
}

// SetPrjMeta writes project meta XML.
func SetPrjMeta(ctx context.Context, proj, xmlBody string) error {
	return withTempFile("trento-osc-prj-meta-*.xml", xmlBody, func(path string) error {
		if _, err := runOsc(ctx, "meta", "prj", proj, "-F", path); err != nil {
			return fmt.Errorf("osc.SetPrjMeta %s: %w", proj, err)
		}
		return nil
	})
}

// GetPrjConf returns the project build configuration.
func GetPrjConf(ctx context.Context, proj string) (string, error) {
	out, err := runOsc(ctx, "meta", "prjconf", proj)
	if err != nil {
		return "", fmt.Errorf("osc.GetPrjConf %s: %w", proj, err)
	}
	return out, nil
}

// SetPrjConf writes the project build configuration.
func SetPrjConf(ctx context.Context, proj, txt string) error {
	return withTempFile("trento-osc-prj-conf-*.conf", txt, func(path string) error {
		if _, err := runOsc(ctx, "meta", "prjconf", proj, "-F", path); err != nil {
			return fmt.Errorf("osc.SetPrjConf %s: %w", proj, err)
		}
		return nil
	})
}

// GetPkgMeta returns the package meta XML.
func GetPkgMeta(ctx context.Context, proj, pkg string) (string, error) {
	out, err := runOsc(ctx, "meta", "pkg", proj, pkg)
	if err != nil {
		return "", fmt.Errorf("osc.GetPkgMeta %s/%s: %w", proj, pkg, err)
	}
	return out, nil
}

// SetPkgMeta writes the package meta XML (use to create packages).
func SetPkgMeta(ctx context.Context, proj, pkg, xmlBody string) error {
	return withTempFile("trento-osc-pkg-meta-*.xml", xmlBody, func(path string) error {
		if _, err := runOsc(ctx, "meta", "pkg", proj, pkg, "-F", path); err != nil {
			return fmt.Errorf("osc.SetPkgMeta %s/%s: %w", proj, pkg, err)
		}
		return nil
	})
}

// EnsureSubproject creates the per-branch subproject from the factory
// project's meta if it doesn't already exist. Idempotent.
//
// Mirrors the bash ensure_subproject: copy factory meta, mutate to
// point at the new project name + keep only the current user as person
// + rewrite repository paths, then copy the factory prjconf so the
// build target set matches.
func EnsureSubproject(ctx context.Context, proj, fromFactory string) error {
	if projectExists(ctx, proj) {
		return nil
	}
	factoryMeta, err := GetPrjMeta(ctx, fromFactory)
	if err != nil {
		return err
	}
	mutated, err := fs.XMLMutateProjectMeta(factoryMeta, fs.ProjectMetaMutation{
		NewProj:  proj,
		KeepUser: obsUser(),
		OldProj:  fromFactory,
	})
	if err != nil {
		return fmt.Errorf("osc.EnsureSubproject mutate meta: %w", err)
	}
	if err := SetPrjMeta(ctx, proj, mutated); err != nil {
		return err
	}
	factoryConf, err := GetPrjConf(ctx, fromFactory)
	if err != nil {
		return err
	}
	if err := SetPrjConf(ctx, proj, factoryConf); err != nil {
		return err
	}
	return nil
}

// minimalPackageMeta is the empty package meta we POST when creating
// a new package via `osc meta pkg <proj> <pkg> -F`.
func minimalPackageMeta(proj, pkg string) string {
	return fmt.Sprintf(`<package name="%s" project="%s"><title/><description/></package>`, pkg, proj)
}

// EnsurePackage creates the package if it doesn't already exist.
func EnsurePackage(ctx context.Context, proj, pkg string) error {
	if packageExists(ctx, proj, pkg) {
		return nil
	}
	return SetPkgMeta(ctx, proj, pkg, minimalPackageMeta(proj, pkg))
}

// CheckoutPackage performs `osc co --output-dir dir proj pkg`. Wipes
// dir first — osc refuses to check out into an existing working copy.
func CheckoutPackage(ctx context.Context, proj, pkg, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("osc.CheckoutPackage clean %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("osc.CheckoutPackage mkdir: %w", err)
	}
	if _, err := runOsc(ctx, "co", "--output-dir", dir, proj, pkg); err != nil {
		return fmt.Errorf("osc.CheckoutPackage %s/%s: %w", proj, pkg, err)
	}
	return nil
}

// CommitPackage performs `osc addremove && osc commit -m msg` inside dir.
func CommitPackage(ctx context.Context, dir, msg string) error {
	if err := runOscIn(ctx, dir, "addremove"); err != nil {
		return fmt.Errorf("osc.CommitPackage addremove %s: %w", dir, err)
	}
	if err := runOscIn(ctx, dir, "commit", "-m", msg); err != nil {
		return fmt.Errorf("osc.CommitPackage commit %s: %w", dir, err)
	}
	return nil
}

// resultsXML / resultXML / statusXML mirror the shape of `osc results
// --xml` so encoding/xml can unmarshal directly.
type resultsXML struct {
	Results []resultXML `xml:"result"`
}
type resultXML struct {
	Repository string      `xml:"repository,attr"`
	Arch       string      `xml:"arch,attr"`
	Statuses   []statusXML `xml:"status"`
}
type statusXML struct {
	Package string `xml:"package,attr"`
	Code    string `xml:"code,attr"`
}

// GetResults returns the parsed status of every build in the project.
//
// Implementation note: we bypass `osc results --xml` and hit OBS's API
// directly via `osc api /build/<proj>/_result`. The high-level
// `results` command misparses a project whose last segment contains a
// dot (`home:balanza:trento.main` → `proj=…trento, pkg=main`) and the
// server replies 404 unknown package. The /build/<proj>/_result URL
// has no such ambiguity.
func GetResults(ctx context.Context, proj string) ([]Result, error) {
	out, err := runOsc(ctx, "api", "/build/"+proj+"/_result")
	if err != nil {
		return nil, fmt.Errorf("osc.GetResults %s: %w", proj, err)
	}
	return parseResultsXML(out)
}

// GetPkgResults is GetResults filtered server-side to a single package.
// Use this when polling for one submodule's packages out of many in a
// project — it cuts the wire payload and the XML parse cost.
func GetPkgResults(ctx context.Context, proj, pkg string) ([]Result, error) {
	out, err := runOsc(ctx, "api", "/build/"+proj+"/_result?package="+pkg)
	if err != nil {
		return nil, fmt.Errorf("osc.GetPkgResults %s/%s: %w", proj, pkg, err)
	}
	return parseResultsXML(out)
}

func parseResultsXML(out string) ([]Result, error) {
	var rs resultsXML
	if err := xml.Unmarshal([]byte(out), &rs); err != nil {
		return nil, fmt.Errorf("parse results XML: %w", err)
	}
	flat := make([]Result, 0, len(rs.Results))
	for _, r := range rs.Results {
		for _, st := range r.Statuses {
			flat = append(flat, Result{
				Repository: r.Repository,
				Arch:       r.Arch,
				Package:    st.Package,
				Code:       st.Code,
			})
		}
	}
	return flat, nil
}

// RemoteBuildLog fetches the build log for one (pkg, repo, arch) tuple.
func RemoteBuildLog(ctx context.Context, proj, pkg, repo, arch string) (string, error) {
	out, err := runOsc(ctx, "remotebuildlog", proj, pkg, repo, arch)
	if err != nil {
		return "", fmt.Errorf("osc.RemoteBuildLog %s/%s/%s/%s: %w", proj, pkg, repo, arch, err)
	}
	return out, nil
}

// RDelete removes the subproject recursively. Used by cleanup.
func RDelete(ctx context.Context, proj string) error {
	if !projectExists(ctx, proj) {
		return nil
	}
	if _, err := runOsc(ctx, "rdelete", "-r", "-m", "cleanup local test", proj); err != nil {
		return fmt.Errorf("osc.RDelete %s: %w", proj, err)
	}
	return nil
}

// IsKnownCode reports whether the given osc result code is one the
// workflow recognises. Useful for surfacing unrecognised states during
// development.
func IsKnownCode(code string) bool {
	switch strings.ToLower(code) {
	case "succeeded", "failed", "unresolvable", "broken",
		"scheduled", "building", "blocked", "finished",
		"signing", "dispatching", "excluded", "disabled", "locked":
		return true
	}
	return false
}
