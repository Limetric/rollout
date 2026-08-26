package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/spf13/cobra"
)

// ReleasesArgs is the flat "what is live where" view.
type ReleasesArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track       string `json:"track,omitempty" jsonschema:"narrow to one track; omit for all tracks"`
}

// ReleasesResult is every release across every track, flattened.
type ReleasesResult struct {
	PackageName string        `json:"package_name"`
	Releases    []ReleaseInfo `json:"releases"`
}

func (r ReleasesResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Releases), []string{"track", "status", "version_codes", "rollout_percent", "name"}
}

// runReleases answers "what is live where" without making the caller walk a
// nested track structure.
//
// It prefers the edit-free listing, which costs nothing against the edit quota,
// and falls back to reading the tracks inside an edit when that endpoint is not
// available for this app. Both produce the same answer; the difference is only
// what it costs.
func runReleases(ctx context.Context, c *Client, args ReleasesArgs) (ReleasesResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ReleasesResult{}, err
	}

	tracks, err := c.readTracksWithoutEdit(ctx, pkg)
	if err != nil {
		if !isNotFound(err) {
			return ReleasesResult{}, toolError("releases", err)
		}
		if tracks, err = c.readTracks(ctx, pkg, ""); err != nil {
			return ReleasesResult{}, toolError("releases", err)
		}
	}

	out := ReleasesResult{PackageName: pkg}
	for _, t := range tracks {
		if args.Track != "" && t.Track != args.Track {
			continue
		}
		out.Releases = append(out.Releases, t.Releases...)
	}
	return out, nil
}

// readTracksWithoutEdit reads the tracks through the edit-free endpoint.
func (c *Client) readTracksWithoutEdit(ctx context.Context, pkg string) ([]TrackInfo, error) {
	var list struct {
		Tracks []apiTrack `json:"tracks"`
	}
	if err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/tracks", nil, nil, &list); err != nil {
		return nil, err
	}
	out := make([]TrackInfo, 0, len(list.Tracks))
	for _, t := range list.Tracks {
		info := TrackInfo{Track: t.Track, Releases: make([]ReleaseInfo, 0, len(t.Releases))}
		for _, r := range t.Releases {
			info.Releases = append(info.Releases, normalizeRelease(t.Track, r))
		}
		out = append(out, info)
	}
	return out, nil
}

// --- CLI front-end ---

var (
	releasesArgs   ReleasesArgs
	releasesFormat string
)

var releasesCmd = &cobra.Command{
	Use:         "releases",
	Short:       "List every release across tracks (what is live where)",
	Annotations: mcpTool("releases"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, releasesArgs, releasesFormat, runReleases)
	},
}

func init() {
	addPackageFlag(releasesCmd, &releasesArgs.PackageName)
	releasesCmd.Flags().StringVar(&releasesArgs.Track, "track", "", "narrow to one track")
	addFormatFlag(releasesCmd, &releasesFormat)
}
