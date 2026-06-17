// Package human exposes HITL primitives built on Restate awakeables.
// Confirm/Await are stubs in the v1 scaffold; they short-circuit with
// the TimeoutChoice so the fix-iterate loop falls through to skip.
package human

import (
	"time"

	restate "github.com/restatedev/sdk-go"
)

// DefaultTimeout is the soft cap before an unresolved awakeable is
// auto-resolved with TimeoutChoice. Real impl will respect this; the
// stub ignores it.
const DefaultTimeout = 24 * time.Hour

// TimeoutChoice is the synthetic choice returned when an awakeable times
// out. The test-on-obs workflow treats it as "skip-package".
const TimeoutChoice = "timeout"

// ConfirmReq is the multi-choice prompt the human sees.
type ConfirmReq struct {
	Title   string
	Body    string
	Choices []string
}

// AwaitReq is the no-choice ack variant.
type AwaitReq struct {
	Title string
	Body  string
}

// Confirm publishes a pending decision and blocks until a human resolves
// the awakeable via Restate's UI or CLI. Stub: returns TimeoutChoice.
//
// Takes restate.Context (the base type) so both workflow handlers and
// plain service handlers can use it — restate.Awakeable accepts Context.
//
// TODO: real impl creates restate.Awakeable[string](ctx), publishes
// (id, req) to the pending-board surface, then awaits awk.Result()
// raced against restate.After(ctx, DefaultTimeout).
func Confirm(_ restate.Context, _ ConfirmReq) (string, error) {
	return TimeoutChoice, nil
}

// Await is the no-choice variant. Stub returns nil immediately.
//
// TODO: real impl creates restate.Awakeable[restate.Void](ctx) and
// blocks on awk.Result() with the same timeout race as Confirm.
func Await(_ restate.Context, _ AwaitReq) error {
	return nil
}
