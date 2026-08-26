package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// The release lifecycle: create a release from uploaded artifacts, turn the
// staged-rollout dial, and halt, resume, or complete it. Every one of these is
// a patch or an insert into exactly one release of one track — see
// applyTrackWrite for why the whole track is never rewritten from arguments.

// releaseFields are the arguments shared by create and update. They map 1:1
// onto Track.Release.
type releaseFields struct {
	Status              string   `json:"status,omitempty" jsonschema:"draft, inProgress, completed, or halted"`
	Rollout             *float64 `json:"rollout,omitempty" jsonschema:"staged-rollout fraction between 0 and 1 exclusive; required for inProgress, and not allowed for draft or completed"`
	ReleaseName         string   `json:"release_name,omitempty" jsonschema:"the release name shown in the Play Console"`
	InAppUpdatePriority int      `json:"in_app_update_priority,omitempty" jsonschema:"in-app update priority from 0 (default) to 5 (most urgent)"`
	Notes               []string `json:"notes,omitempty" jsonschema:"release notes as locale=text pairs, for example en-US=Bug fixes"`
	NotesDir            string   `json:"notes_dir,omitempty" jsonschema:"a directory of release notes: <locale>.txt, or the fastlane layout <locale>/changelogs/<versionCode>.txt"`
}

// resolveNotes turns the note arguments into release notes, enforcing Play's
// per-locale limit before anything is staged.
func (f releaseFields) resolveNotes(versionCode string) ([]releaseNote, error) {
	if len(f.Notes) > 0 && f.NotesDir != "" {
		return nil, fmt.Errorf("pass either --notes or --notes-dir, not both")
	}
	var notes []releaseNote
	var err error
	switch {
	case f.NotesDir != "":
		notes, err = readReleaseNotesDir(f.NotesDir, versionCode)
	case len(f.Notes) > 0:
		notes, err = parseReleaseNotes(f.Notes)
	}
	if err != nil {
		return nil, err
	}
	if err := validateReleaseNotes(notes); err != nil {
		return nil, err
	}
	return notes, nil
}

