package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

// TestersArgs reads the tester groups of a closed-testing track.
type TestersArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track       string `json:"track" jsonschema:"the testing track (internal, alpha, beta, or a custom closed-testing track name)"`
}

// TestersResult is a track's tester groups.
//
// Only Google Groups appear here. The Publisher API has no per-email tester
// list at all — individual testers exist only in the Console — so a caller
// seeing an empty list has not lost anything, and adding an email address is
// not something this tool can offer.
type TestersResult struct {
	PackageName  string   `json:"package_name"`
	Track        string   `json:"track"`
	GoogleGroups []string `json:"google_groups"`
}

func (r TestersResult) tableRows() ([]json.RawMessage, []string) {
	rows := make([]json.RawMessage, 0, len(r.GoogleGroups))
	for _, group := range r.GoogleGroups {
		rows = append(rows, jsonRow(map[string]string{"track": r.Track, "google_group": group}))
	}
	return rows, []string{"track", "google_group"}
}

// apiTesters is the wire shape.
type apiTesters struct {
	GoogleGroups []string `json:"googleGroups"`
}

// runTesters reads a track's tester groups inside a read-only edit.
func runTesters(ctx context.Context, c *Client, args TestersArgs) (TestersResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return TestersResult{}, err
	}
	if args.Track == "" {
		return TestersResult{}, fmt.Errorf("track is required — pass --track internal (testers are per track)")
	}
	var testers apiTesters
	_, err = c.withEdit(ctx, pkg, false, commitOptions{}, func(e *editSession) error {
		return c.do(ctx, http.MethodGet, e.path("testers/"+args.Track), nil, nil, &testers)
	})
	if err != nil {
		return TestersResult{}, toolError("testers", err)
	}
	return TestersResult{PackageName: pkg, Track: args.Track, GoogleGroups: testers.GoogleGroups}, nil
}

// --- CLI front-end ---

var (
	testersArgs   TestersArgs
	testersFormat string
)

// testersCmd reads the tester groups and parents `testers set`.
var testersCmd = &cobra.Command{
	Use:         "testers",
	Short:       "Show the Google Groups testing a track",
	Annotations: mcpTool("testers"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, testersArgs, testersFormat, runTesters)
	},
}

func init() {
	addPackageFlag(testersCmd, &testersArgs.PackageName)
	testersCmd.Flags().StringVar(&testersArgs.Track, "track", "", "testing track (required)")
	addFormatFlag(testersCmd, &testersFormat)
}
