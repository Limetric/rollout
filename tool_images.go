package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// appImageTypes are the AppImageType values the Publisher API accepts. Reading
// "all images" means eight calls, one per type — the API has no listing that
// spans them.
var appImageTypes = []string{
	"icon",
	"featureGraphic",
	"phoneScreenshots",
	"sevenInchScreenshots",
	"tenInchScreenshots",
	"tvBanner",
	"tvScreenshots",
	"wearScreenshots",
}

// validImageType reports whether t is an AppImageType, and is what stops a
// typo from becoming a 400 with a message that does not list the alternatives.
func validImageType(t string) bool {
	for _, known := range appImageTypes {
		if known == t {
			return true
		}
	}
	return false
}

func imageTypeError(t string) error {
	return fmt.Errorf("unknown image type %q — expected one of: %s", t, strings.Join(appImageTypes, ", "))
}

// ImagesArgs lists the store listing images of one locale.
type ImagesArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Locale      string `json:"locale" jsonschema:"the BCP-47 locale whose images to list, such as en-US"`
	Type        string `json:"type,omitempty" jsonschema:"one image type (icon, featureGraphic, phoneScreenshots, sevenInchScreenshots, tenInchScreenshots, tvBanner, tvScreenshots, wearScreenshots); omit for all eight"`
}

// ImageInfo is one uploaded image. SHA256 is what makes a sync able to skip an
// unchanged screenshot instead of re-uploading it.
type ImageInfo struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	SHA1   string `json:"sha1,omitempty"`
}

// ImagesResult is every image of a locale.
type ImagesResult struct {
	PackageName string      `json:"package_name"`
	Locale      string      `json:"locale"`
	Images      []ImageInfo `json:"images"`
}

func (r ImagesResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Images), []string{"type", "id", "sha256", "url"}
}

// runImages lists a locale's images inside one read-only edit.
func runImages(ctx context.Context, c *Client, args ImagesArgs) (ImagesResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ImagesResult{}, err
	}
	if args.Locale == "" {
		return ImagesResult{}, fmt.Errorf("locale is required — pass --locale en-US (images are per locale)")
	}
	types := appImageTypes
	if args.Type != "" {
		if !validImageType(args.Type) {
			return ImagesResult{}, imageTypeError(args.Type)
		}
		types = []string{args.Type}
	}

	var images []ImageInfo
	_, err = c.withEdit(ctx, pkg, false, commitOptions{}, func(e *editSession) error {
		found, err := e.images(ctx, args.Locale, types)
		if err != nil {
			return err
		}
		images = found
		return nil
	})
	if err != nil {
		return ImagesResult{}, toolError("images", err)
	}
	return ImagesResult{PackageName: pkg, Locale: args.Locale, Images: images}, nil
}

// images lists the given image types inside an already-open edit. All eight
// types go through one edit, not eight.
func (e *editSession) images(ctx context.Context, locale string, types []string) ([]ImageInfo, error) {
	var out []ImageInfo
	for _, imageType := range types {
		var list struct {
			Images []struct {
				ID     string `json:"id"`
				URL    string `json:"url"`
				SHA256 string `json:"sha256"`
				SHA1   string `json:"sha1"`
			} `json:"images"`
		}
		path := e.path("listings/" + locale + "/" + imageType)
		if err := e.c.do(ctx, http.MethodGet, path, nil, nil, &list); err != nil {
			// A locale with no images of this type is an answer, not a failure.
			if isNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("list %s images for %s: %w", imageType, locale, err)
		}
		for _, img := range list.Images {
			out = append(out, ImageInfo{Type: imageType, ID: img.ID, URL: img.URL, SHA256: img.SHA256, SHA1: img.SHA1})
		}
	}
	return out, nil
}

// --- CLI front-end ---

var (
	imagesArgs   ImagesArgs
	imagesFormat string
)

// imagesCmd reads images and parents the image writes.
var imagesCmd = &cobra.Command{
	Use:         "images",
	Short:       "List store listing images for a locale",
	Annotations: mcpTool("images"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, imagesArgs, imagesFormat, runImages)
	},
}

func init() {
	addPackageFlag(imagesCmd, &imagesArgs.PackageName)
	imagesCmd.Flags().StringVar(&imagesArgs.Locale, "locale", "", "BCP-47 locale (required)")
	imagesCmd.Flags().StringVar(&imagesArgs.Type, "type", "", "one image type (default: all eight)")
	addFormatFlag(imagesCmd, &imagesFormat)
}
