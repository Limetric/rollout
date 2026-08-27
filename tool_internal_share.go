package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Internal app sharing: upload a build and get a link people can install it
// from, without a track, a release, or a review.

// InternalShareArgs uploads an artifact for internal app sharing.
type InternalShareArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	File        string `json:"file" jsonschema:"path to the .aab or .apk to share"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runInternalShare stages or applies an internal app sharing upload.
//
// This is the one upload that never reaches a track: Play stores the artifact
// and returns a link, and anyone with access to the app in Play Console (plus
// the testers the Console's internal-sharing settings allow) can install from
// it. It still previews first, because it creates a downloadable copy of an
// unreleased build that this API cannot delete afterwards.
func runInternalShare(ctx context.Context, c *Client, args InternalShareArgs) (WriteResult, error) {
	const tool = "internal_share"
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
	// The hash identifies the artifact in the preview and is what the apply
	// checks, so a rebuild between preview and confirm cannot share a build
	// nobody looked at.
	sum, err := fileSHA256(path)
	if err != nil {
		return WriteResult{}, err
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchInternalShare,
		Summary: fmt.Sprintf("Share %s (%s, %s, sha256 %s) internally for %s\nIt joins no track and ships to nobody, but the link it returns installs this build — and the Publisher API cannot withdraw it.",
			filepath.Base(path), kind, humanBytes(info.Size()), sum[:12], pkg),
		Payload: internalSharePayload{
			FilePath: path, ContentType: contentType, SHA256: sum, Kind: kind,
		},
	})
}

// --- CLI front-end ---

var internalShareArgs InternalShareArgs

var internalShareCmd = &cobra.Command{
	Use:         "internal-share",
	Short:       "Upload a build for internal app sharing and get its install link (previews first; --confirm to apply)",
	Annotations: mcpTool("internal_share"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, internalShareArgs, runInternalShare)
	},
}

func init() {
	addPackageFlag(internalShareCmd, &internalShareArgs.PackageName)
	internalShareCmd.Flags().StringVar(&internalShareArgs.File, "file", "", "path to the .aab or .apk (required)")
	addConfirmFlag(internalShareCmd, &internalShareArgs.Confirm)
}
