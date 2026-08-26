package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Store listing image writes: uploads and deletions.

// UploadImagesArgs uploads one or more images of a single type.
type UploadImagesArgs struct {
	PackageName string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Locale      string   `json:"locale" jsonschema:"the BCP-47 locale the images belong to"`
	Type        string   `json:"type" jsonschema:"the image type (icon, featureGraphic, phoneScreenshots, sevenInchScreenshots, tenInchScreenshots, tvBanner, tvScreenshots, wearScreenshots)"`
	Files       []string `json:"files" jsonschema:"paths to the PNG or JPEG files to upload"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runUploadImages stages or applies an image upload.
//
// The preview reports each file's real dimensions, read from its header, and
// warns where Play's published constraints are not met. Those are warnings
// rather than refusals: the rules move and differ by device type, and blocking
// an upload Play would have accepted is worse than letting the API decide —
// but finding out before the transfer is spent is worth the few bytes.
func runUploadImages(ctx context.Context, c *Client, args UploadImagesArgs) (WriteResult, error) {
	const tool = "upload_images"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.Locale == "" {
		return WriteResult{}, fmt.Errorf("locale is required — pass --locale en-US (images are per locale)")
	}
	if !validImageType(args.Type) {
		return WriteResult{}, imageTypeError(args.Type)
	}
	if len(args.Files) == 0 {
		return WriteResult{}, fmt.Errorf("no files given — pass --file screenshot.png (repeatable)")
	}

	existing, err := runImages(ctx, c, ImagesArgs{PackageName: pkg, Locale: args.Locale, Type: args.Type})
	if err != nil {
		return WriteResult{}, err
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Upload %d image(s) as %s for %s:", len(args.Files), args.Type, args.Locale)
	uploads := make([]imageUpload, 0, len(args.Files))
	for _, file := range args.Files {
		path := expandHome(file)
		meta, err := readImageMeta(path, args.Type)
		if err != nil {
			return WriteResult{}, err
		}
		uploads = append(uploads, imageUpload{Type: args.Type, Path: path, SHA256: meta.SHA256, Warnings: meta.Warnings})
		fmt.Fprintf(&summary, "\n  %s (%s %d×%d, %s)", filepath.Base(path), meta.Format, meta.Width, meta.Height, humanBytes(meta.Bytes))
		for _, warning := range meta.Warnings {
			fmt.Fprintf(&summary, "\n    warning: %s", warning)
		}
	}
	// Uploads add to a type rather than replacing it, so the resulting count is
	// what has to fit Play's limit.
	for _, warning := range countWarnings(args.Type, len(existing.Images)+len(args.Files)) {
		fmt.Fprintf(&summary, "\n  warning: %s", warning)
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchImages, Summary: summary.String(),
		Payload: imagesPayload{
			Locale: args.Locale, Uploads: uploads,
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// DeleteImagesArgs removes store listing images.
type DeleteImagesArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Locale      string `json:"locale" jsonschema:"the BCP-47 locale the images belong to"`
	Type        string `json:"type" jsonschema:"the image type to delete from"`
	ID          string `json:"id,omitempty" jsonschema:"the id of one image to delete, from play_images"`
	All         bool   `json:"all,omitempty" jsonschema:"delete every image of this type in this locale"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runDeleteImages stages or applies an image deletion. Deleting every image of
// a type takes two confirmations: the originals are frequently nowhere else.
func runDeleteImages(ctx context.Context, c *Client, args DeleteImagesArgs) (WriteResult, error) {
	const tool = "delete_images"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.Locale == "" {
		return WriteResult{}, fmt.Errorf("locale is required — pass --locale en-US")
	}
	if !validImageType(args.Type) {
		return WriteResult{}, imageTypeError(args.Type)
	}
	if args.All == (args.ID != "") {
		return WriteResult{}, fmt.Errorf("pass either --id <id> or --all, not both and not neither")
	}

	base := "listings/" + args.Locale + "/" + args.Type
	if !args.All {
		return previewPlayWrite(stagePlayWriteRequest{
			Tool: tool, PackageName: pkg, Dispatch: dispatchEdit, ScopedDelete: true,
			Summary: fmt.Sprintf("Delete the %s image %s for %s", args.Type, args.ID, args.Locale),
			Payload: editPayload{
				Requests: []editRequest{{
					Method: http.MethodDelete, Path: base + "/" + args.ID,
					Describe: fmt.Sprintf("%s image %s", args.Type, args.ID),
				}},
				ChangesNotSentForReview: args.ChangesNotSentForReview,
			},
		})
	}

	existing, err := runImages(ctx, c, ImagesArgs{PackageName: pkg, Locale: args.Locale, Type: args.Type})
	if err != nil {
		return WriteResult{}, err
	}
	if len(existing.Images) == 0 {
		return WriteResult{}, fmt.Errorf("%s has no %s images for %s — nothing to delete", pkg, args.Type, args.Locale)
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchEdit,
		Summary: fmt.Sprintf("Delete all %d %s image(s) for %s", len(existing.Images), args.Type, args.Locale),
		Payload: editPayload{
			Requests: []editRequest{{
				Method: http.MethodDelete, Path: base,
				Describe: fmt.Sprintf("all %s images for %s", args.Type, args.Locale),
			}},
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// --- CLI front-end ---

var (
	uploadImagesArgs UploadImagesArgs
	deleteImagesArgs DeleteImagesArgs
)

var uploadImagesCmd = &cobra.Command{
	Use:         "upload",
	Short:       "Upload store listing images (previews first; --confirm to apply)",
	Annotations: mcpTool("upload_images"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, uploadImagesArgs, runUploadImages)
	},
}

var deleteImagesCmd = &cobra.Command{
	Use:         "delete",
	Short:       "Delete store listing images (--all takes two confirmations)",
	Annotations: mcpTool("delete_images"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, deleteImagesArgs, runDeleteImages)
	},
}

func init() {
	addPackageFlag(uploadImagesCmd, &uploadImagesArgs.PackageName)
	uploadImagesCmd.Flags().StringVar(&uploadImagesArgs.Locale, "locale", "", "BCP-47 locale (required)")
	uploadImagesCmd.Flags().StringVar(&uploadImagesArgs.Type, "type", "", "image type (required)")
	uploadImagesCmd.Flags().StringArrayVar(&uploadImagesArgs.Files, "file", nil, "image file to upload (repeatable)")
	uploadImagesCmd.Flags().BoolVar(&uploadImagesArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(uploadImagesCmd, &uploadImagesArgs.Confirm)

	addPackageFlag(deleteImagesCmd, &deleteImagesArgs.PackageName)
	deleteImagesCmd.Flags().StringVar(&deleteImagesArgs.Locale, "locale", "", "BCP-47 locale (required)")
	deleteImagesCmd.Flags().StringVar(&deleteImagesArgs.Type, "type", "", "image type (required)")
	deleteImagesCmd.Flags().StringVar(&deleteImagesArgs.ID, "id", "", "id of one image to delete")
	deleteImagesCmd.Flags().BoolVar(&deleteImagesArgs.All, "all", false, "delete every image of this type in this locale")
	deleteImagesCmd.Flags().BoolVar(&deleteImagesArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(deleteImagesCmd, &deleteImagesArgs.Confirm)

	imagesCmd.AddCommand(uploadImagesCmd, deleteImagesCmd)
}
