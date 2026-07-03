package dependabotsweep

import (
	"fmt"

	restate "github.com/restatedev/sdk-go"
)

// runV wraps restate.RunVoid with a label and tags any error with that
// label. Mirrors the helpers in fixprci and patchrelease.
func runV(ctx restate.Context, label string, fn func(restate.RunContext) error) error {
	if err := restate.RunVoid(ctx, fn, restate.WithName(label)); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// runT wraps restate.Run with a label and tags any error with that
// label. Mirrors the helpers in fixprci; patchrelease uses the shared
// release.RunT.
func runT[T any](
	ctx restate.Context,
	label string,
	fn func(restate.RunContext) (T, error),
) (T, error) {
	v, err := restate.Run(ctx, fn, restate.WithName(label))
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%s: %w", label, err)
	}
	return v, nil
}
