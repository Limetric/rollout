package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makePNG writes a minimal but structurally real PNG of the given size, so the
// header decoder is tested against actual bytes rather than a fixture blob.
func makePNG(t *testing.T, path string, width, height int) string {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(pngMagic)
	// IHDR: length, type, width, height, then the rest of the header fields.
	_ = binary.Write(&buf, binary.BigEndian, uint32(13))
	buf.WriteString("IHDR")
	_ = binary.Write(&buf, binary.BigEndian, uint32(width))
	_ = binary.Write(&buf, binary.BigEndian, uint32(height))
	buf.Write([]byte{8, 6, 0, 0, 0})
	// A CRC and an IEND, so the file is at least plausible on disk.
	buf.Write([]byte{0, 0, 0, 0})
	buf.Write([]byte{0, 0, 0, 0})
	buf.WriteString("IEND")
	buf.Write([]byte{0xae, 0x42, 0x60, 0x82})

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestDecodeImageHeader(t *testing.T) {
	t.Run("png", func(t *testing.T) {
		path := makePNG(t, filepath.Join(t.TempDir(), "icon.png"), 512, 512)
		meta, err := readImageMeta(path, "icon")
		if err != nil {
			t.Fatalf("readImageMeta: %v", err)
		}
		if meta.Format != "png" || meta.Width != 512 || meta.Height != 512 {
			t.Errorf("decoded %+v", meta)
		}
		if len(meta.Warnings) != 0 {
			t.Errorf("a correct icon should have no warnings: %v", meta.Warnings)
		}
	})

	t.Run("jpeg", func(t *testing.T) {
		// SOI, then a SOF0 segment declaring 1024×500.
		data := []byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x11, 0x08}
		data = binary.BigEndian.AppendUint16(data, 500)  // height
		data = binary.BigEndian.AppendUint16(data, 1024) // width
		data = append(data, make([]byte, 16)...)

		path := filepath.Join(t.TempDir(), "feature.jpg")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		meta, err := readImageMeta(path, "featureGraphic")
		if err != nil {
			t.Fatalf("readImageMeta: %v", err)
		}
		if meta.Format != "jpeg" || meta.Width != 1024 || meta.Height != 500 {
			t.Errorf("decoded %+v", meta)
		}
	})

	t.Run("anything else is named as such", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "art.gif")
		if err := os.WriteFile(path, []byte("GIF89a............"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := readImageMeta(path, "phoneScreenshots")
		if err == nil || !strings.Contains(err.Error(), "PNG or JPEG") {
			t.Errorf("err = %v, want one naming the accepted formats", err)
		}
	})
}

// TestImageWarningsNameTheConstraint: Play rejects an image for its dimensions
// with a message that names neither the file nor the rule, after the transfer
// is already spent.
func TestImageWarningsNameTheConstraint(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name      string
		imageType string
		w, h      int
		want      string
	}{
		{"a wrong-sized icon", "icon", 256, 256, "512×512"},
		{"a wrong-sized feature graphic", "featureGraphic", 1024, 400, "1024×500"},
		{"a tiny screenshot", "phoneScreenshots", 100, 200, "between 320 and 3840"},
		{"an over-wide screenshot", "phoneScreenshots", 3000, 400, "2:1"},
		{"a huge screenshot", "phoneScreenshots", 4000, 4000, "between 320 and 3840"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := makePNG(t, filepath.Join(dir, tc.name+".png"), tc.w, tc.h)
			meta, err := readImageMeta(path, tc.imageType)
			if err != nil {
				t.Fatalf("readImageMeta: %v", err)
			}
			if len(meta.Warnings) == 0 {
				t.Fatalf("expected a warning for %d×%d as %s", tc.w, tc.h, tc.imageType)
			}
			if !strings.Contains(strings.Join(meta.Warnings, " "), tc.want) {
				t.Errorf("warnings %v should mention %q", meta.Warnings, tc.want)
			}
		})
	}

	// A valid screenshot passes.
	path := makePNG(t, filepath.Join(dir, "ok.png"), 1080, 1920)
	meta, err := readImageMeta(path, "phoneScreenshots")
	if err != nil {
		t.Fatalf("readImageMeta: %v", err)
	}
	if len(meta.Warnings) != 0 {
		t.Errorf("a 1080×1920 screenshot should pass: %v", meta.Warnings)
	}
}

