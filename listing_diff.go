package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Reconciling a local metadata directory with the store. The plan is computed
// by pure functions here so it can be unit-tested without HTTP, and so the
// preview shows exactly what the apply will do.

// Play's store listing limits. The API rejects an over-length field with a
// message that names neither the locale nor which field, after an edit is open.
const (
	maxTitleRunes            = 30
	maxShortDescriptionRunes = 80
	maxFullDescriptionRunes  = 4000
)

// listingFieldLimits maps a listing field to its limit, for error messages that
// name both.
var listingFieldLimits = []struct {
	name  string
	limit int
	get   func(Listing) string
}{
	{"title", maxTitleRunes, func(l Listing) string { return l.Title }},
	{"short description", maxShortDescriptionRunes, func(l Listing) string { return l.ShortDescription }},
	{"full description", maxFullDescriptionRunes, func(l Listing) string { return l.FullDescription }},
}

// validateListing enforces the character limits, naming the locale and field.
func validateListing(l Listing) error {
	for _, field := range listingFieldLimits {
		value := field.get(l)
		if n := utf8.RuneCountInString(value); n > field.limit {
			return fmt.Errorf("the %s for %s is %d characters; Play's limit is %d", field.name, l.Language, n, field.limit)
		}
	}
	return nil
}

// listingChange is one locale's text difference.
type listingChange struct {
	Locale string `json:"locale"`
	Field  string `json:"field"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// diffListing reports the fields that differ between what the store has and
// what is being set. It only considers fields the caller actually supplied, so
// a partial update never reads as clearing everything else.
func diffListing(current, desired Listing, supplied map[string]bool) []listingChange {
	var changes []listingChange
	add := func(field, from, to string) {
		if supplied != nil && !supplied[field] {
			return
		}
		if from != to {
			changes = append(changes, listingChange{Locale: desired.Language, Field: field, From: from, To: to})
		}
	}
	add("title", current.Title, desired.Title)
	add("short_description", current.ShortDescription, desired.ShortDescription)
	add("full_description", current.FullDescription, desired.FullDescription)
	add("video", current.Video, desired.Video)
	return changes
}

// describeChange renders one field change for a preview, truncating long text.
func describeChange(c listingChange) string {
	return fmt.Sprintf("%s %s: %s → %s", c.Locale, c.Field, truncate(c.From, 60), truncate(c.To, 60))
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if s == "" {
		return "(empty)"
	}
	if utf8.RuneCountInString(s) <= n {
		return `"` + s + `"`
	}
	runes := []rune(s)
	return `"` + string(runes[:n]) + `…"`
}

// --- directory sync ---

// localMetadata is what a metadata directory holds for one locale.
type localMetadata struct {
	Locale  string
	Listing Listing
	// Images maps an AppImageType to the local files for it, in the order they
	// should appear on the store page.
	Images map[string][]string
	// Supplied names the listing fields the directory actually provides, so a
	// locale with only a title does not read as clearing its description.
	Supplied map[string]bool
}

// listingFileFields maps a file name in a locale directory to the listing field
// it supplies. These names are fastlane `supply`'s, which is what almost every
// existing Android release pipeline already has on disk.
var listingFileFields = map[string]string{
	"title.txt":             "title",
	"short_description.txt": "short_description",
	"full_description.txt":  "full_description",
	"video.txt":             "video",
}

// readMetadataDir reads a fastlane-style metadata tree.
//
// Layout: `<dir>/<locale>/{title,short_description,full_description,video}.txt`
// and `<dir>/<locale>/images/{icon,featureGraphic}.png` plus
// `<dir>/<locale>/images/phoneScreenshots/*.png`.
func readMetadataDir(dir string, locales []string) ([]localMetadata, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read metadata directory %q: %w", dir, err)
	}
	wanted := map[string]bool{}
	for _, l := range locales {
		wanted[l] = true
	}

	var out []localMetadata
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		locale := entry.Name()
		if len(wanted) > 0 && !wanted[locale] {
			continue
		}
		meta, err := readLocaleDir(filepath.Join(dir, locale), locale)
		if err != nil {
			return nil, err
		}
		if meta != nil {
			out = append(out, *meta)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no locale directories found in %q — expected <locale>/title.txt and friends", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Locale < out[j].Locale })
	return out, nil
}

// readLocaleDir reads one locale's text and images. A directory with neither is
// skipped rather than failing: metadata trees hold `changelogs`-only locales.
func readLocaleDir(dir, locale string) (*localMetadata, error) {
	meta := localMetadata{
		Locale:   locale,
		Listing:  Listing{Language: locale},
		Images:   map[string][]string{},
		Supplied: map[string]bool{},
	}

	for file, field := range listingFileFields {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s for %s: %w", file, locale, err)
		}
		value := strings.TrimRight(string(data), "\n")
		meta.Supplied[field] = true
		switch field {
		case "title":
			meta.Listing.Title = value
		case "short_description":
			meta.Listing.ShortDescription = value
		case "full_description":
			meta.Listing.FullDescription = value
		case "video":
			meta.Listing.Video = value
		}
	}

	images, err := readLocaleImages(filepath.Join(dir, "images"))
	if err != nil {
		return nil, fmt.Errorf("read images for %s: %w", locale, err)
	}
	meta.Images = images

	if len(meta.Supplied) == 0 && len(meta.Images) == 0 {
		return nil, nil
	}
	return &meta, nil
}

// readLocaleImages reads an `images/` directory: single-image types as
// `<type>.png`, multi-image types as `<type>/*.png`.
func readLocaleImages(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, err
	}

	out := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if !validImageType(name) {
				continue
			}
			files, err := imageFilesIn(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			if len(files) > 0 {
				out[name] = files
			}
			continue
		}
		imageType := strings.TrimSuffix(name, filepath.Ext(name))
		if !validImageType(imageType) || !isImageFile(name) {
			continue
		}
		out[imageType] = []string{filepath.Join(dir, name)}
	}
	return out, nil
}

// imageFilesIn lists the image files in a directory, sorted by name — which is
// how a screenshot set's order is expressed on disk (01.png, 02.png, …).
func imageFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !isImageFile(entry.Name()) {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

func isImageFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg":
		return true
	default:
		return false
	}
}

// --- the sync plan ---

// imageUpload is one image the sync will upload.
type imageUpload struct {
	Type string `json:"type"`
	Path string `json:"path"`
	// SHA256 is the hash at plan time. The apply refuses when the file has
	// changed since — a confirm token outlives the command that produced it,
	// and re-running the design tool in that window is exactly what happens.
	SHA256   string   `json:"sha256"`
	Warnings []string `json:"warnings,omitempty"`
}

// imageDeletion is one image the sync will remove.
type imageDeletion struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// localeSyncPlan is everything the sync will do to one locale.
type localeSyncPlan struct {
	Locale string `json:"locale"`
	// Listing is the text to write, or nil when the locale's text is already
	// what the directory says.
	Listing *Listing        `json:"listing,omitempty"`
	Changes []listingChange `json:"changes,omitempty"`
	Uploads []imageUpload   `json:"image_uploads,omitempty"`
	Deletes []imageDeletion `json:"image_deletes,omitempty"`
	// Warnings collects per-type count problems (too many screenshots).
	Warnings []string `json:"warnings,omitempty"`
}

// empty reports whether this locale needs no work at all.
func (p localeSyncPlan) empty() bool {
	return p.Listing == nil && len(p.Uploads) == 0 && len(p.Deletes) == 0
}

// planLocaleSync computes what one locale needs.
//
// Images are compared by SHA-256 against what `images.list` reported, which is
// what lets an unchanged screenshot be skipped rather than re-uploaded — the
// difference between a sync that costs one call and one that re-uploads eight
// megabytes of identical PNGs every run.
func planLocaleSync(local localMetadata, currentListing *Listing, currentImages []ImageInfo, withImages, deleteMissing bool) (localeSyncPlan, error) {
	plan := localeSyncPlan{Locale: local.Locale}

	if len(local.Supplied) > 0 {
		if err := validateListing(local.Listing); err != nil {
			return plan, err
		}
		existing := Listing{Language: local.Locale}
		if currentListing != nil {
			existing = *currentListing
		}
		plan.Changes = diffListing(existing, local.Listing, local.Supplied)
		if len(plan.Changes) > 0 {
			// The API's listings.update replaces the whole listing, so fields
			// the directory does not supply have to be carried over from what
			// is there — otherwise syncing a title would blank the description.
			merged := existing
			merged.Language = local.Locale
			applySuppliedFields(&merged, local.Listing, local.Supplied)
			plan.Listing = &merged
		}
	}

	if !withImages {
		return plan, nil
	}

	byType := map[string][]ImageInfo{}
	for _, img := range currentImages {
		byType[img.Type] = append(byType[img.Type], img)
	}
	for _, imageType := range appImageTypes {
		files := local.Images[imageType]
		remote := byType[imageType]
		if len(files) == 0 {
			// A type the directory does not mention is left alone unless the
			// caller explicitly asked for deletions — a metadata tree with only
			// phone screenshots must not wipe the tablet ones.
			if deleteMissing {
				for _, img := range remote {
					plan.Deletes = append(plan.Deletes, imageDeletion{Type: imageType, ID: img.ID})
				}
			}
			continue
		}

		remoteHashes := map[string]bool{}
		for _, img := range remote {
			if img.SHA256 != "" {
				remoteHashes[img.SHA256] = true
			}
		}
		localHashes := map[string]bool{}
		for _, file := range files {
			meta, err := readImageMeta(file, imageType)
			if err != nil {
				return plan, err
			}
			localHashes[meta.SHA256] = true
			if remoteHashes[meta.SHA256] {
				continue // already on the store, byte for byte
			}
			plan.Uploads = append(plan.Uploads, imageUpload{
				Type: imageType, Path: file, SHA256: meta.SHA256, Warnings: meta.Warnings,
			})
		}
		if deleteMissing {
			for _, img := range remote {
				if img.SHA256 == "" || !localHashes[img.SHA256] {
					plan.Deletes = append(plan.Deletes, imageDeletion{Type: imageType, ID: img.ID})
				}
			}
		}
		plan.Warnings = append(plan.Warnings, countWarnings(imageType, len(files))...)
	}
	return plan, nil
}

// applySuppliedFields copies only the fields the directory provided.
func applySuppliedFields(dst *Listing, src Listing, supplied map[string]bool) {
	if supplied["title"] {
		dst.Title = src.Title
	}
	if supplied["short_description"] {
		dst.ShortDescription = src.ShortDescription
	}
	if supplied["full_description"] {
		dst.FullDescription = src.FullDescription
	}
	if supplied["video"] {
		dst.Video = src.Video
	}
}

// describeSyncPlan renders the whole plan for a preview.
func describeSyncPlan(plans []localeSyncPlan) string {
	var b strings.Builder
	for _, plan := range plans {
		if plan.empty() {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", plan.Locale)
		for _, change := range plan.Changes {
			fmt.Fprintf(&b, "    %s\n", describeChange(change))
		}
		for _, upload := range plan.Uploads {
			fmt.Fprintf(&b, "    upload %s → %s\n", filepath.Base(upload.Path), upload.Type)
			for _, warning := range upload.Warnings {
				fmt.Fprintf(&b, "      warning: %s\n", warning)
			}
		}
		for _, deletion := range plan.Deletes {
			fmt.Fprintf(&b, "    delete %s image %s\n", deletion.Type, deletion.ID)
		}
		for _, warning := range plan.Warnings {
			fmt.Fprintf(&b, "    warning: %s\n", warning)
		}
	}
	if b.Len() == 0 {
		return "  (nothing to do — the store already matches the directory)\n"
	}
	return b.String()
}
