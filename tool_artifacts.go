package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/spf13/cobra"
)

// ArtifactsArgs lists the uploaded bundles and APKs of an app.
type ArtifactsArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
}

// ArtifactInfo is one uploaded artifact.
type ArtifactInfo struct {
	VersionCode int64 `json:"version_code"`
	// Type is "bundle" or "apk". The two live behind different endpoints and
	// carry different hash fields, which is exactly the detail a caller should
	// not have to care about.
	Type   string `json:"type"`
	SHA256 string `json:"sha256,omitempty"`
	SHA1   string `json:"sha1,omitempty"`
}

// ArtifactsResult lists every artifact the app has, newest first.
type ArtifactsResult struct {
	PackageName string         `json:"package_name"`
	Artifacts   []ArtifactInfo `json:"artifacts"`
}

func (r ArtifactsResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Artifacts), []string{"version_code", "type", "sha256"}
}

// runArtifacts lists bundles and APKs together, inside one read-only edit.
func runArtifacts(ctx context.Context, c *Client, args ArtifactsArgs) (ArtifactsResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ArtifactsResult{}, err
	}

	var artifacts []ArtifactInfo
	_, err = c.withEdit(ctx, pkg, false, commitOptions{}, func(e *editSession) error {
		var bundles struct {
			Bundles []struct {
				VersionCode int64  `json:"versionCode"`
				SHA256      string `json:"sha256"`
				SHA1        string `json:"sha1"`
			} `json:"bundles"`
		}
		if err := c.do(ctx, http.MethodGet, e.path("bundles"), nil, nil, &bundles); err != nil {
			return err
		}
		for _, b := range bundles.Bundles {
			artifacts = append(artifacts, ArtifactInfo{VersionCode: b.VersionCode, Type: "bundle", SHA256: b.SHA256, SHA1: b.SHA1})
		}

		var apks struct {
			APKs []struct {
				VersionCode int64 `json:"versionCode"`
				Binary      struct {
					SHA256 string `json:"sha256"`
					SHA1   string `json:"sha1"`
				} `json:"binary"`
			} `json:"apks"`
		}
		if err := c.do(ctx, http.MethodGet, e.path("apks"), nil, nil, &apks); err != nil {
			return err
		}
		for _, a := range apks.APKs {
			artifacts = append(artifacts, ArtifactInfo{VersionCode: a.VersionCode, Type: "apk", SHA256: a.Binary.SHA256, SHA1: a.Binary.SHA1})
		}
		return nil
	})
	if err != nil {
		return ArtifactsResult{}, toolError("artifacts", err)
	}

	// Newest first: the version code someone is about to release is almost
	// always the highest one, and it should not be at the bottom of a list of
	// two hundred.
	sort.SliceStable(artifacts, func(i, j int) bool {
		return artifacts[i].VersionCode > artifacts[j].VersionCode
	})
	return ArtifactsResult{PackageName: pkg, Artifacts: artifacts}, nil
}

// --- CLI front-end ---

var (
	artifactsArgs   ArtifactsArgs
	artifactsFormat string
)

var artifactsCmd = &cobra.Command{
	Use:         "artifacts",
	Short:       "List uploaded app bundles and APKs",
	Annotations: mcpTool("artifacts"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, artifactsArgs, artifactsFormat, runArtifacts)
	},
}

func init() {
	addPackageFlag(artifactsCmd, &artifactsArgs.PackageName)
	addFormatFlag(artifactsCmd, &artifactsFormat)
}
