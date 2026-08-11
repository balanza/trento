// Package container wraps the docker/podman + distrobox shell-out surfaces.
package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/trento-project/trento-workflows/internal/lib"
)

// Runtime is the argv prefix to invoke the container manager. It can
// be a single binary (`["podman"]`, `["docker"]`) or a sudo-prefixed
// pair (`["sudo", "-n", "podman"]`) so rootful setups work in a
// non-interactive handler when NOPASSWD is configured.
type Runtime []string

// Docker returns the unprefixed `docker` runtime. Constructor, not var,
// because []string can't be const and the linter forbids var slices.
func Docker() Runtime { return Runtime{"docker"} }

// Podman returns the unprefixed `podman` runtime.
func Podman() Runtime { return Runtime{"podman"} }

// SudoDocker returns the `sudo -n docker` runtime (rootful via NOPASSWD sudo).
func SudoDocker() Runtime { return Runtime{"sudo", "-n", "docker"} }

// SudoPodman returns the `sudo -n podman` runtime (rootful via NOPASSWD sudo).
func SudoPodman() Runtime { return Runtime{"sudo", "-n", "podman"} }

// Name returns a short single-word label for the runtime, useful for
// logs and metrics.
func (r Runtime) Name() string {
	if len(r) == 0 {
		return ""
	}
	return r[len(r)-1]
}

// Cmd returns argv = r ++ extra for shell-outs.
func (r Runtime) Cmd(extra ...string) []string {
	out := make([]string, 0, len(r)+len(extra))
	out = append(out, r...)
	out = append(out, extra...)
	return out
}

// RunOpts holds options for a one-shot container run.
type RunOpts struct {
	StagingDir string            // bind-mounted at /work
	Env        map[string]string // additional env vars
	LogPath    string            // path to tee the combined output (truncated to last 4 KiB in RunResult.Stdout)
}

// RunResult is what RunOneShot returns to the workflow.
type RunResult struct {
	ExitCode int
	Stdout   string // last 4 KiB of combined stdout+stderr
}

// stdoutTail is how much of the combined output we keep in memory.
const stdoutTail = 4 * 1024

// DetectRuntime probes (rootless docker, rootless podman, sudo-podman,
// sudo-docker) in order and returns the first one whose `info` call
// succeeds. Rootful fallbacks require passwordless sudo for the
// binary; see the workflows README for the sudoers snippet.
func DetectRuntime(ctx context.Context) (Runtime, error) {
	for _, rt := range []Runtime{Docker(), Podman(), SudoPodman(), SudoDocker()} {
		if _, err := exec.LookPath(rt[len(rt)-1]); err != nil {
			continue
		}
		_, _, code, _ := lib.Sh(ctx, "", rt.Cmd("info")...)
		if code == 0 {
			return rt, nil
		}
	}
	return nil, errors.New("container.DetectRuntime: no usable container runtime (tried docker, podman, sudo podman, sudo docker)")
}

// RunOneShot runs `script` inside `image`, with opts.StagingDir
// bind-mounted at /work. Streams output to opts.LogPath while keeping
// only the tail in RunResult.Stdout.
//
// Implementation note: we do NOT pass --user. The image's default user
// is `osc`, which has a valid /etc/passwd entry; overriding with a
// numeric host uid breaks bash startup in this image. Instead the
// staging dir is chmod'd 0o777 before the run.
func RunOneShot(ctx context.Context, image string, opts RunOpts, script string) (RunResult, error) {
	rt, err := DetectRuntime(ctx)
	if err != nil {
		return RunResult{}, err
	}
	if opts.StagingDir == "" {
		return RunResult{}, errors.New("container.RunOneShot: StagingDir is required")
	}
	// Recursive chmod so the container's user can read the files we
	// already wrote (source tarball, spec, etc.). 0o777 is intentional
	// — the staging dir is workflow-owned and ephemeral.
	if err := chmodRecursive(opts.StagingDir, 0o777); err != nil {
		return RunResult{}, fmt.Errorf("container.RunOneShot: chmod staging: %w", err)
	}

	args := rt.Cmd(
		"run", "--rm",
		// `:Z` relabels the bind mount for SELinux. No-op on systems
		// without SELinux. Required on Fedora/openSUSE rootful podman
		// or the container gets "Permission denied" on perfectly
		// world-readable files.
		"-v", opts.StagingDir+":/work:Z",
		"-w", "/work",
		"-e", "HOME=/tmp",
	)
	for k, v := range opts.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, image, "bash", "-xeuo", "pipefail", "-c", script)

	stdout, stderr, code, err := lib.Sh(ctx, "", args...)
	if err != nil {
		return RunResult{}, fmt.Errorf("container.RunOneShot: %w", err)
	}

	combined := stdout + stderr
	if opts.LogPath != "" {
		_ = os.WriteFile(opts.LogPath, []byte(combined), 0o644) //nolint:gosec // tee log
	}
	tail := combined
	if len(tail) > stdoutTail {
		tail = tail[len(tail)-stdoutTail:]
	}
	return RunResult{ExitCode: code, Stdout: tail}, nil
}

// Preflight runs a tiny probe inside `image` to verify the runtime + image
// can actually execute a bash command before we waste minutes per submodule.
func Preflight(ctx context.Context, image string) error {
	rt, err := DetectRuntime(ctx)
	if err != nil {
		return err
	}
	if _, err := lib.MustSh(ctx, "", rt.Cmd(
		"run", "--rm",
		"-e", "HOME=/tmp",
		image,
		"bash", "-c", "id; echo PATH=$PATH; which go mix npm cargo osc || true",
	)...); err != nil {
		return fmt.Errorf("container.Preflight %s: %w", image, err)
	}
	return nil
}

// chmodRecursive applies mode to path and every file/dir under it.
// Errors on individual entries abort the walk.
func chmodRecursive(path string, mode os.FileMode) error {
	walkErr := filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(p, mode)
	})
	if walkErr != nil {
		return fmt.Errorf("chmodRecursive %s: %w", path, walkErr)
	}
	return nil
}

// DistroboxRun executes argv inside the named distrobox container as
// root (--root). Stdout is captured and returned.
func DistroboxRun(ctx context.Context, name string, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("container.DistroboxRun: empty argv")
	}
	cmd := append([]string{"distrobox", "enter", "--name", name, "--root", "--"}, argv...)
	out, err := lib.MustSh(ctx, "", cmd...)
	if err != nil {
		return "", fmt.Errorf("container.DistroboxRun %s: %w", strings.Join(argv, " "), err)
	}
	return out, nil
}
