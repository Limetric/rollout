package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/spf13/cobra"
)

// DetailsArgs reads the app-level details: default language and contact info.
type DetailsArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
}

// AppDetails is the API's AppDetails resource.
type AppDetails struct {
	DefaultLanguage string `json:"default_language,omitempty"`
	ContactWebsite  string `json:"contact_website,omitempty"`
	ContactEmail    string `json:"contact_email,omitempty"`
	ContactPhone    string `json:"contact_phone,omitempty"`
}

// DetailsResult is the app's details.
type DetailsResult struct {
	PackageName string     `json:"package_name"`
	Details     AppDetails `json:"details"`
}

func (r DetailsResult) tableRows() ([]json.RawMessage, []string) {
	return []json.RawMessage{jsonRow(r.Details)}, []string{"default_language", "contact_email", "contact_website", "contact_phone"}
}

// apiAppDetails is the wire shape.
type apiAppDetails struct {
	DefaultLanguage string `json:"defaultLanguage"`
	ContactWebsite  string `json:"contactWebsite"`
	ContactEmail    string `json:"contactEmail"`
	ContactPhone    string `json:"contactPhone"`
}

// normalize converts the wire shape to the output shape; see apiListing.
func (d apiAppDetails) normalize() AppDetails { return AppDetails(d) }

// runDetails reads the app details inside a read-only edit.
func runDetails(ctx context.Context, c *Client, args DetailsArgs) (DetailsResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return DetailsResult{}, err
	}
	var details apiAppDetails
	_, err = c.withEdit(ctx, pkg, false, commitOptions{}, func(e *editSession) error {
		return c.do(ctx, http.MethodGet, e.path("details"), nil, nil, &details)
	})
	if err != nil {
		return DetailsResult{}, toolError("details", err)
	}
	return DetailsResult{PackageName: pkg, Details: details.normalize()}, nil
}

// --- CLI front-end ---

var (
	detailsArgs   DetailsArgs
	detailsFormat string
)

// detailsCmd reads the details and parents `details set`.
var detailsCmd = &cobra.Command{
	Use:         "details",
	Short:       "Show app details (default language, contact info)",
	Annotations: mcpTool("details"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, detailsArgs, detailsFormat, runDetails)
	},
}

func init() {
	addPackageFlag(detailsCmd, &detailsArgs.PackageName)
	addFormatFlag(detailsCmd, &detailsFormat)
}
