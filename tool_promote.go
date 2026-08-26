package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// Promotion moves an already-tested build up a track, which is the one release
// operation that reads one track and writes another.

// PromoteReleaseArgs promotes a release from one track to another.
type PromoteReleaseArgs struct {
	PackageName  string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	FromTrack    string   `json:"from_track" jsonschema:"the track to promote from, for example internal"`
	ToTrack      string   `json:"to_track" jsonschema:"the track to promote to, for example beta"`
	VersionCodes []string `json:"version_codes,omitempty" jsonschema:"the version codes to promote; omit to promote the highest version code on the source track"`
	Status       string   `json:"status,omitempty" jsonschema:"status on the destination track; defaults to draft on production and completed elsewhere"`
	Rollout      *float64 `json:"rollout,omitempty" jsonschema:"staged-rollout fraction between 0 and 1 exclusive; required when status is inProgress"`
	Notes        []string `json:"notes,omitempty" jsonschema:"replacement release notes as locale=text pairs; omit to carry the source release's notes across"`
	NotesDir     string   `json:"notes_dir,omitempty" jsonschema:"a directory of replacement release notes"`

	RemoveOtherDrafts       bool   `json:"remove_other_drafts,omitempty" jsonschema:"drop other draft releases on the destination track"`
	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runPromoteRelease stages or applies a promotion.
//
// The source release is read at preview time and copied wholesale — name,
// release notes, in-app update priority — because a promotion is meant to ship
// the thing that was tested, not a fresh release that happens to share a
// version code. Only status and rollout are decided anew.
func runPromoteRelease(ctx context.Context, c *Client, args PromoteReleaseArgs) (WriteResult, error) {
	const tool = "promote_release"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.FromTrack == "" || args.ToTrack == "" {
		return WriteResult{}, fmt.Errorf("both tracks are required — pass --from internal --to beta")
	}
	if args.FromTrack == args.ToTrack {
		return WriteResult{}, fmt.Errorf("--from and --to are both %q — a promotion moves a release between tracks", args.FromTrack)
	}

	var codes []string
	if len(args.VersionCodes) > 0 {
		if codes, err = parseVersionCodes(args.VersionCodes); err != nil {
			return WriteResult{}, err
		}
	}

	source, err := c.findPromotableRelease(ctx, pkg, args.FromTrack, codes)
	if err != nil {
		return WriteResult{}, toolError(tool, err)
	}

	// Promoting to production defaults to draft, everywhere else to completed.
	// A testing track exists to be shipped to; production is the one where an
	// unreviewed automatic rollout is the wrong default.
	status := statusCompleted
	if args.ToTrack == productionTrack {
		status = statusDraft
	}
	if args.Status != "" {
		if status, err = parseStatus(args.Status); err != nil {
			return WriteResult{}, err
		}
	}
	if err := validateRolloutForStatus(status, args.Rollout); err != nil {
		return WriteResult{}, err
	}

	release := *source
	release.Status = status
	release.UserFraction = args.Rollout
	if len(args.Notes) > 0 || args.NotesDir != "" {
		notes, err := (releaseFields{Notes: args.Notes, NotesDir: args.NotesDir}).resolveNotes(firstOrEmpty(release.VersionCodes))
		if err != nil {
			return WriteResult{}, err
		}
		release.ReleaseNotes = notes
	}
	// The source release's own notes carry across untouched, but they still
	// have to fit: a note that was fine on a track with no review can fail the
	// commit here for a reason that has nothing to do with the promotion.
	if err := validateReleaseNotes(release.ReleaseNotes); err != nil {
		return WriteResult{}, err
	}

	raw, err := json.Marshal(release)
	if err != nil {
		return WriteResult{}, err
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchTrack,
		Summary: fmt.Sprintf("Promote %s → %s: %s", args.FromTrack, args.ToTrack, describeRelease(release)),
		Track:   args.ToTrack, RolloutFraction: declaredRollout(status, args.Rollout),
		Payload: trackPayload{
			Track: args.ToTrack, Release: raw,
			RemoveOtherDrafts:       args.RemoveOtherDrafts,
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// findPromotableRelease picks the release to promote: the named one, or the
// highest version code on the source track.
func (c *Client) findPromotableRelease(ctx context.Context, pkg, from string, codes []string) (*trackRelease, error) {
	tracks, err := c.readTracks(ctx, pkg, from)
	if err != nil {
		return nil, err
	}
	if len(tracks) == 0 || len(tracks[0].Releases) == 0 {
		return nil, fmt.Errorf("track %s has no releases to promote", from)
	}

	var best *trackRelease
	var bestCode int64
	for _, info := range tracks[0].Releases {
		var release trackRelease
		if err := json.Unmarshal(info.Raw, &release); err != nil {
			continue
		}
		if len(codes) > 0 {
			if sameVersionCodes(release.VersionCodes, codes) {
				return &release, nil
			}
			continue
		}
		// Highest version code wins: "promote what we just tested" means the
		// newest build, and picking whatever the API listed first would
		// promote an old one on a track that still holds it.
		code, ok := parseVersionCode(firstOrEmpty(release.VersionCodes))
		if !ok {
			continue
		}
		if best == nil || code > bestCode {
			copied := release
			best, bestCode = &copied, code
		}
	}
	if best == nil {
		if len(codes) > 0 {
			return nil, fmt.Errorf("no release with version code %s on track %s", firstOrEmpty(codes), from)
		}
		return nil, fmt.Errorf("track %s has no release with a usable version code", from)
	}
	return best, nil
}

// --- CLI front-end ---

var (
	promoteArgs    PromoteReleaseArgs
	promoteRollout float64
)

var promoteCmd = &cobra.Command{
	Use:         "promote",
	Short:       "Promote a release from one track to another (previews first; --confirm to apply)",
	Annotations: mcpTool("promote_release"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("rollout") {
			promoteArgs.Rollout = &promoteRollout
		}
		return runPlayWrite(cmd, promoteArgs, runPromoteRelease)
	},
}

func init() {
	addPackageFlag(promoteCmd, &promoteArgs.PackageName)
	promoteCmd.Flags().StringVar(&promoteArgs.FromTrack, "from", "", "track to promote from (required)")
	promoteCmd.Flags().StringVar(&promoteArgs.ToTrack, "to", "", "track to promote to (required)")
	promoteCmd.Flags().StringArrayVar(&promoteArgs.VersionCodes, "version-codes", nil, "version codes to promote (default: the highest on the source track)")
	promoteCmd.Flags().StringVar(&promoteArgs.Status, "status", "", "status on the destination track")
	promoteCmd.Flags().Float64Var(&promoteRollout, "rollout", 0, "staged-rollout fraction between 0 and 1 (exclusive)")
	promoteCmd.Flags().StringArrayVar(&promoteArgs.Notes, "notes", nil, "replacement release notes as <locale>=<text> (repeatable)")
	promoteCmd.Flags().StringVar(&promoteArgs.NotesDir, "notes-dir", "", "directory of replacement release notes")
	promoteCmd.Flags().BoolVar(&promoteArgs.RemoveOtherDrafts, "remove-other-drafts", false, "drop other draft releases on the destination track")
	promoteCmd.Flags().BoolVar(&promoteArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(promoteCmd, &promoteArgs.Confirm)
}
