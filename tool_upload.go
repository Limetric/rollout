package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Artifact uploads. The upload and the release that carries it happen in one
// edit, because an artifact uploaded into an edit that is never committed does
// not exist.

// UploadArtifactArgs uploads an app bundle or APK.
type UploadArtifactArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	File        string `json:"file" jsonschema:"path to the .aab or .apk to upload"`
	Track       string `json:"track,omitempty" jsonschema:"the track to add the uploaded artifact to; defaults to internal"`
	releaseFields
	NoRelease               bool   `json:"no_release,omitempty" jsonschema:"upload the artifact without adding it to any track"`
	RemoveOtherDrafts       bool   `json:"remove_other_drafts,omitempty" jsonschema:"drop other draft releases in the target track"`
	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runUploadArtifact stages or applies an artifact upload.
func runUploadArtifact(ctx context.Context, c *Client, args UploadArtifactArgs) (WriteResult, error) {
	const tool = "upload_artifact"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.File == "" {
		return WriteResult{}, fmt.Errorf("file is required — pass --file app.aab")
	}

	path := expandHome(args.File)
	kind, contentType, err := artifactKind(path)
	if err != nil {
		return WriteResult{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("read %q: %w", path, err)
	}
	if info.IsDir() {
		return WriteResult{}, fmt.Errorf("%q is a directory, not an artifact", path)
	}
	// Hashing here does double duty: it identifies the artifact in the preview,
	// and it is what the apply compares against so a rebuild between preview
	// and confirm cannot ship an artifact nobody looked at.
	sum, err := fileSHA256(path)
	if err != nil {
		return WriteResult{}, err
	}

	payload := uploadPayload{
		FilePath: path, ContentType: contentType, SHA256: sum, Kind: kind,
		RemoveOtherDrafts:       args.RemoveOtherDrafts,
		ChangesNotSentForReview: args.ChangesNotSentForReview,
	}
	summary := fmt.Sprintf("Upload %s (%s, %s, sha256 %s)", filepath.Base(path), kind, humanBytes(info.Size()), sum[:12])

	var rollout *float64
	trackName := ""
	if !args.NoRelease {
		// Internal by default: an upload has to land somewhere to be visible in
		// the Console at all, and internal is the track where that costs
		// nothing.
		trackName = args.Track
		if trackName == "" {
			trackName = "internal"
		}
		// Draft by default: uploading a build and shipping it are separate
		// decisions, and conflating them is how an untested artifact reaches
		// users from a one-line command.
		status := statusDraft
		if args.Status != "" {
			if status, err = parseStatus(args.Status); err != nil {
				return WriteResult{}, err
			}
		}
		if err := validateRolloutForStatus(status, args.Rollout); err != nil {
			return WriteResult{}, err
		}
		notes, err := args.resolveNotes("")
		if err != nil {
			return WriteResult{}, err
		}
		rollout = args.Rollout
		payload.Track = trackName
		payload.Release = &trackRelease{
			Name:                args.ReleaseName,
			Status:              status,
			UserFraction:        rollout,
			InAppUpdatePriority: args.InAppUpdatePriority,
			ReleaseNotes:        notes,
		}
		summary += fmt.Sprintf(" and add it to %s as %s", trackName, describeRelease(*payload.Release))
	} else if args.Track != "" {
		return WriteResult{}, fmt.Errorf("--no-release and --track are contradictory — drop one")
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchUpload,
		Summary: summary, Track: trackName, RolloutFraction: rollout,
		Payload: payload,
	})
}

// artifactKind decides which endpoint an artifact goes to, from its extension.
// Sending a bundle to the APK endpoint fails with a message that mentions
// neither the file nor the extension.
func artifactKind(path string) (kind, contentType string, err error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".aab":
		return "bundle", "application/octet-stream", nil
	case ".apk":
		return "apk", "application/octet-stream", nil
	default:
		return "", "", fmt.Errorf("%q is neither a .aab nor a .apk — Play takes app bundles and APKs", path)
	}
}

// humanBytes renders a file size the way a person would say it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// --- deobfuscation files ---

// deobfuscationTypes are the file kinds the API accepts.
var deobfuscationTypes = []string{"proguard", "nativeCode"}

