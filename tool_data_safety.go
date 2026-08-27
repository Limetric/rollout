package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// The Data safety declaration — the form that becomes the "Data safety" section
// of the store listing.
//
// Play exposes exactly one endpoint for it, and it is a write: the CSV the
// Console exports goes up, and nothing comes back down. There is deliberately
// no `play_data_safety` read here, because there is no API that could answer
// one; the current declaration is visible only in Play Console.

// UpdateDataSafetyArgs replaces an app's Data safety declaration.
type UpdateDataSafetyArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	File        string `json:"file" jsonschema:"path to the Data safety CSV exported from Play Console"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runUpdateDataSafety stages or applies a Data safety declaration.
//
// The whole CSV is staged rather than the path alone, so `rollout confirm` in
// another process sends exactly the document that was previewed.
func runUpdateDataSafety(ctx context.Context, c *Client, args UpdateDataSafetyArgs) (WriteResult, error) {
	const tool = "update_data_safety"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.File == "" {
		return WriteResult{}, fmt.Errorf("file is required — pass --file data-safety.csv (export it from Play Console → App content → Data safety)")
	}

	path := expandHome(args.File)
	data, err := os.ReadFile(path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("read %q: %w", path, err)
	}
	labels := string(data)
	rows, err := countCSVRows(path, labels)
	if err != nil {
		return WriteResult{}, err
	}

	body, err := json.Marshal(map[string]string{"safetyLabels": labels})
	if err != nil {
		return WriteResult{}, err
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDirect,
		Summary: fmt.Sprintf("Replace the Data safety declaration of %s with %s (%s, %s)\nPlay has no read endpoint for this form, so what is live now cannot be shown here or restored afterwards — keep the CSV that is currently published.",
			pkg, filepath.Base(path), humanBytes(int64(len(data))), plural(rows, "row")),
		Payload: editPayload{Requests: []editRequest{{
			Method: http.MethodPost, Path: "applications/" + pkg + "/dataSafety", Body: body,
			Describe: "data safety declaration",
		}}},
	})
}

// countCSVRows checks the file really is the CSV the endpoint expects and
// reports its size in rows.
//
// The API takes the whole document as one string and rejects a malformed one
// with a message about the field it was handed, not about the line that broke —
// so an unparseable file is caught here, where the error can name it.
func countCSVRows(path, content string) (int, error) {
	if strings.TrimSpace(content) == "" {
		return 0, fmt.Errorf("%s is empty — Play expects the Data safety CSV exported from Play Console", path)
	}
	reader := csv.NewReader(strings.NewReader(content))
	// The export is not rectangular: answer rows carry more columns than the
	// questions above them, and rejecting that would refuse every real file.
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return 0, fmt.Errorf("%s is not readable as CSV: %w — Play expects the file exported from Play Console → App content → Data safety", path, err)
	}
	return len(records), nil
}

// --- CLI front-end ---

var updateDataSafetyArgs UpdateDataSafetyArgs

var dataSafetyCmd = &cobra.Command{
	Use:   "data-safety",
	Short: "Manage the Data safety declaration",
}

var dataSafetySetCmd = &cobra.Command{
	Use:         "set",
	Short:       "Replace the Data safety declaration from a CSV (previews first; --confirm to apply)",
	Annotations: mcpTool("update_data_safety"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, updateDataSafetyArgs, runUpdateDataSafety)
	},
}

func init() {
	addPackageFlag(dataSafetySetCmd, &updateDataSafetyArgs.PackageName)
	dataSafetySetCmd.Flags().StringVar(&updateDataSafetyArgs.File, "file", "", "path to the Data safety CSV (required)")
	addConfirmFlag(dataSafetySetCmd, &updateDataSafetyArgs.Confirm)
	dataSafetyCmd.AddCommand(dataSafetySetCmd)
}
