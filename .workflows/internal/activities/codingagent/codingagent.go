// Package codingagent provides a unified abstraction over pi and claude
// coding agent clients. Callers specify which client to use, and the
// package routes the request to the corresponding implementation.
package codingagent

import (
	"context"
	"fmt"

	"github.com/trento-project/trento-workflows/internal/activities/claude"
	"github.com/trento-project/trento-workflows/internal/activities/pi"
)

// Client identifies which coding agent backend to use.
type Client string

const (
	// Claude uses the `claude` CLI binary.
	Claude Client = "claude"
	// Pi uses the `pi` CLI binary.
	Pi Client = "pi"
)

// Request is the input contract for a coding agent invocation. It
// covers the superset of fields accepted by both the claude and pi
// activities. Fields that only apply to one backend are silently
// ignored for the other.
type Request struct {
	Prompt       string   // the prompt text (rendered from a template)
	Files        []string // files to attach (paths relative to Cwd)
	AllowedTools []string // e.g. {"Read"} for proposal-only
	Cwd          string   // working directory
	Model        string   // model identifier (pi only; ignored for claude)
}

// Response carries what the agent returned.
type Response struct {
	Text      string
	SessionID string
}

// Invoke dispatches the request to either the claude or pi backend
// depending on the client parameter. Returns an error for unknown
// client values.
func Invoke(ctx context.Context, client Client, req Request) (Response, error) {
	switch client {
	case Claude:
		return invokeClaude(ctx, req)
	case Pi:
		return invokePi(ctx, req)
	default:
		return Response{}, fmt.Errorf("codingagent: unknown client %q", client)
	}
}

// ParseClient parses a client name string. Returns an error for
// invalid values. Accepts "claude" and "pi" (case-sensitive).
func ParseClient(s string) (Client, error) {
	switch Client(s) {
	case Claude, Pi:
		return Client(s), nil
	default:
		return "", fmt.Errorf("codingagent: invalid client %q (must be %q or %q)", s, Claude, Pi)
	}
}

func invokeClaude(ctx context.Context, req Request) (Response, error) {
	resp, err := claude.Invoke(ctx, claude.Request{
		Prompt:       req.Prompt,
		Files:        req.Files,
		AllowedTools: req.AllowedTools,
		Cwd:          req.Cwd,
	})
	if err != nil {
		return Response{}, fmt.Errorf("codingagent: %w", err)
	}
	return Response{Text: resp.Text, SessionID: resp.SessionID}, nil
}

func invokePi(ctx context.Context, req Request) (Response, error) {
	resp, err := pi.Invoke(ctx, pi.Request{
		Prompt:       req.Prompt,
		Files:        req.Files,
		AllowedTools: req.AllowedTools,
		Cwd:          req.Cwd,
		Model:        req.Model,
	})
	if err != nil {
		return Response{}, fmt.Errorf("codingagent: %w", err)
	}
	return Response{Text: resp.Text, SessionID: resp.SessionID}, nil
}
