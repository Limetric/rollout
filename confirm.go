package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// `rollout confirm <token>` applies a staged write by token alone, so the user
// doesn't have to re-type the original command with --confirm appended. The
// pending file already stores everything needed (platform, tool, package,
// payload), and applying exactly what was staged preserves the token/tool
// binding that the per-tool confirm path enforces.

// ConfirmResult is a WriteResult plus the tool that staged the write, so the
// caller can see what they just applied.
type ConfirmResult struct {
	Tool string `json:"tool"`
	WriteResult
}

// runConfirm consumes the token and applies its staged mutation.
func runConfirm(ctx context.Context, c mutationApplier, token string) (ConfirmResult, error) {
	// Parity with the per-tool confirm path, but on a peek — before the
	// single-use token is consumed — so a temporarily blocked confirm doesn't
	// burn the token the user would need after fixing the configuration.
	peeked, err := peekMutation(token)
	if err != nil {
		return ConfirmResult{}, err
	}
	cfg := loadSafetyConfig()
	if err := checkBlockedOperation(peeked.Tool, cfg); err != nil {
		return ConfirmResult{}, toolError(peeked.Tool, err)
	}
	if err := checkRolloutFraction(peeked.RolloutFraction, cfg); err != nil {
		return ConfirmResult{}, toolError(peeked.Tool, err)
	}
	p, err := consumeMutation(token)
	if err != nil {
		return ConfirmResult{}, err
	}
	res, err := applyConsumed(ctx, c, p)
	if err != nil {
		return ConfirmResult{}, err
	}
	return ConfirmResult{Tool: p.Tool, WriteResult: res}, nil
}

// applierForToken builds the client that can apply the write staged under
// token. The pending file names the platform that staged it, so a token is
// never applied through the wrong store's API — and the client is built only
// for the platform being confirmed, so an unconfigured second platform cannot
// break a confirm for the first.
func applierForToken(ctx context.Context, token string) (mutationApplier, string, error) {
	p, err := peekMutation(token)
	if err != nil {
		return nil, "", err
	}
	name := p.platform()
	plat, err := lookupPlatform(name)
	if err != nil {
		return nil, "", fmt.Errorf("confirmation %q was staged by unknown platform %q: %w", token, name, err)
	}
	if plat.NewApplier == nil {
		return nil, "", fmt.Errorf("%s has no write tools, but confirmation %q claims to be one", plat.Title, token)
	}
	applier, err := plat.NewApplier(ctx)
	if err != nil {
		return nil, "", err
	}
	return applier, name, nil
}

// --- CLI front-end ---

var confirmCmd = &cobra.Command{
	Use:   "confirm <token>",
	Short: "Apply a previously previewed write by its confirm token",
	Long:  "Apply a staged write exactly as previewed, identified by the confirm token from\nthe preview — no need to re-run the original command with --confirm.\n\nThe token remembers which platform staged it, so `rollout confirm` works the\nsame for every platform. Destructive operations return a second token that must\nbe confirmed once more.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		applier, _, err := applierForToken(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := runConfirm(cmd.Context(), applier, args[0])
		if err != nil {
			return err
		}
		if err := printJSON(cmd.OutOrStdout(), res); err != nil {
			return err
		}
		// The hint goes to stderr so stdout stays valid JSON for jq pipelines.
		if !res.Applied && res.ConfirmToken != "" {
			errOut := cmd.ErrOrStderr()
			s := newStyles(errOut)
			fmt.Fprintf(errOut, "%s %s\n", s.failure("Second confirmation required:"), s.accent("rollout confirm "+res.ConfirmToken))
		}
		return nil
	},
}