// UploadDeobfuscationArgs attaches a mapping or native-symbol file to a build.
type UploadDeobfuscationArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	VersionCode string `json:"version_code" jsonschema:"the version code the file belongs to; the artifact must already be uploaded"`
	Type        string `json:"type" jsonschema:"proguard for a mapping.txt, or nativeCode for a native debug symbols archive"`
	File        string `json:"file" jsonschema:"path to the mapping or symbols file"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runUploadDeobfuscation stages or applies a deobfuscation-file upload. Without
// it, every crash in Play's vitals is an unreadable stack of obfuscated frames.
func runUploadDeobfuscation(ctx context.Context, c *Client, args UploadDeobfuscationArgs) (WriteResult, error) {
	const tool = "upload_deobfuscation"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.File == "" {
		return WriteResult{}, fmt.Errorf("file is required — pass --file mapping.txt")
	}
	if _, ok := parseVersionCode(args.VersionCode); !ok {
		return WriteResult{}, fmt.Errorf("version_code %q is not a number — pass the version code of an already-uploaded artifact (see `rollout play artifacts`)", args.VersionCode)
	}
	fileType := args.Type
	if fileType == "" {
		fileType = "proguard"
	}
	if !slicesContainsString(deobfuscationTypes, fileType) {
		return WriteResult{}, fmt.Errorf("unknown deobfuscation type %q — expected one of: %s", args.Type, strings.Join(deobfuscationTypes, ", "))
	}

	path := expandHome(args.File)
	info, err := os.Stat(path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("read %q: %w", path, err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return WriteResult{}, err
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDeobfuscation,
		Summary: fmt.Sprintf("Attach %s (%s, %s) to version code %s as the %s file",
			filepath.Base(path), humanBytes(info.Size()), "sha256 "+sum[:12], args.VersionCode, fileType),
		Payload: deobfuscationPayload{
			FilePath: path, SHA256: sum, VersionCode: args.VersionCode, Type: fileType,
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

func slicesContainsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- CLI front-end ---

var (
	uploadArgs        UploadArtifactArgs
	uploadRollout     float64
	deobfuscationArgs UploadDeobfuscationArgs
)

var uploadCmd = &cobra.Command{
	Use:         "upload",
	Short:       "Upload an app bundle or APK and add it to a track (previews first; --confirm to apply)",
	Annotations: mcpTool("upload_artifact"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("rollout") {
			uploadArgs.Rollout = &uploadRollout
		}
		return runPlayWrite(cmd, uploadArgs, runUploadArtifact)
	},
}

var deobfuscationCmd = &cobra.Command{
	Use:   "deobfuscation",
	Short: "Manage mapping and native symbol files",
}

var deobfuscationUploadCmd = &cobra.Command{
	Use:         "upload",
	Short:       "Attach a mapping or native symbols file to a build (previews first; --confirm to apply)",
	Annotations: mcpTool("upload_deobfuscation"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, deobfuscationArgs, runUploadDeobfuscation)
	},
}

func init() {
	addPackageFlag(uploadCmd, &uploadArgs.PackageName)
	uploadCmd.Flags().StringVar(&uploadArgs.File, "file", "", "path to the .aab or .apk (required)")
	uploadCmd.Flags().StringVar(&uploadArgs.Track, "track", "", "track to add the artifact to (default: internal)")
	uploadCmd.Flags().BoolVar(&uploadArgs.NoRelease, "no-release", false, "upload the artifact without adding it to a track")
	uploadCmd.Flags().BoolVar(&uploadArgs.RemoveOtherDrafts, "remove-other-drafts", false, "drop other draft releases in the target track")
	uploadCmd.Flags().BoolVar(&uploadArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addReleaseFieldFlags(uploadCmd, &uploadArgs.releaseFields, &uploadRollout)
	addConfirmFlag(uploadCmd, &uploadArgs.Confirm)

	addPackageFlag(deobfuscationUploadCmd, &deobfuscationArgs.PackageName)
	deobfuscationUploadCmd.Flags().StringVar(&deobfuscationArgs.VersionCode, "version-code", "", "version code the file belongs to (required)")
	deobfuscationUploadCmd.Flags().StringVar(&deobfuscationArgs.Type, "type", "proguard", "proguard or nativeCode")
	deobfuscationUploadCmd.Flags().StringVar(&deobfuscationArgs.File, "file", "", "path to the mapping or symbols file (required)")
	deobfuscationUploadCmd.Flags().BoolVar(&deobfuscationArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(deobfuscationUploadCmd, &deobfuscationArgs.Confirm)
	deobfuscationCmd.AddCommand(deobfuscationUploadCmd)
}
