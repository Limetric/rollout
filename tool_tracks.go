package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/spf13/cobra"
)

// TracksArgs reads the release tracks of an app.
type TracksArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track       string `json:"track,omitempty" jsonschema:"narrow to one track (production, beta, alpha, internal, or a custom closed-testing track); omit for all tracks"`
}

// TrackInfo is one track with its releases, as the API returned it plus the
// handful of fields worth reading at a glance.
type TrackInfo struct {
	Track    string        `json:"track"`
	Releases []ReleaseInfo `json:"releases"`
}

// ReleaseInfo is one release in a track. Raw carries the API's own object so an
// agent sees the wire format; the named fields are what a human scans.
type ReleaseInfo struct {
	Track        string   `json:"track"`
	Name         string   `json:"name,omitempty"`
	Status       string   `json:"status,omitempty"`
	VersionCodes []string `json:"version_codes,omitempty"`
	// RolloutPercent is userFraction as a percentage, because that is the unit
	// the Console shows and the unit people say out loud. It is absent unless
	// the release is actually a staged rollout.
	RolloutPercent      *float64        `json:"rollout_percent,omitempty"`
	InAppUpdatePriority int             `json:"in_app_update_priority,omitempty"`
	ReleaseNoteLocales  []string        `json:"release_note_locales,omitempty"`
	Raw                 json.RawMessage `json:"raw,omitempty"`
}

// TracksResult is the whole track listing.
type TracksResult struct {
	PackageName string      `json:"package_name"`
	Tracks      []TrackInfo `json:"tracks"`
}

func (r TracksResult) tableRows() ([]json.RawMessage, []string) {
	var releases []ReleaseInfo
	for _, t := range r.Tracks {
		releases = append(releases, t.Releases...)
	}
	return jsonRows(releases), []string{"track", "status", "version_codes", "rollout_percent", "name"}
}

// apiTrack is the API's Track resource, kept separate from TrackInfo so the raw
// release objects survive into the result untouched.
type apiTrack struct {
	Track    string            `json:"track"`
	Releases []json.RawMessage `json:"releases"`
}

// apiRelease is the subset of Track.Release the normalized fields come from.
type apiRelease struct {
	Name                string   `json:"name"`
	Status              string   `json:"status"`
	VersionCodes        []string `json:"versionCodes"`
	UserFraction        *float64 `json:"userFraction"`
	InAppUpdatePriority int      `json:"inAppUpdatePriority"`
	ReleaseNotes        []struct {
		Language string `json:"language"`
	} `json:"releaseNotes"`
}

// runTracks reads the app's tracks inside a read-only edit.
//
// There is no edit-free way to read a track's releases, so this opens one and
// deletes it on the way out (see withEdit). That is one edit per call by
// design: an edit ID is never persisted, because they expire and opening a new
// one invalidates the last.
func runTracks(ctx context.Context, c *Client, args TracksArgs) (TracksResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return TracksResult{}, err
	}
	tracks, err := c.readTracks(ctx, pkg, args.Track)
	if err != nil {
		return TracksResult{}, toolError("tracks", err)
	}
	return TracksResult{PackageName: pkg, Tracks: tracks}, nil
}

// readTracks fetches one track or all of them inside a single read-only edit.
func (c *Client) readTracks(ctx context.Context, pkg, only string) ([]TrackInfo, error) {
	var raw []apiTrack
	_, err := c.withEdit(ctx, pkg, false, commitOptions{}, func(e *editSession) error {
		if only != "" {
			var t apiTrack
			if err := c.do(ctx, http.MethodGet, e.path("tracks/"+only), nil, nil, &t); err != nil {
				return err
			}
			raw = []apiTrack{t}
			return nil
		}
		var list struct {
			Tracks []apiTrack `json:"tracks"`
		}
		if err := c.do(ctx, http.MethodGet, e.path("tracks"), nil, nil, &list); err != nil {
			return err
		}
		raw = list.Tracks
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]TrackInfo, 0, len(raw))
	for _, t := range raw {
		info := TrackInfo{Track: t.Track, Releases: make([]ReleaseInfo, 0, len(t.Releases))}
		for _, r := range t.Releases {
			info.Releases = append(info.Releases, normalizeRelease(t.Track, r))
		}
		out = append(out, info)
	}
	return out, nil
}

// normalizeRelease adds the readable fields without discarding the wire object.
func normalizeRelease(trackName string, raw json.RawMessage) ReleaseInfo {
	info := ReleaseInfo{Track: trackName, Raw: raw}
	var r apiRelease
	if err := json.Unmarshal(raw, &r); err != nil {
		// A release shape we do not recognize is still worth showing raw; the
		// alternative is dropping a live release from the listing.
		return info
	}
	info.Name, info.Status, info.VersionCodes = r.Name, r.Status, r.VersionCodes
	info.InAppUpdatePriority = r.InAppUpdatePriority
	if r.UserFraction != nil {
		percent := *r.UserFraction * 100
		info.RolloutPercent = &percent
	}
	for _, note := range r.ReleaseNotes {
		info.ReleaseNoteLocales = append(info.ReleaseNoteLocales, note.Language)
	}
	return info
}

// --- CLI front-end ---

var (
	tracksArgs   TracksArgs
	tracksFormat string
)

var tracksCmd = &cobra.Command{
	Use:         "tracks",
	Short:       "Show release tracks and the releases in them",
	Annotations: mcpTool("tracks"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, tracksArgs, tracksFormat, runTracks)
	},
}

func init() {
	addPackageFlag(tracksCmd, &tracksArgs.PackageName)
	tracksCmd.Flags().StringVar(&tracksArgs.Track, "track", "", "narrow to one track (production, beta, alpha, internal, …)")
	addFormatFlag(tracksCmd, &tracksFormat)
}
