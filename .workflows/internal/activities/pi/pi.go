// Package pi wraps `pi` CLI invocations. Mirrors the shape of the
// claude activity: receives extra args + a final prompt and forwards
// to `pi` as `[extra_args...] -p`, with the prompt fed via stdin
// to avoid the OS argv limit (E2BIG).
package pi

import (
	"context"
	"fmt"
	"strings"

	"github.com/trento-project/trento-workflows/internal/lib"
)

const defaultModel = "opencode-go/deepseek-v4-flash"

// Request is the input contract for a pi agent invocation.
type Request struct {
	Prompt       string   // the prompt text (rendered from a template)
	Files        []string // files to attach (paths relative to Cwd); appended to the prompt as a context block
	AllowedTools []string // e.g. {"Read"} for proposal-only, {"Read","Edit"} for self-apply
	Cwd          string   // working directory
	Model        string   // model name, optionally prefixed with provider (e.g. "opencode-go/deepseek-v4-flash"); defaults to defaultModel
}

// Response carries what the agent returned.
type Response struct {
	Text      string // model's final text output (stdout from `pi -p`)
	SessionID string // not currently populated; reserved for future use
}

// piToolNames maps the PascalCase tool names used by the claude activity
// (and all callers) to the lowercase names the pi CLI expects.
var piToolNames = map[string]string{
	"Read":  "read",
	"Bash":  "bash",
	"Edit":  "edit",
	"Write": "write",
}

// Invoke runs `pi` with the request and returns the response.
//
// pi CLI args used:
//   - --model "<provider>/<name>"   — which model to use; provider prefix is required
//   - --tools "<csv>"               — restrict tool surface (tool names mapped to lowercase)
//   - -p / --print                  — non-interactive mode: process prompt and exit
//
// The prompt is fed via stdin to bypass the OS argv limit (E2BIG).
// Files in req.Files are surfaced as a Markdown context block appended
// to the prompt so the agent knows what's relevant without needing to
// glob.
func Invoke(ctx context.Context, req Request) (Response, error) {
	prompt := buildPrompt(req)
	model := req.Model
	if model == "" {
		model = defaultModel
	}
	args := []string{"pi", "--model", model}
	if len(req.AllowedTools) > 0 {
		names := make([]string, 0, len(req.AllowedTools))
		for _, t := range req.AllowedTools {
			if mapped, ok := piToolNames[t]; ok {
				names = append(names, mapped)
			}
		}
		if len(names) > 0 {
			args = append(args, "--tools", strings.Join(names, ","))
		}
	}
	args = append(args, "--print")

	out, err := lib.MustShStdin(ctx, req.Cwd, prompt, args...)
	if err != nil {
		return Response{}, fmt.Errorf("pi.Invoke: %w", err)
	}
	return Response{Text: strings.TrimSpace(out), SessionID: ""}, nil
}

// buildPrompt appends a file-attachment list to the prompt body so the
// agent knows which files matter for the task without scanning.
func buildPrompt(req Request) string {
	if len(req.Files) == 0 {
		return req.Prompt
	}
	var b strings.Builder
	b.WriteString(req.Prompt)
	b.WriteString("\n\nRelevant files:\n")
	for _, f := range req.Files {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	return b.String()
}