func TestCountWarnings(t *testing.T) {
	if countWarnings("phoneScreenshots", 8) != nil {
		t.Error("8 screenshots is the limit, not over it")
	}
	if warnings := countWarnings("phoneScreenshots", 9); len(warnings) == 0 {
		t.Error("9 screenshots should warn")
	}
	if warnings := countWarnings("icon", 2); len(warnings) == 0 {
		t.Error("two icons should warn")
	}
}

func TestValidateListingNamesLocaleAndField(t *testing.T) {
	err := validateListing(Listing{Language: "de-DE", Title: strings.Repeat("x", 31)})
	if err == nil {
		t.Fatal("an over-length title should be rejected")
	}
	for _, want := range []string{"de-DE", "title", "30"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if err := validateListing(Listing{Language: "en-US", ShortDescription: strings.Repeat("x", 81)}); err == nil {
		t.Error("an over-length short description should be rejected")
	}
	if err := validateListing(Listing{Language: "en-US", FullDescription: strings.Repeat("x", 4001)}); err == nil {
		t.Error("an over-length full description should be rejected")
	}
	// Runes, not bytes.
	if err := validateListing(Listing{Language: "ja-JP", Title: strings.Repeat("あ", 30)}); err != nil {
		t.Errorf("a 30-rune title should be accepted: %v", err)
	}
}

func TestDiffListingOnlyConsidersSuppliedFields(t *testing.T) {
	current := Listing{Language: "en-US", Title: "Old", ShortDescription: "Short", FullDescription: "Long"}
	desired := Listing{Language: "en-US", Title: "New"}

	// A partial update must not read as clearing everything else.
	changes := diffListing(current, desired, map[string]bool{"title": true})
	if len(changes) != 1 || changes[0].Field != "title" || changes[0].To != "New" {
		t.Fatalf("unexpected changes: %+v", changes)
	}

	// With nothing supplied restricted, every difference shows.
	changes = diffListing(current, desired, nil)
	if len(changes) != 3 {
		t.Errorf("expected every field to differ: %+v", changes)
	}
}

// writeMetadataTree lays down a fastlane-style tree for the sync tests.
func writeMetadataTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "en-US", "title.txt"), "Example\n")
	writeFile(t, filepath.Join(dir, "en-US", "short_description.txt"), "A short one\n")
	writeFile(t, filepath.Join(dir, "en-US", "full_description.txt"), "A long one\n")
	makePNG(t, filepath.Join(dir, "en-US", "images", "icon.png"), 512, 512)
	makePNG(t, filepath.Join(dir, "en-US", "images", "phoneScreenshots", "01.png"), 1080, 1920)
	makePNG(t, filepath.Join(dir, "en-US", "images", "phoneScreenshots", "02.png"), 1080, 1921)

	writeFile(t, filepath.Join(dir, "nl-NL", "title.txt"), "Voorbeeld\n")
	// A locale directory with only changelogs is common in real metadata trees
	// and must not break the read.
	writeFile(t, filepath.Join(dir, "de-DE", "changelogs", "42.txt"), "Fehlerbehebungen\n")
	return dir
}

