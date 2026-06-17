// Package fs holds the small set of filesystem helpers the workflows
// need (tar building, spec file read/write, XML mutation of OBS project
// metadata, HITL-approval-gated patch application).
package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trento-project/trento-workflows/internal/lib"
)

// fileMode is the permission used for files WriteFile / WriteSpec create.
const fileMode = 0o644

// TarOpts configures TarCreate.
type TarOpts struct {
	Excludes  []string // glob patterns passed to --exclude
	Transform string   // sed-style --transform argument
}

// ProjectMetaMutation describes the transform applied to a copied
// factory project meta XML when EnsureSubproject derives a personal one.
type ProjectMetaMutation struct {
	NewProj  string // replaces the `name` attribute
	KeepUser string // only `<person userid="X" .../>` matching this is kept
	OldProj  string // any `<path project="OldProj" .../>` is rewritten to NewProj
}

// TarCreate writes a gzipped tarball at destFile from srcDir with the
// given excludes and pax transform. Shell-outs to the system `tar`
// because GNU tar's --transform/--exclude semantics match what the
// existing obs-test.sh expects.
func TarCreate(ctx context.Context, srcDir, destFile string, opts TarOpts) error {
	if err := os.MkdirAll(filepath.Dir(destFile), 0o750); err != nil {
		return fmt.Errorf("fs.TarCreate mkdir: %w", err)
	}
	args := []string{"tar"}
	for _, e := range opts.Excludes {
		args = append(args, "--exclude="+e)
	}
	if opts.Transform != "" {
		args = append(args, "--transform", opts.Transform)
	}
	args = append(args, "-C", srcDir, "-czf", destFile, "./")
	if _, err := lib.MustSh(ctx, "", args...); err != nil {
		return fmt.Errorf("fs.TarCreate %s: %w", destFile, err)
	}
	return nil
}

// ReadSpec returns the RPM spec file body at path.
func ReadSpec(_ context.Context, path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("fs.ReadSpec %s: %w", path, err)
	}
	return string(body), nil
}

// WriteSpec writes body to path.
func WriteSpec(_ context.Context, path, body string) error {
	if err := os.WriteFile(path, []byte(body), fileMode); err != nil {
		return fmt.Errorf("fs.WriteSpec %s: %w", path, err)
	}
	return nil
}

// specVersionRegexp matches the Version: line in an RPM spec.
var specVersionRegexp = regexp.MustCompile(`(?m)^Version:\s*.*$`)

// SetSpecVersion replaces the `Version:` field in a spec body. Pure
// string function; safe to call from workflow code without restate.Run.
func SetSpecVersion(body, version string) string {
	replacement := "Version:        " + version
	if specVersionRegexp.MatchString(body) {
		return specVersionRegexp.ReplaceAllString(body, replacement)
	}
	// No Version line found; prepend one for safety.
	return replacement + "\n" + body
}

// projectNameRegexp captures the `<project name="…">` root opener.
var projectNameRegexp = regexp.MustCompile(`(<project\s+[^>]*?name=")[^"]*(")`)

// personElemRegexp matches a self-closing `<person userid="…" .../>` element.
var personElemRegexp = regexp.MustCompile(`<person\s+[^/>]*userid="([^"]+)"[^/>]*/>`)

// XMLMutateProjectMeta returns a modified project meta XML per opts.
// Pure string function; safe to call from workflow code.
//
// Mirrors the inline-python in obs-test.sh's ensure_subproject:
//  1. Rewrite the root <project name="…"> to NewProj.
//  2. Drop every <person userid="X" …/> whose userid != KeepUser.
//  3. Rewrite every <path project="OldProj" …/> to NewProj.
//
// String-regex rather than DOM parsing on purpose — OBS emits the meta
// in a stable shape, and pulling in a third-party XML lib for this one
// place would be overkill.
func XMLMutateProjectMeta(xml string, opts ProjectMetaMutation) (string, error) {
	if opts.NewProj == "" {
		return "", errors.New("fs.XMLMutateProjectMeta: NewProj is required")
	}
	out := projectNameRegexp.ReplaceAllString(xml, "${1}"+opts.NewProj+"${2}")
	out = personElemRegexp.ReplaceAllStringFunc(out, func(match string) string {
		m := personElemRegexp.FindStringSubmatch(match)
		if len(m) > 1 && m[1] != opts.KeepUser {
			return ""
		}
		return match
	})
	if opts.OldProj != "" {
		pathRe := regexp.MustCompile(
			`(<path\s+[^>]*?project=")` + regexp.QuoteMeta(opts.OldProj) + `(")`,
		)
		out = pathRe.ReplaceAllString(out, "${1}"+opts.NewProj+"${2}")
	}
	return out, nil
}

