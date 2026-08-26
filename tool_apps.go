package main

import (
	"context"
	"encoding/json"

	"github.com/spf13/cobra"
)

// AppsArgs lists the apps this credential can reach.
type AppsArgs struct{}

// AppInfo is one app the credential can see.
type AppInfo struct {
	PackageName string `json:"package_name"`
	DisplayName string `json:"display_name,omitempty"`
	// Default marks the app every other tool falls back to when --package is
	// omitted, so an agent listing apps can tell which one it is already
	// pointed at rather than guessing from configuration it cannot read.
	Default bool `json:"default,omitempty"`
}

// AppsResult lists the reachable apps.
type AppsResult struct {
	Apps []AppInfo `json:"apps"`
	// Message explains a degraded result — most often a credential that can
	// publish but cannot read the Reporting API, where the configured app is
	// all we can honestly report.
	Message string `json:"message,omitempty"`
}

func (r AppsResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Apps), []string{"package_name", "display_name", "default"}
}

// runApps lists the apps the credential can see.
//
// The Publisher API has no "list my apps" call at all, so this goes through the
// Reporting API's apps:search — the only enumeration either API offers. That
// makes it the one tool whose failure is routine rather than exceptional: a
// service account granted release permissions but not "View app information"
// publishes fine and cannot list. Reporting the configured app and saying why
// is more useful than failing the first call an agent makes.
func runApps(ctx context.Context, c *Client, _ AppsArgs) (AppsResult, error) {
	configured := c.cfg.PackageName

	apps, err := c.searchApps(ctx)
	if err != nil {
		if configured == "" {
			return AppsResult{}, toolError("apps", reportingPermissionHint(err))
		}
		return AppsResult{
			Apps:    []AppInfo{{PackageName: configured, Default: true}},
			Message: "could not list apps (" + reportingPermissionHint(err).Error() + ") — showing the configured app only",
		}, nil
	}

	out := AppsResult{Apps: make([]AppInfo, 0, len(apps))}
	for _, app := range apps {
		out.Apps = append(out.Apps, AppInfo{
			PackageName: app.PackageName,
			DisplayName: app.DisplayName,
			Default:     app.PackageName == configured,
		})
	}
	// The default app goes first: it is the one an agent will act on, and
	// burying it in an alphabetical list of forty invites the wrong choice.
	for i, app := range out.Apps {
		if app.Default && i > 0 {
			out.Apps[0], out.Apps[i] = out.Apps[i], out.Apps[0]
			break
		}
	}
	if configured != "" && !containsPackage(out.Apps, configured) {
		out.Apps = append([]AppInfo{{PackageName: configured, Default: true}}, out.Apps...)
		out.Message = "the configured default app is not visible to the Reporting API — check the package name, or grant \"View app information\" to this credential"
	}
	return out, nil
}

func containsPackage(apps []AppInfo, pkg string) bool {
	for _, app := range apps {
		if app.PackageName == pkg {
			return true
		}
	}
	return false
}

// --- CLI front-end ---

var appsFormat string

var appsCmd = &cobra.Command{
	Use:         "apps",
	Short:       "List the apps these credentials can reach",
	Annotations: mcpTool("apps"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, AppsArgs{}, appsFormat, runApps)
	},
}

func init() {
	addFormatFlag(appsCmd, &appsFormat)
}