func TestReadMetadataDir(t *testing.T) {
	dir := writeMetadataTree(t)

	local, err := readMetadataDir(dir, nil)
	if err != nil {
		t.Fatalf("readMetadataDir: %v", err)
	}
	if len(local) != 2 {
		t.Fatalf("read %d locales, want the two with listing content: %+v", len(local), local)
	}
	if local[0].Locale != "en-US" || local[1].Locale != "nl-NL" {
		t.Errorf("locales are not sorted: %+v", local)
	}

	en := local[0]
	if en.Listing.Title != "Example" || en.Listing.ShortDescription != "A short one" {
		t.Errorf("unexpected listing: %+v", en.Listing)
	}
	// Only what the directory supplies is marked supplied, so a locale with no
	// video.txt does not read as clearing the video.
	if en.Supplied["video"] {
		t.Error("video should not be marked supplied")
	}
	if len(en.Images["phoneScreenshots"]) != 2 || len(en.Images["icon"]) != 1 {
		t.Errorf("unexpected images: %+v", en.Images)
	}
	// Screenshot order comes from the file names.
	if !strings.HasSuffix(en.Images["phoneScreenshots"][0], "01.png") {
		t.Errorf("screenshots are not in name order: %v", en.Images["phoneScreenshots"])
	}

	t.Run("locale filter", func(t *testing.T) {
		local, err := readMetadataDir(dir, []string{"nl-NL"})
		if err != nil {
			t.Fatalf("readMetadataDir: %v", err)
		}
		if len(local) != 1 || local[0].Locale != "nl-NL" {
			t.Errorf("filter did not apply: %+v", local)
		}
	})

	t.Run("an empty directory says what it expected", func(t *testing.T) {
		_, err := readMetadataDir(t.TempDir(), nil)
		if err == nil || !strings.Contains(err.Error(), "title.txt") {
			t.Errorf("err = %v, want one naming the expected layout", err)
		}
	})
}

func TestPlanLocaleSync(t *testing.T) {
	dir := writeMetadataTree(t)
	local, err := readMetadataDir(dir, []string{"en-US"})
	if err != nil {
		t.Fatalf("readMetadataDir: %v", err)
	}
	meta := local[0]

	iconHash, err := fileSHA256(meta.Images["icon"][0])
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}

	t.Run("a new locale uploads everything", func(t *testing.T) {
		plan, err := planLocaleSync(meta, nil, nil, true, false)
		if err != nil {
			t.Fatalf("planLocaleSync: %v", err)
		}
		if plan.Listing == nil || len(plan.Changes) != 3 {
			t.Fatalf("unexpected text plan: %+v", plan)
		}
		if len(plan.Uploads) != 3 {
			t.Errorf("expected every image to upload: %+v", plan.Uploads)
		}
	})

	t.Run("an unchanged screenshot is skipped", func(t *testing.T) {
		// This is what makes a sync cost one call instead of re-uploading
		// eight megabytes of identical PNGs on every run.
		current := Listing{Language: "en-US", Title: "Example", ShortDescription: "A short one", FullDescription: "A long one"}
		plan, err := planLocaleSync(meta, &current, []ImageInfo{
			{Type: "icon", ID: "icon-1", SHA256: iconHash},
		}, true, false)
		if err != nil {
			t.Fatalf("planLocaleSync: %v", err)
		}
		if plan.Listing != nil {
			t.Errorf("identical text should need no write: %+v", plan.Changes)
		}
		for _, upload := range plan.Uploads {
			if upload.Type == "icon" {
				t.Errorf("an identical icon was queued for upload: %+v", upload)
			}
		}
		if len(plan.Uploads) != 2 {
			t.Errorf("expected only the screenshots to upload: %+v", plan.Uploads)
		}
	})

	t.Run("a changed title carries the other fields across", func(t *testing.T) {
		// listings.update replaces the whole listing, so a locale whose
		// directory only changes the title must not blank its description.
		current := Listing{Language: "en-US", Title: "Older", ShortDescription: "A short one", FullDescription: "A long one", Video: "https://youtu.be/x"}
		plan, err := planLocaleSync(meta, &current, nil, false, false)
		if err != nil {
			t.Fatalf("planLocaleSync: %v", err)
		}
		if plan.Listing == nil {
			t.Fatal("expected a text write")
		}
		if plan.Listing.Title != "Example" {
			t.Errorf("title was not updated: %+v", plan.Listing)
		}
		if plan.Listing.Video != "https://youtu.be/x" {
			t.Errorf("the video the directory does not mention was dropped: %+v", plan.Listing)
		}
	})

	t.Run("images are left alone without --images", func(t *testing.T) {
		plan, err := planLocaleSync(meta, nil, []ImageInfo{{Type: "icon", ID: "icon-1", SHA256: "different"}}, false, false)
		if err != nil {
			t.Fatalf("planLocaleSync: %v", err)
		}
		if len(plan.Uploads) != 0 || len(plan.Deletes) != 0 {
			t.Errorf("images should not be touched: %+v", plan)
		}
	})

	t.Run("deletions are opt-in", func(t *testing.T) {
		stale := []ImageInfo{
			{Type: "phoneScreenshots", ID: "old-1", SHA256: "gone"},
			{Type: "tenInchScreenshots", ID: "tablet-1", SHA256: "untouched"},
		}
		plan, err := planLocaleSync(meta, nil, stale, true, false)
		if err != nil {
			t.Fatalf("planLocaleSync: %v", err)
		}
		if len(plan.Deletes) != 0 {
			t.Errorf("nothing should be deleted by default: %+v", plan.Deletes)
		}

		plan, err = planLocaleSync(meta, nil, stale, true, true)
		if err != nil {
			t.Fatalf("planLocaleSync: %v", err)
		}
		if len(plan.Deletes) != 2 {
			t.Fatalf("expected both stale images to be deleted: %+v", plan.Deletes)
		}
		// A type the directory does not mention at all is only touched with
		// --delete-missing, which is exactly this case.
		var sawTablet bool
		for _, deletion := range plan.Deletes {
			if deletion.Type == "tenInchScreenshots" {
				sawTablet = true
			}
		}
		if !sawTablet {
			t.Errorf("expected the unmentioned type to be deleted too: %+v", plan.Deletes)
		}
	})

	t.Run("an over-length field names the locale", func(t *testing.T) {
		bad := meta
		bad.Listing.Title = strings.Repeat("x", 31)
		_, err := planLocaleSync(bad, nil, nil, false, false)
		if err == nil || !strings.Contains(err.Error(), "en-US") {
			t.Errorf("err = %v, want one naming the locale", err)
		}
	})
}

