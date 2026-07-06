package dependabotsweep

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyMergeState(t *testing.T) {
	cases := []struct {
		state string
		want  mergeAction
	}{
		{"clean", mergeNow},
		{"blocked", approveThenMerge},
		{"dirty", skipNotReady},
		{"behind", skipNotReady},
		{"unstable", skipNotReady},
		{"has_hooks", skipNotReady},
		{"", skipNotReady},
		{"unknown", skipNotReady},
	}
	for _, c := range cases {
		got := classifyMergeState(c.state)
		assert.Equal(t, c.want, got, "state=%q", c.state)
	}
}

func TestClassifyPostApprovalState(t *testing.T) {
	cases := []struct {
		state string
		want  mergeAction
	}{
		{"clean", mergeNow},
		{"blocked", skipNotReady},
		{"dirty", skipNotReady},
		{"behind", skipNotReady},
		{"unstable", skipNotReady},
		{"has_hooks", skipNotReady},
		{"", skipNotReady},
	}
	for _, c := range cases {
		got := classifyPostApprovalState(c.state)
		assert.Equal(t, c.want, got, "state=%q", c.state)
	}
}

func TestClassifyInitialNote(t *testing.T) {
	cases := []struct {
		state string
		want  string
	}{
		{"", "mergeable_state unknown (API not yet computed)"},
		{"dirty", "mergeable_state=dirty (branch needs rebase)"},
		{"behind", "mergeable_state=behind (branch needs rebase)"},
		{"unstable", "mergeable_state=unstable (CI still failing — fix-pr-ci should have caught this)"},
		{"has_hooks", "mergeable_state=has_hooks (pre-receive hooks configured — needs human)"},
		{"blocked", "mergeable_state=\"blocked\" (not actionable)"},
		{"weird-future-value", "mergeable_state=\"weird-future-value\" (not actionable)"},
	}
	for _, c := range cases {
		got := classifyInitialNote(c.state)
		assert.Equal(t, c.want, got, "state=%q", c.state)
	}
}

func TestClassifyPostApprovalNote(t *testing.T) {
	cases := []struct {
		state string
		want  string
	}{
		{"blocked", "approval applied but additional reviews required (e.g. CODEOWNERS)"},
		{"behind", "approval applied but branch is now behind base"},
		{"dirty", "approval applied but branch now has merge conflicts"},
		{"unstable", "approval applied but a CI check is still failing"},
		{"", "approval applied but mergeable_state not yet computed"},
		{"weird-future-value", "approval applied but mergeable_state=\"weird-future-value\""},
	}
	for _, c := range cases {
		got := classifyPostApprovalNote(c.state)
		assert.Equal(t, c.want, got, "state=%q", c.state)
	}
}

// TestApproveThenMerge_ScenarioWalkthrough exercises the full state
// machine end-to-end (without invoking gh) by composing the
// classifiers the way tryMergeWithApproval does. This guards the
// shape of the decision flow against regressions.
func TestApproveThenMerge_ScenarioWalkthrough(t *testing.T) {
	type result struct {
		merged bool
		note   string
	}
	cases := []struct {
		name              string
		initial           string
		approvalSucceeded bool
		postApproval      string
		want              result
	}{
		{
			name:    "clean: no approval needed",
			initial: "clean",
			want:    result{merged: true, note: ""},
		},
		{
			name:              "blocked -> approval -> clean: merge",
			initial:           "blocked",
			approvalSucceeded: true,
			postApproval:      "clean",
			want:              result{merged: true, note: ""},
		},
		{
			name:              "blocked -> approval -> still blocked: skip",
			initial:           "blocked",
			approvalSucceeded: true,
			postApproval:      "blocked",
			want:              result{merged: false, note: "approval applied but additional reviews required (e.g. CODEOWNERS)"},
		},
		{
			name:              "blocked -> approval -> behind: skip",
			initial:           "blocked",
			approvalSucceeded: true,
			postApproval:      "behind",
			want:              result{merged: false, note: "approval applied but branch is now behind base"},
		},
		{
			name:              "blocked -> approval failed: skip",
			initial:           "blocked",
			approvalSucceeded: false,
			want:              result{merged: false, note: "approval failed"},
		},
		{
			name:    "dirty: skip (no approval attempted)",
			initial: "dirty",
			want:    result{merged: false, note: "mergeable_state=dirty (branch needs rebase)"},
		},
		{
			name:    "behind: skip (no approval attempted)",
			initial: "behind",
			want:    result{merged: false, note: "mergeable_state=behind (branch needs rebase)"},
		},
		{
			name:    "unstable: skip (no approval attempted)",
			initial: "unstable",
			want:    result{merged: false, note: "mergeable_state=unstable (CI still failing — fix-pr-ci should have caught this)"},
		},
		{
			name:    "empty: skip (no approval attempted)",
			initial: "",
			want:    result{merged: false, note: "mergeable_state unknown (API not yet computed)"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got result
			switch classifyMergeState(c.initial) {
			case mergeNow:
				got = result{merged: true, note: ""}
			case approveThenMerge:
				if !c.approvalSucceeded {
					got = result{merged: false, note: "approval failed"}
				} else {
					if classifyPostApprovalState(c.postApproval) == mergeNow {
						got = result{merged: true, note: ""}
					} else {
						got = result{merged: false, note: classifyPostApprovalNote(c.postApproval)}
					}
				}
			case skipNotReady:
				got = result{merged: false, note: classifyInitialNote(c.initial)}
			}
			assert.Equal(t, c.want, got, c.name)
		})
	}
}
