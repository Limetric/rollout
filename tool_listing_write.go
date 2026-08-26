package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Store listing writes: per-locale text, app details, and the deletions.

// UpdateListingArgs sets store listing text for one locale.
type UpdateListingArgs struct {
	PackageName      string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Locale           string `json:"locale" jsonschema:"the BCP-47 locale to write, such as en-US"`
	Title            string `json:"title,omitempty" jsonschema:"the app title, at most 30 characters"`
	ShortDescription string `json:"short_description,omitempty" jsonschema:"the short description, at most 80 characters"`
	FullDescription  string `json:"full_description,omitempty" jsonschema:"the full description, at most 4000 characters"`
	Video            string `json:"video,omitempty" jsonschema:"a YouTube URL for the promo video"`
	FromFile         string `json:"from_file,omitempty" jsonschema:"read the listing fields from a JSON file instead of the flags"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`

	// supplied records which fields the caller actually set, so a partial
	// update carries the rest across instead of blanking them.
	supplied map[string]bool
}

// runUpdateListing stages or applies a listing text change.
//
// The API's listings.update replaces the whole listing, so the current text is
// fetched during the preview and merged: setting a title must not blank the
// description that nobody mentioned.
func runUpdateListing(ctx context.Context, c *Client, args UpdateListingArgs) (WriteResult, error) {
	const tool = "update_listing"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.Locale == "" {
		return WriteResult{}, fmt.Errorf("locale is required — pass --locale en-US (listings are per locale)")
	}
	if args.FromFile != "" {
		if err := args.readFromFile(); err != nil {
			return WriteResult{}, err
		}
	}
	supplied := args.suppliedFields()
	if len(supplied) == 0 {
		return WriteResult{}, fmt.Errorf("nothing to change — pass --title, --short-description, --full-description, --video, or --from-file")
	}

	desired := Listing{
		Language: args.Locale, Title: args.Title,
		ShortDescription: args.ShortDescription, FullDescription: args.FullDescription,
		Video: args.Video,
	}
	if err := validateListing(desired); err != nil {
		return WriteResult{}, err
	}

	current, err := c.readListings(ctx, pkg, args.Locale)
	if err != nil {
		return WriteResult{}, toolError(tool, err)
	}
	existing := Listing{Language: args.Locale}
	if len(current) > 0 {
		existing = current[0]
	}
	changes := diffListing(existing, desired, supplied)
	if len(changes) == 0 {
		return WriteResult{}, fmt.Errorf("the %s listing already matches — nothing to do", args.Locale)
	}

	merged := existing
	merged.Language = args.Locale
	applySuppliedFields(&merged, desired, supplied)
	body, err := json.Marshal(apiListing(merged))
	if err != nil {
		return WriteResult{}, err
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Update the %s store listing:", args.Locale)
	for _, change := range changes {
		fmt.Fprintf(&summary, "\n  %s", describeChange(change))
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchEdit, Summary: summary.String(),
		Payload: editPayload{
			Requests: []editRequest{{
				Method: http.MethodPut, Path: "listings/" + args.Locale, Body: body,
				Describe: "listing for " + args.Locale,
			}},
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// readFromFile loads listing fields from a JSON file, which is how a long full
// description gets in without fighting shell quoting.
func (a *UpdateListingArgs) readFromFile() error {
	data, err := os.ReadFile(expandHome(a.FromFile))
	if err != nil {
		return fmt.Errorf("read listing file %q: %w", a.FromFile, err)
	}
	var file struct {
		Title            *string `json:"title"`
		ShortDescription *string `json:"short_description"`
		FullDescription  *string `json:"full_description"`
		Video            *string `json:"video"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse listing file %q: %w", a.FromFile, err)
	}
	if a.supplied == nil {
		a.supplied = map[string]bool{}
	}
	// Pointers, so a field present-but-empty in the file clears that field
	// while a field the file omits is left alone.
	if file.Title != nil {
		a.Title, a.supplied["title"] = *file.Title, true
	}
	if file.ShortDescription != nil {
		a.ShortDescription, a.supplied["short_description"] = *file.ShortDescription, true
	}
	if file.FullDescription != nil {
		a.FullDescription, a.supplied["full_description"] = *file.FullDescription, true
	}
	if file.Video != nil {
		a.Video, a.supplied["video"] = *file.Video, true
	}
	return nil
}

// suppliedFields reports which listing fields this call is setting.
func (a UpdateListingArgs) suppliedFields() map[string]bool {
	supplied := map[string]bool{}
	for field, set := range a.supplied {
		if set {
			supplied[field] = true
		}
	}
	if a.Title != "" {
		supplied["title"] = true
	}
	if a.ShortDescription != "" {
		supplied["short_description"] = true
	}
	if a.FullDescription != "" {
		supplied["full_description"] = true
	}
	if a.Video != "" {
		supplied["video"] = true
	}
	return supplied
}

// DeleteListingArgs removes a locale's store listing.
type DeleteListingArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Locale      string `json:"locale,omitempty" jsonschema:"the locale to delete; omit and set all to remove every localized listing"`
	All         bool   `json:"all,omitempty" jsonschema:"delete every localized listing"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runDeleteListing stages or applies a listing deletion. It takes two
// confirmations: the API cannot bring a listing back, and the text is often the
// only copy of work nobody has in version control.
func runDeleteListing(ctx context.Context, c *Client, args DeleteListingArgs) (WriteResult, error) {
	const tool = "delete_listing"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.All == (args.Locale != "") {
		return WriteResult{}, fmt.Errorf("pass either --locale <locale> or --all, not both and not neither")
	}

	path, summary := "listings/"+args.Locale, "Delete the "+args.Locale+" store listing"
	if args.All {
		current, err := c.readListings(ctx, pkg, "")
		if err != nil {
			return WriteResult{}, toolError(tool, err)
		}
		locales := make([]string, 0, len(current))
		for _, listing := range current {
			locales = append(locales, listing.Language)
		}
		path = "listings"
		summary = fmt.Sprintf("Delete every store listing (%d locales: %s)", len(locales), strings.Join(locales, ", "))
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchEdit, Summary: summary,
		Payload: editPayload{
			Requests: []editRequest{{
				Method: http.MethodDelete, Path: path, Describe: summary,
			}},
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// UpdateDetailsArgs sets the app-level details.
type UpdateDetailsArgs struct {
	PackageName     string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	DefaultLanguage string `json:"default_language,omitempty" jsonschema:"the BCP-47 locale the store falls back to"`
	ContactWebsite  string `json:"contact_website,omitempty" jsonschema:"the developer website shown on the store page"`
	ContactEmail    string `json:"contact_email,omitempty" jsonschema:"the developer contact email shown on the store page"`
	ContactPhone    string `json:"contact_phone,omitempty" jsonschema:"the developer contact phone shown on the store page"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runUpdateDetails stages or applies an app-details change. The API's
// details.patch only touches the fields sent, so this is a genuine patch.
func runUpdateDetails(ctx context.Context, c *Client, args UpdateDetailsArgs) (WriteResult, error) {
	const tool = "update_details"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}

	patch := map[string]any{}
	var changes []string
	current, err := runDetails(ctx, c, DetailsArgs{PackageName: pkg})
	if err != nil {
		return WriteResult{}, err
	}
	for _, field := range []struct {
		key, value, was string
	}{
		{"defaultLanguage", args.DefaultLanguage, current.Details.DefaultLanguage},
		{"contactWebsite", args.ContactWebsite, current.Details.ContactWebsite},
		{"contactEmail", args.ContactEmail, current.Details.ContactEmail},
		{"contactPhone", args.ContactPhone, current.Details.ContactPhone},
	} {
		if field.value == "" {
			continue
		}
		patch[field.key] = field.value
		changes = append(changes, fmt.Sprintf("%s: %s → %s", field.key, truncate(field.was, 40), truncate(field.value, 40)))
	}
	if len(patch) == 0 {
		return WriteResult{}, fmt.Errorf("nothing to change — pass --default-language, --contact-website, --contact-email, or --contact-phone")
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return WriteResult{}, err
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchEdit,
		Summary: "Update app details:\n  " + strings.Join(changes, "\n  "),
		Payload: editPayload{
			Requests: []editRequest{{
				Method: http.MethodPatch, Path: "details", Body: body, Describe: "app details",
			}},
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// --- CLI front-end ---

var (
	updateListingArgs UpdateListingArgs
	deleteListingArgs DeleteListingArgs
	updateDetailsArgs UpdateDetailsArgs
)

var setListingCmd = &cobra.Command{
	Use:         "set",
	Short:       "Set store listing text for a locale (previews first; --confirm to apply)",
	Annotations: mcpTool("update_listing"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// Cobra cannot tell "--title=''" from an unset flag, and the two mean
		// different things when the update is a merge.
		updateListingArgs.supplied = map[string]bool{}
		for flag, field := range map[string]string{
			"title": "title", "short-description": "short_description",
			"full-description": "full_description", "video": "video",
		} {
			if cmd.Flags().Changed(flag) {
				updateListingArgs.supplied[field] = true
			}
		}
		return runPlayWrite(cmd, updateListingArgs, runUpdateListing)
	},
}

var deleteListingCmd = &cobra.Command{
	Use:         "delete",
	Short:       "Delete a locale's store listing (destructive: takes two confirmations)",
	Annotations: mcpTool("delete_listing"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, deleteListingArgs, runDeleteListing)
	},
}

var setDetailsCmd = &cobra.Command{
	Use:         "set",
	Short:       "Set app details (previews first; --confirm to apply)",
	Annotations: mcpTool("update_details"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, updateDetailsArgs, runUpdateDetails)
	},
}

func init() {
	addPackageFlag(setListingCmd, &updateListingArgs.PackageName)
	setListingCmd.Flags().StringVar(&updateListingArgs.Locale, "locale", "", "BCP-47 locale (required)")
	setListingCmd.Flags().StringVar(&updateListingArgs.Title, "title", "", "app title (max 30 characters)")
	setListingCmd.Flags().StringVar(&updateListingArgs.ShortDescription, "short-description", "", "short description (max 80 characters)")
	setListingCmd.Flags().StringVar(&updateListingArgs.FullDescription, "full-description", "", "full description (max 4000 characters)")
	setListingCmd.Flags().StringVar(&updateListingArgs.Video, "video", "", "YouTube URL for the promo video")
	setListingCmd.Flags().StringVar(&updateListingArgs.FromFile, "from-file", "", "read the listing fields from a JSON file")
	setListingCmd.Flags().BoolVar(&updateListingArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(setListingCmd, &updateListingArgs.Confirm)

	addPackageFlag(deleteListingCmd, &deleteListingArgs.PackageName)
	deleteListingCmd.Flags().StringVar(&deleteListingArgs.Locale, "locale", "", "locale to delete")
	deleteListingCmd.Flags().BoolVar(&deleteListingArgs.All, "all", false, "delete every localized listing")
	deleteListingCmd.Flags().BoolVar(&deleteListingArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(deleteListingCmd, &deleteListingArgs.Confirm)

	addPackageFlag(setDetailsCmd, &updateDetailsArgs.PackageName)
	setDetailsCmd.Flags().StringVar(&updateDetailsArgs.DefaultLanguage, "default-language", "", "BCP-47 locale the store falls back to")
	setDetailsCmd.Flags().StringVar(&updateDetailsArgs.ContactWebsite, "contact-website", "", "developer website")
	setDetailsCmd.Flags().StringVar(&updateDetailsArgs.ContactEmail, "contact-email", "", "developer contact email")
	setDetailsCmd.Flags().StringVar(&updateDetailsArgs.ContactPhone, "contact-phone", "", "developer contact phone")
	setDetailsCmd.Flags().BoolVar(&updateDetailsArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(setDetailsCmd, &updateDetailsArgs.Confirm)

	listingCmd.AddCommand(setListingCmd, deleteListingCmd)
	detailsCmd.AddCommand(setDetailsCmd)
}
