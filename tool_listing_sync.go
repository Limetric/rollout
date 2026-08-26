package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// SyncListingArgs reconciles a local metadata directory with the store.
type SyncListingArgs struct {
	PackageName string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Dir         string   `json:"dir" jsonschema:"the metadata directory, in the fastlane supply layout: <locale>/title.txt, <locale>/images/phoneScreenshots/*.png"`
	Locales     []string `json:"locales,omitempty" jsonschema:"only sync these locales; omit for every locale directory found"`
	Images      bool     `json:"images,omitempty" jsonschema:"also sync images; text only by default because image uploads are slow and irreversible"`
	// DeleteMissing is opt-in because the common case is a metadata tree that
	// covers some image types and not others, where deleting the rest would be
	// destructive and surprising.
	DeleteMissing bool `json:"delete_missing,omitempty" jsonschema:"delete store images that the directory does not contain"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runSyncListing stages or applies a whole metadata directory.
//
// The plan is computed at preview time against what the store currently has,
// and the *whole plan* is staged — file paths, hashes, image ids and all — so
// `rollout confirm` can apply it from another process without re-reading the
// directory, and so a file edited in the meantime is refused rather than
// silently published.
func runSyncListing(ctx context.Context, c *Client, args SyncListingArgs) (WriteResult, error) {
	const tool = "sync_listing"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.Dir == "" {
		return WriteResult{}, fmt.Errorf("dir is required — pass --dir metadata/android")
	}

	local, err := readMetadataDir(expandHome(args.Dir), args.Locales)
	if err != nil {
		return WriteResult{}, err
	}

	plans, err := c.planListingSync(ctx, pkg, local, args.Images, args.DeleteMissing)
	if err != nil {
		return WriteResult{}, toolError(tool, err)
	}
	var pending []localeSyncPlan
	for _, plan := range plans {
		if !plan.empty() {
			pending = append(pending, plan)
		}
	}
	if len(pending) == 0 {
		return WriteResult{}, fmt.Errorf("the store already matches %s — nothing to sync", args.Dir)
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Sync %s → %s (%d locale(s)):\n", args.Dir, pkg, len(pending))
	summary.WriteString(describeSyncPlan(pending))
	summary.WriteString("Everything below is committed in one edit; if any locale fails, none of it lands.")

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchListingSync, Summary: summary.String(),
		Payload: listingSyncPayload{Plans: pending, ChangesNotSentForReview: args.ChangesNotSentForReview},
	})
}

// planListingSync reads the store's current state and computes the plan, using
// one read-only edit for every locale rather than one per locale.
func (c *Client) planListingSync(ctx context.Context, pkg string, local []localMetadata, withImages, deleteMissing bool) ([]localeSyncPlan, error) {
	current := map[string]Listing{}
	images := map[string][]ImageInfo{}

	_, err := c.withEdit(ctx, pkg, false, commitOptions{}, func(e *editSession) error {
		listings, err := e.listings(ctx, "")
		if err != nil {
			return err
		}
		for _, listing := range listings {
			current[listing.Language] = listing
		}
		if !withImages {
			return nil
		}
		for _, meta := range local {
			found, err := e.images(ctx, meta.Locale, appImageTypes)
			if err != nil {
				return err
			}
			images[meta.Locale] = found
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	plans := make([]localeSyncPlan, 0, len(local))
	for _, meta := range local {
		var existing *Listing
		if listing, ok := current[meta.Locale]; ok {
			existing = &listing
		}
		plan, err := planLocaleSync(meta, existing, images[meta.Locale], withImages, deleteMissing)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// --- CLI front-end ---

var syncListingArgs SyncListingArgs

var syncListingCmd = &cobra.Command{
	Use:         "sync",
	Short:       "Reconcile a metadata directory with the store (previews the plan; --confirm to apply)",
	Annotations: mcpTool("sync_listing"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, syncListingArgs, runSyncListing)
	},
}

func init() {
	addPackageFlag(syncListingCmd, &syncListingArgs.PackageName)
	syncListingCmd.Flags().StringVar(&syncListingArgs.Dir, "dir", "", "metadata directory (required)")
	syncListingCmd.Flags().StringArrayVar(&syncListingArgs.Locales, "locales", nil, "only sync these locales")
	syncListingCmd.Flags().BoolVar(&syncListingArgs.Images, "images", false, "also sync images")
	syncListingCmd.Flags().BoolVar(&syncListingArgs.DeleteMissing, "delete-missing", false, "delete store images the directory does not contain")
	syncListingCmd.Flags().BoolVar(&syncListingArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(syncListingCmd, &syncListingArgs.Confirm)
	listingCmd.AddCommand(syncListingCmd)
}