// CreateReleaseArgs creates a track release from already-uploaded artifacts.
type CreateReleaseArgs struct {
	PackageName  string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track        string   `json:"track" jsonschema:"the track to release on (production, beta, alpha, internal, or a custom closed-testing track)"`
	VersionCodes []string `json:"version_codes" jsonschema:"the version codes to release, from play_artifacts"`
	releaseFields
	RemoveOtherDrafts       bool   `json:"remove_other_drafts,omitempty" jsonschema:"drop other draft releases in this track; in-progress and completed releases are never removed"`
	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runCreateRelease stages or applies a new track release.
func runCreateRelease(ctx context.Context, c *Client, args CreateReleaseArgs) (WriteResult, error) {
	const tool = "create_release"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.Track == "" {
		return WriteResult{}, fmt.Errorf("track is required — pass --track internal")
	}
	codes, err := parseVersionCodes(args.VersionCodes)
	if err != nil {
		return WriteResult{}, err
	}
	// Draft by default: an artifact reaching users is a separate decision from
	// an artifact existing on a track.
	status := statusDraft
	if args.Status != "" {
		if status, err = parseStatus(args.Status); err != nil {
			return WriteResult{}, err
		}
	}
	if err := validateRolloutForStatus(status, args.Rollout); err != nil {
		return WriteResult{}, err
	}
	notes, err := args.resolveNotes(codes[0])
	if err != nil {
		return WriteResult{}, err
	}

	release := trackRelease{
		Name:                args.ReleaseName,
		VersionCodes:        codes,
		Status:              status,
		UserFraction:        args.Rollout,
		InAppUpdatePriority: args.InAppUpdatePriority,
		ReleaseNotes:        notes,
	}
	raw, err := json.Marshal(release)
	if err != nil {
		return WriteResult{}, err
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchTrack,
		Summary: fmt.Sprintf("Release on %s: %s", args.Track, describeRelease(release)),
		Track:   args.Track, RolloutFraction: declaredRollout(status, args.Rollout),
		Payload: trackPayload{
			Track: args.Track, Release: raw,
			RemoveOtherDrafts:       args.RemoveOtherDrafts,
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// UpdateReleaseArgs patches an existing release. This is the staged-rollout
// dial.
type UpdateReleaseArgs struct {
	PackageName  string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track        string   `json:"track" jsonschema:"the track holding the release"`
	VersionCodes []string `json:"version_codes,omitempty" jsonschema:"the version codes identifying the release; omit when the track holds exactly one release"`
	releaseFields
	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runUpdateRelease stages or applies a change to one release.
func runUpdateRelease(ctx context.Context, c *Client, args UpdateReleaseArgs) (WriteResult, error) {
	const tool = "update_release"
	return stageReleasePatch(ctx, c, tool, releasePatchRequest{
		PackageName:  args.PackageName,
		Track:        args.Track,
		VersionCodes: args.VersionCodes,
		Fields:       args.releaseFields,
		Confirm:      args.Confirm,
		NoReview:     args.ChangesNotSentForReview,
	})
}

// releasePatchRequest is what the update-shaped tools share. halt, resume, and
// complete are the same operation with the status decided for you.
type releasePatchRequest struct {
	PackageName  string
	Track        string
	VersionCodes []string
	Fields       releaseFields
	Confirm      string
	NoReview     bool
	// ForcedStatus is set by the sugar commands.
	ForcedStatus string
	// Verb names the operation in the preview ("Halt", "Complete").
	Verb string
}

// stageReleasePatch is the shared body of update/halt/resume/complete.
//
// It reads the track during the *preview* so the summary can describe the
// release as it is now, and stages only the fields being changed. The merge
// happens at apply time against the current release, so a token confirmed
// minutes later turns the dial rather than reverting anything edited in
// between.
func stageReleasePatch(ctx context.Context, c *Client, tool string, req releasePatchRequest) (WriteResult, error) {
	pkg, err := c.resolvePackage(req.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if req.Confirm != "" {
		return applyConfirmed(ctx, c, tool, req.Confirm)
	}
	if req.Track == "" {
		return WriteResult{}, fmt.Errorf("track is required — pass --track production")
	}

	var codes []string
	if len(req.VersionCodes) > 0 {
		if codes, err = parseVersionCodes(req.VersionCodes); err != nil {
			return WriteResult{}, err
		}
	}

	status := req.ForcedStatus
	if status == "" && req.Fields.Status != "" {
		if status, err = parseStatus(req.Fields.Status); err != nil {
			return WriteResult{}, err
		}
	}
	rollout := req.Fields.Rollout
	// `--rollout 1` is what people type when they mean "ship it to everyone",
	// and the API rejects it. Translate rather than refuse: the intent is
	// unambiguous, and a completed release is exactly what they asked for.
	if rollout != nil && *rollout >= 1 && status == "" {
		status, rollout = statusCompleted, nil
	}
	// A patch that names a fraction has to pair it with the status correctly.
	// A patch that only names a status does not: halting and resuming leave
	// the release's existing fraction in place, and demanding one here would
	// make `halt` require a number nobody is changing.
	if rollout != nil {
		effective := status
		if effective == "" {
			effective = statusInProgress
		}
		if err := validateRolloutForStatus(effective, rollout); err != nil {
			return WriteResult{}, err
		}
	}

	notes, err := req.Fields.resolveNotes(firstOrEmpty(codes))
	if err != nil {
		return WriteResult{}, err
	}

	patch := map[string]any{}
	var remove []string
	if status != "" {
		patch["status"] = status
	}
	if rollout != nil {
		patch["userFraction"] = *rollout
	}
	// A completed release must not carry a fraction; the API rejects the pair.
	if status == statusCompleted {
		remove = append(remove, "userFraction")
		delete(patch, "userFraction")
	}
	if req.Fields.ReleaseName != "" {
		patch["name"] = req.Fields.ReleaseName
	}
	if req.Fields.InAppUpdatePriority != 0 {
		patch["inAppUpdatePriority"] = req.Fields.InAppUpdatePriority
	}
	if len(notes) > 0 {
		patch["releaseNotes"] = notes
	}
	if len(patch) == 0 && len(remove) == 0 {
		return WriteResult{}, fmt.Errorf("nothing to change — pass --rollout, --status, --notes, --release-name, or --priority")
	}
	rawPatch, err := json.Marshal(patch)
	if err != nil {
		return WriteResult{}, err
	}

	current := describeCurrentRelease(ctx, c, pkg, req.Track, codes)
	verb := req.Verb
	if verb == "" {
		verb = "Update"
	}
	summary := fmt.Sprintf("%s the %s release (%s) — %s", verb, req.Track, current, describePatch(status, rollout, req.Fields, remove))

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchTrack,
		Summary: summary, Track: req.Track, RolloutFraction: declaredRollout(status, rollout),
		Payload: trackPayload{
			Track: req.Track, MatchVersionCodes: codes,
			Patch: rawPatch, PatchRemove: remove,
			ChangesNotSentForReview: req.NoReview,
		},
	})
}

// describeCurrentRelease reads the track so the preview can say what is being
// changed. Best-effort: a preview that cannot read the track still stages, and
// the apply will report the real problem.
func describeCurrentRelease(ctx context.Context, c *Client, pkg, trackName string, codes []string) string {
	tracks, err := c.readTracks(ctx, pkg, trackName)
	if err != nil || len(tracks) == 0 {
		return "current state unknown"
	}
	releases := tracks[0].Releases
	for _, r := range releases {
		if len(codes) == 0 || sameVersionCodes(r.VersionCodes, codes) {
			var release trackRelease
			if json.Unmarshal(r.Raw, &release) == nil {
				return "currently " + describeRelease(release)
			}
		}
	}
	if len(codes) > 0 {
		return "version " + strings.Join(codes, "+") + " is not currently in this track"
	}
	return "no releases in this track"
}

// describePatch renders the change for the preview line.
func describePatch(status string, rollout *float64, fields releaseFields, remove []string) string {
	var parts []string
	if status != "" {
		parts = append(parts, "status "+status)
	}
	if rollout != nil {
		parts = append(parts, "rollout "+formatFraction(*rollout))
	}
	for _, field := range remove {
		if field == "userFraction" {
			parts = append(parts, "reaching every user")
		}
	}
	if fields.ReleaseName != "" {
		parts = append(parts, "name "+fields.ReleaseName)
	}
	if fields.InAppUpdatePriority != 0 {
		parts = append(parts, fmt.Sprintf("in-app update priority %d", fields.InAppUpdatePriority))
	}
	if len(fields.Notes) > 0 || fields.NotesDir != "" {
		parts = append(parts, "new release notes")
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return "setting " + strings.Join(parts, ", ")
}

func firstOrEmpty(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

// --- halt / resume / complete ---

// ReleaseStatusArgs is the argument shape of the three status-only writes.
type ReleaseStatusArgs struct {
	PackageName             string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track                   string   `json:"track" jsonschema:"the track holding the release"`
	VersionCodes            []string `json:"version_codes,omitempty" jsonschema:"the version codes identifying the release; omit when the track holds exactly one release"`
	Rollout                 *float64 `json:"rollout,omitempty" jsonschema:"the fraction to resume at, between 0 and 1 exclusive; omit to keep the current fraction"`
	ChangesNotSentForReview bool     `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string   `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

func (a ReleaseStatusArgs) request(status, verb string) releasePatchRequest {
	return releasePatchRequest{
		PackageName: a.PackageName, Track: a.Track, VersionCodes: a.VersionCodes,
		Fields: releaseFields{Rollout: a.Rollout}, Confirm: a.Confirm,
		NoReview: a.ChangesNotSentForReview, ForcedStatus: status, Verb: verb,
	}
}

// runHaltRelease stops a staged rollout. It takes two confirmations: users who
// already have the release keep it, and the ones who do not will not get it —
// resuming later does not undo the hours it was gone.
func runHaltRelease(ctx context.Context, c *Client, args ReleaseStatusArgs) (WriteResult, error) {
	return stageReleasePatch(ctx, c, "halt_release", args.request(statusHalted, "Halt"))
}

// runResumeRelease restarts a halted rollout.
func runResumeRelease(ctx context.Context, c *Client, args ReleaseStatusArgs) (WriteResult, error) {
	return stageReleasePatch(ctx, c, "resume_release", args.request(statusInProgress, "Resume"))
}

// runCompleteRelease rolls a release out to every user. On production this
// takes two confirmations: there is no lower fraction to fall back to.
func runCompleteRelease(ctx context.Context, c *Client, args ReleaseStatusArgs) (WriteResult, error) {
	return stageReleasePatch(ctx, c, "complete_release", args.request(statusCompleted, "Complete"))
}

// --- CLI front-end ---

var (
	createReleaseArgs   CreateReleaseArgs
	updateReleaseArgs   UpdateReleaseArgs
	haltReleaseArgs     ReleaseStatusArgs
	resumeReleaseArgs   ReleaseStatusArgs
	completeReleaseArgs ReleaseStatusArgs
	createRollout       float64
	updateRollout       float64
	resumeRollout       float64
)

var releaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Create and drive track releases",
}

var createReleaseCmd = &cobra.Command{
	Use:         "create",
	Short:       "Create a track release from uploaded version codes (previews first; --confirm to apply)",
	Annotations: mcpTool("create_release"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("rollout") {
			createReleaseArgs.Rollout = &createRollout
		}
		return runPlayWrite(cmd, createReleaseArgs, runCreateRelease)
	},
}

var updateReleaseCmd = &cobra.Command{
	Use:         "update",
	Short:       "Change a release — the staged-rollout dial (previews first; --confirm to apply)",
	Annotations: mcpTool("update_release"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("rollout") {
			updateReleaseArgs.Rollout = &updateRollout
		}
		return runPlayWrite(cmd, updateReleaseArgs, runUpdateRelease)
	},
}

var haltReleaseCmd = &cobra.Command{
	Use:         "halt",
	Short:       "Halt a staged rollout (destructive: takes two confirmations)",
	Annotations: mcpTool("halt_release"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, haltReleaseArgs, runHaltRelease)
	},
}

var resumeReleaseCmd = &cobra.Command{
	Use:         "resume",
	Short:       "Resume a halted rollout (previews first; --confirm to apply)",
	Annotations: mcpTool("resume_release"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("rollout") {
			resumeReleaseArgs.Rollout = &resumeRollout
		}
		return runPlayWrite(cmd, resumeReleaseArgs, runResumeRelease)
	},
}

var completeReleaseCmd = &cobra.Command{
	Use:         "complete",
	Short:       "Roll a release out to every user (on production: takes two confirmations)",
	Annotations: mcpTool("complete_release"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, completeReleaseArgs, runCompleteRelease)
	},
}

// addReleaseFieldFlags registers the flags create and update share.
func addReleaseFieldFlags(cmd *cobra.Command, fields *releaseFields, rollout *float64) {
	cmd.Flags().StringVar(&fields.Status, "status", "", "draft, inProgress, completed, or halted")
	cmd.Flags().Float64Var(rollout, "rollout", 0, "staged-rollout fraction between 0 and 1 (exclusive)")
	cmd.Flags().StringVar(&fields.ReleaseName, "release-name", "", "release name shown in the Play Console")
	cmd.Flags().IntVar(&fields.InAppUpdatePriority, "priority", 0, "in-app update priority, 0 (default) to 5 (most urgent)")
	cmd.Flags().StringArrayVar(&fields.Notes, "notes", nil, "release notes as <locale>=<text> (repeatable)")
	cmd.Flags().StringVar(&fields.NotesDir, "notes-dir", "", "directory of release notes (<locale>.txt, or the fastlane layout)")
}

// addStatusReleaseFlags registers the flags halt/resume/complete share.
func addStatusReleaseFlags(cmd *cobra.Command, args *ReleaseStatusArgs) {
	addPackageFlag(cmd, &args.PackageName)
	cmd.Flags().StringVar(&args.Track, "track", "", "track holding the release (required)")
	cmd.Flags().StringArrayVar(&args.VersionCodes, "version-codes", nil, "version codes identifying the release")
	cmd.Flags().BoolVar(&args.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(cmd, &args.Confirm)
}

func init() {
	addPackageFlag(createReleaseCmd, &createReleaseArgs.PackageName)
	createReleaseCmd.Flags().StringVar(&createReleaseArgs.Track, "track", "", "track to release on (required)")
	createReleaseCmd.Flags().StringArrayVar(&createReleaseArgs.VersionCodes, "version-codes", nil, "version codes to release (required)")
	createReleaseCmd.Flags().BoolVar(&createReleaseArgs.RemoveOtherDrafts, "remove-other-drafts", false, "drop other draft releases in this track")
	createReleaseCmd.Flags().BoolVar(&createReleaseArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addReleaseFieldFlags(createReleaseCmd, &createReleaseArgs.releaseFields, &createRollout)
	addConfirmFlag(createReleaseCmd, &createReleaseArgs.Confirm)

	addPackageFlag(updateReleaseCmd, &updateReleaseArgs.PackageName)
	updateReleaseCmd.Flags().StringVar(&updateReleaseArgs.Track, "track", "", "track holding the release (required)")
	updateReleaseCmd.Flags().StringArrayVar(&updateReleaseArgs.VersionCodes, "version-codes", nil, "version codes identifying the release")
	updateReleaseCmd.Flags().BoolVar(&updateReleaseArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addReleaseFieldFlags(updateReleaseCmd, &updateReleaseArgs.releaseFields, &updateRollout)
	addConfirmFlag(updateReleaseCmd, &updateReleaseArgs.Confirm)

	addStatusReleaseFlags(haltReleaseCmd, &haltReleaseArgs)
	addStatusReleaseFlags(resumeReleaseCmd, &resumeReleaseArgs)
	resumeReleaseCmd.Flags().Float64Var(&resumeRollout, "rollout", 0, "fraction to resume at (default: keep the current one)")
	addStatusReleaseFlags(completeReleaseCmd, &completeReleaseArgs)

	releaseCmd.AddCommand(createReleaseCmd, updateReleaseCmd, haltReleaseCmd, resumeReleaseCmd, completeReleaseCmd)
}

// declaredRollout is the fraction the guard rails should evaluate.
//
// A completed release carries no userFraction — the API rejects the pair — but
// it reaches every user, which is precisely what a rollout cap exists to stop.
// Reporting it as "no fraction" would let `max_rollout_fraction = 0.2` block
// dialling to 50% while waving through the full release.
func declaredRollout(status string, rollout *float64) *float64 {
	if status == statusCompleted {
		full := 1.0
		return &full
	}
	return rollout
}
