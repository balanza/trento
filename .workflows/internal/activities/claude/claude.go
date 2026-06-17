// Package claude wraps `claude -p` invocations. Mirrors the shape of
// hack/amake-claude: receives extra args + a final prompt and forwards
// to `claude` as `[extra_args...] -p "<prompt>"`.
package claude

import (
	"context"
	"fmt"
	"strings"

	"github.com/trento-project/trento-workflows/internal/lib"
)

// Request is the input contract for an agent invocation.
type Request struct {
	Prompt       string   // the prompt text (rendered from a template)
	Files        []string // files to attach (paths relative to Cwd); appended to the prompt as a context block
	AllowedTools []string // e.g. {"Read"} for proposal-only, {"Read","Edit"} for self-apply
	Cwd          string   // working directory; sandboxes file access for the agent
}

// Response carries what the agent returned.
type Response struct {
	Text      string // model's final text output (stdout from `claude -p`)
	SessionID string // not currently populated; reserved for future use
}

// Invoke runs `claude -p` with the request and returns the response.
//
// claude CLI args used:
//   - --allowedTools "<csv>"   — restrict tool surface
//   - --add-dir "<cwd>"        — give the agent access to the working tree
//   - -p "<prompt>"            — the prompt (must be last per amake-claude)
//
// Files in req.Files are surfaced as a Markdown context block appended
// to the prompt so the agent knows what's relevant without needing to
// glob.
func Invoke(ctx context.Context, req Request) (Response, error) {
	prompt := buildPrompt(req)
	args := []string{"claude"}
	if len(req.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.AllowedTools, ","))
	}
	if req.Cwd != "" {
		args = append(args, "--add-dir", req.Cwd)
	}
	args = append(args, "-p", prompt)

	out, err := lib.MustSh(ctx, req.Cwd, args...)
	if err != nil {
		return Response{}, fmt.Errorf("claude.Invoke: %w", err)
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