// ApplyPatch applies a unified diff to files under submodulePath via
// the system `patch` binary. The diff is written to a temp file and
// fed via -i; -d sets the patching directory so all paths in the diff
// resolve relative to submodulePath.
//
// Refuses absolute paths in the diff body to avoid surprises (e.g. an
// AI emitting `/etc/...`).
//
// On failure: keeps the temp file so the caller can inspect the diff
// that didn't apply, and includes both stdout (where `patch` writes its
// "Hunk #N FAILED at line M" messages) and stderr in the error.
func ApplyPatch(ctx context.Context, submodulePath, diff string) error {
	if submodulePath == "" {
		return errors.New("fs.ApplyPatch: submodulePath is required")
	}
	abs, err := filepath.Abs(submodulePath)
	if err != nil {
		return fmt.Errorf("fs.ApplyPatch abs: %w", err)
	}
	if err := rejectAbsolutePaths(diff); err != nil {
		return err
	}
	f, err := os.CreateTemp("", "trento-patch-*.diff")
	if err != nil {
		return fmt.Errorf("fs.ApplyPatch temp: %w", err)
	}
	// Keep the temp file on failure for post-mortem; remove only on
	// successful apply.
	cleanup := func() { _ = os.Remove(f.Name()) }
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if _, err := f.WriteString(diff); err != nil {
		_ = f.Close()
		return fmt.Errorf("fs.ApplyPatch write: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fs.ApplyPatch close: %w", err)
	}
	stdout, stderr, code, runErr := lib.Sh(ctx, "", "patch", "-p1", "-d", abs, "-i", f.Name())
	if runErr != nil {
		// Process couldn't start (or ctx cancelled). Keep the diff.
		cleanup = nil
		return fmt.Errorf("fs.ApplyPatch run (diff kept at %s): %w", f.Name(), runErr)
	}
	if code != 0 {
		cleanup = nil
		return fmt.Errorf(
			"fs.ApplyPatch %s exited %d (diff kept at %s):\nstdout: %s\nstderr: %s",
			abs, code, f.Name(),
			strings.TrimSpace(stdout),
			strings.TrimSpace(stderr),
		)
	}
	return nil
}

// rejectAbsolutePaths returns an error if the diff contains a `+++ /`
// or `--- /` line. We never want a workflow-driven patch to escape the
// submodule's tree.
func rejectAbsolutePaths(diff string) error {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ /"), strings.HasPrefix(line, "--- /"):
			return fmt.Errorf("fs.ApplyPatch: diff references absolute path: %q", line)
		}
	}
	return nil
}

// WriteFile writes body to path. Caller is responsible for ensuring
// the parent directory exists.
func WriteFile(_ context.Context, path, body string) error {
	if err := os.WriteFile(path, []byte(body), fileMode); err != nil {
		return fmt.Errorf("fs.WriteFile %s: %w", path, err)
	}
	return nil
}

// ReadFile is the generic counterpart of WriteFile (ReadSpec is the
// spec-specific alias kept around for readability at call sites).
func ReadFile(_ context.Context, path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("fs.ReadFile %s: %w", path, err)
	}
	return string(body), nil
}

// MkTempDir creates a fresh temporary directory and returns its
// absolute path. Non-deterministic — always called inside restate.Run.
func MkTempDir(_ context.Context, prefix string) (string, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", fmt.Errorf("fs.MkTempDir %s: %w", prefix, err)
	}
	return dir, nil
}
