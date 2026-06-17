package lib

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Sh runs argv[0] with argv[1:] in dir (cwd if empty) and returns
// (stdout, stderr, exitCode, err). A non-zero exit is NOT an error in
// itself — the caller decides what exit codes mean. err is set only
// when the process couldn't start, ctx was cancelled, or some other
// non-exit failure occurred (in which case exitCode is -1).
func Sh(ctx context.Context, dir string, argv ...string) (string, string, int, error) {
	if len(argv) == 0 {
		return "", "", -1, errors.New("sh: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // dev tool, args are caller-controlled
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
	}
	return stdout.String(), stderr.String(), -1, fmt.Errorf("sh %s: %w", argv[0], err)
}

// MustSh is Sh that surfaces a non-zero exit as an error, including
// stderr in the message for diagnostics.
func MustSh(ctx context.Context, dir string, argv ...string) (string, error) {
	stdout, stderr, code, err := Sh(ctx, dir, argv...)
	if err != nil {
		return stdout, err
	}
	if code != 0 {
		return stdout, fmt.Errorf("%s exited %d: %s", argv[0], code, strings.TrimSpace(stderr))
	}
	return stdout, nil
}
