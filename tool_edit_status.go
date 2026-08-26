package main

import (
	"context"
	"encoding/json"

	"github.com/spf13/cobra"
)

// EditStatusArgs probes whether this credential can open an edit on the app.
type EditStatusArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
}

// EditStatusResult reports whether a write would be allowed.
type EditStatusResult struct {
	PackageName string `json:"package_name"`
	// CanEdit is the question this tool exists to answer.
	CanEdit bool `json:"can_edit"`
	// EditID is the edit that was opened and immediately deleted. It is
	// reported so a failure can be correlated with the Console's activity log.
	EditID string `json:"edit_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (r EditStatusResult) tableRows() ([]json.RawMessage, []string) {
	return []json.RawMessage{jsonRow(r)}, []string{"package_name", "can_edit", "reason"}
}

// runEditStatus opens an edit and immediately deletes it.
//
// It is the cheapest possible "can I write?" probe, and it exists as a tool
// because the alternative is finding out at confirm time: an agent that stages
// a release write, hands a token to a human, and only then discovers the
// service account was never invited has wasted everybody's turn. A credential
// that mints tokens fine can still have no access to this app.
//
// It answers rather than fails when the API says no — "can_edit: false" with
// the reason is the useful shape for a precondition check, and a hard error
// would make an agent treat a clear answer as a broken tool.
func runEditStatus(ctx context.Context, c *Client, args EditStatusArgs) (EditStatusResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return EditStatusResult{}, err
	}
	editID, err := c.withEdit(ctx, pkg, false, commitOptions{}, func(*editSession) error { return nil })
	if err != nil {
		// A definitive rejection is an answer. Anything else — a 5xx, a dropped
		// connection — says nothing about permissions, so it stays an error.
		if liveVerdictFor(err) == liveFailed {
			return EditStatusResult{PackageName: pkg, CanEdit: false, Reason: err.Error()}, nil
		}
		return EditStatusResult{}, toolError("edit_status", err)
	}
	return EditStatusResult{PackageName: pkg, CanEdit: true, EditID: editID}, nil
}

// --- CLI front-end ---

var (
	editStatusArgs   EditStatusArgs
	editStatusFormat string
)

var editCmd = &cobra.Command{
	Use:   "edit",
	Short: "Inspect the publishing edit session",
}

var editStatusCmd = &cobra.Command{
	Use:         "status",
	Short:       "Check whether this credential can open an edit (a cheap write-permission probe)",
	Annotations: mcpTool("edit_status"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, editStatusArgs, editStatusFormat, runEditStatus)
	},
}

func init() {
	addPackageFlag(editStatusCmd, &editStatusArgs.PackageName)
	addFormatFlag(editStatusCmd, &editStatusFormat)
	editCmd.AddCommand(editStatusCmd)
}
