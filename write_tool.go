package main

import (
	"context"
	"fmt"
)

// The shared half of the write path: the result shape every write tool returns
// and the apply sequence a confirmed token runs through. Which endpoint the
// write lands on is the platform's business (see write_play.go).

// WriteResult is the standard structured output for a write tool. The first
// call (no confirm token) returns ConfirmToken+Preview; the confirm call
// returns Applied=true with a Detail summary and the edit that carried it.
type WriteResult struct {
	Applied      bool   `json:"applied"`
	ConfirmToken string `json:"confirm_token,omitempty"`
	Preview      string `json:"preview,omitempty"`
	Detail       string `json:"detail,omitempty"`
	// EditID is the Play edit the confirmed write was committed in. It is the
	// handle that correlates a change here with the Console's activity log.
	EditID string `json:"edit_id,omitempty"`
	// NextActionHint points the agent at the next tool to call to continue a
	// workflow (e.g. promoting a staged release once its rollout looks healthy).
	NextActionHint *NextActionHint `json:"next_action_hint,omitempty"`
}

// NextActionHint names the tool that continues a workflow, so an agent does not
// have to guess what comes after a staged rollout.
type NextActionHint struct {
	Tool        string         `json:"tool"`
	Params      map[string]any `json:"params"`
	Description string         `json:"description"`
}

// previewResult wraps a freshly staged pending mutation as a preview WriteResult.
func previewResult(p *PendingMutation) WriteResult {
	return WriteResult{Applied: false, ConfirmToken: p.Token, Preview: p.previewText()}
}

// applyConfirmed consumes a confirm token and applies the staged write via the
// correct dispatch, writing an audit line on both success and failure.
func applyConfirmed(ctx context.Context, c mutationApplier, tool, confirm string) (WriteResult, error) {
	p, err := consumeMutation(confirm)
	if err != nil {
		return WriteResult{}, err
	}
	// A token is bound to the platform *and* the tool that staged it.
	//
	// The tool binding is what stops a play_halt_release preview from being
	// executed through play_promote_release. The platform binding keeps that
	// true once a second store names a tool the same way — without it, a Play
	// token passed to another platform's confirm would clear the tool check and
	// reach an applier that has never heard of the route, after the staged
	// write is already gone.
	if staged := p.platform(); staged != c.platformName() {
		return WriteResult{}, fmt.Errorf("confirmation token was issued for %s, not %s — the staged operation (%s) has been discarded; re-run the original command against %s for a fresh preview", staged, c.platformName(), p.Summary, staged)
	}
	if p.Tool != tool {
		return WriteResult{}, fmt.Errorf("confirmation token was issued by %q, not %q — the staged operation (%s) has been discarded; re-run the original command for a fresh preview", p.Tool, tool, p.Summary)
	}
	return applyConsumed(ctx, c, p)
}

// applyConsumed finishes a consumed pending write: it re-stages destructive
// operations for their second confirmation, otherwise applies and audits.
// Shared by the per-tool confirm path (applyConfirmed) and `rollout confirm`.
func applyConsumed(ctx context.Context, c mutationApplier, p *PendingMutation) (WriteResult, error) {
	// Destructive operations take two confirmations: the first consume
	// re-stages under a fresh token instead of applying.
	if p.RequiresDouble && !p.DoubleConfirmed {
		p2, err := restageForDoubleConfirm(p)
		if err != nil {
			return WriteResult{}, err
		}
		return WriteResult{
			Applied:      false,
			ConfirmToken: p2.Token,
			Preview:      p2.previewText() + "\nDESTRUCTIVE — a second confirmation is required. Re-run with the token above to apply.",
			Detail:       "second confirmation required",
		}, nil
	}
	outcome, err := c.applyMutation(ctx, p)
	if err != nil {
		auditLog(p, outcome, err)
		return WriteResult{}, toolError(p.Tool, err)
	}
	auditLog(p, outcome, nil)
	res := WriteResult{Applied: true, Detail: p.Summary}
	if outcome != nil {
		res.EditID = outcome.EditID
		if outcome.Detail != "" {
			res.Detail = outcome.Detail
		}
	}
	return res, nil
}