func TestDescribeSyncPlan(t *testing.T) {
	plans := []localeSyncPlan{
		{
			Locale:  "en-US",
			Changes: []listingChange{{Locale: "en-US", Field: "title", From: "Old", To: "New"}},
			Uploads: []imageUpload{{Type: "icon", Path: "/tmp/icon.png", Warnings: []string{"the app icon must be 512×512"}}},
			Deletes: []imageDeletion{{Type: "phoneScreenshots", ID: "old-1"}},
		},
		{Locale: "nl-NL"},
	}
	rendered := describeSyncPlan(plans)
	for _, want := range []string{"en-US", "title", "New", "upload icon.png", "delete phoneScreenshots image old-1", "512×512"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("plan description missing %q:\n%s", want, rendered)
		}
	}
	// A locale with nothing to do is not worth a line.
	if strings.Contains(rendered, "nl-NL") {
		t.Errorf("an empty locale plan should be omitted:\n%s", rendered)
	}

	if got := describeSyncPlan(nil); !strings.Contains(got, "nothing to do") {
		t.Errorf("an empty plan should say so: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("", 10); got != "(empty)" {
		t.Errorf("truncate(\"\") = %q", got)
	}
	if got := truncate("short", 10); got != `"short"` {
		t.Errorf("truncate = %q", got)
	}
	long := truncate(strings.Repeat("x", 100), 10)
	if !strings.HasSuffix(long, `…"`) {
		t.Errorf("a long value should be elided: %q", long)
	}
	// Newlines would break the one-line preview layout.
	if got := truncate("a\nb", 10); strings.Contains(got, "\n") {
		t.Errorf("newlines survived: %q", got)
	}
}
