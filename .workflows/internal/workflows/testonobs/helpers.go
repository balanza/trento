package testonobs

import (
	"fmt"
	"time"

	restate "github.com/restatedev/sdk-go"
)

// runV wraps restate.RunVoid with a label and tags any error with that
// label so call sites don't need to re-wrap external errors.
func runV(ctx restate.Context, label string, fn func(restate.RunContext) error) error {
	if err := restate.RunVoid(ctx, fn, restate.WithName(label)); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// runT wraps restate.Run with a label and tags any error with that
// label so call sites don't need to re-wrap external errors.
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

// sleep wraps restate.Sleep with an unconditional error wrap.
func sleep(ctx restate.Context, d time.Duration) error {
	if err := restate.Sleep(ctx, d); err != nil {
		return fmt.Errorf("sleep: %w", err)
	}
	return nil
}

// terminal wraps restate.TerminalError so the result can be returned
// directly without tripping wrapcheck.
func terminal(err error) error {
	return fmt.Errorf("terminal: %w", restate.TerminalError(err))
}
