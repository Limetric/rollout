package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listingWriteAPI serves the reads a listing preview makes and records the
// writes an apply performs.
type listingWriteAPI struct {
	listings map[string]apiListing
	images   map[string][]ImageInfo
	details  string

	puts    map[string]string
	patches map[string]string
	deletes []string
	uploads []string
	commits int
}

func (a *listingWriteAPI) handler(t *testing.T) *fakePlayAPI {
	t.Helper()
	a.puts, a.patches = map[string]string{}, map[string]string{}
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		path := r.URL.Path
		switch {
		case r.Method == http.MethodDelete && strings.HasSuffix(path, "/edits/edit-1"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			a.deletes = append(a.deletes, path)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, ":commit"):
			a.commits++
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.HasSuffix(path, ":validate"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.Contains(path, "/upload/"):
			a.uploads = append(a.uploads, path)
			w.Header().Set("Location", "http://"+r.Host+"/resumable-session")
			w.WriteHeader(http.StatusOK)
		case path == "/resumable-session":
			writeJSON(w, http.StatusOK, `{"id":"new-image","sha256":"new"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case r.Method == http.MethodPut:
			a.puts[path] = string(body)
			writeJSON(w, http.StatusOK, `{}`)
		case r.Method == http.MethodPatch:
			a.patches[path] = string(body)
			writeJSON(w, http.StatusOK, `{}`)
		case strings.HasSuffix(path, "/details"):
			writeJSON(w, http.StatusOK, a.detailsBody())
		case strings.Contains(path, "/listings/"):
			a.serveListingOrImages(w, path)
		case strings.HasSuffix(path, "/listings"):
			a.serveListings(w)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
}

func (a *listingWriteAPI) detailsBody() string {
	if a.details == "" {
		return `{"defaultLanguage":"en-US","contactEmail":"dev@example.com"}`
	}
	return a.details
}

func (a *listingWriteAPI) serveListings(w http.ResponseWriter) {
	var listings []apiListing
	for _, l := range a.listings {
		listings = append(listings, l)
	}
	body, _ := json.Marshal(map[string]any{"listings": listings})
	writeJSON(w, http.StatusOK, string(body))
}

func (a *listingWriteAPI) serveListingOrImages(w http.ResponseWriter, path string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// …/listings/<locale> or …/listings/<locale>/<imageType>
	idx := -1
	for i, part := range parts {
		if part == "listings" {
			idx = i
		}
	}
	if idx < 0 || idx+1 >= len(parts) {
		writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"Not found","status":"NOT_FOUND"}}`)
		return
	}
	locale := parts[idx+1]
	if idx+2 < len(parts) {
		imageType := parts[idx+2]
		var images []map[string]string
		for _, img := range a.images[locale] {
			if img.Type == imageType {
				images = append(images, map[string]string{"id": img.ID, "sha256": img.SHA256})
			}
		}
		body, _ := json.Marshal(map[string]any{"images": images})
		writeJSON(w, http.StatusOK, string(body))
		return
	}
	listing, ok := a.listings[locale]
	if !ok {
		writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"Not found","status":"NOT_FOUND"}}`)
		return
	}
	body, _ := json.Marshal(listing)
	writeJSON(w, http.StatusOK, string(body))
}

// TestUpdateListingMergesRatherThanReplaces: listings.update replaces the whole
// listing, so setting a title must not blank the description nobody mentioned.
func TestUpdateListingMergesRatherThanReplaces(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{listings: map[string]apiListing{
		"en-US": {Language: "en-US", Title: "Old", ShortDescription: "Short", FullDescription: "Long", Video: "https://youtu.be/x"},
	}}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUpdateListing(ctx, client, UpdateListingArgs{
		Locale: "en-US", Title: "New", supplied: map[string]bool{"title": true},
	})
	if err != nil {
		t.Fatalf("runUpdateListing: %v", err)
	}
	// The preview is a field diff so the reader can see exactly what moves.
	if !strings.Contains(preview.Preview, `"Old" → "New"`) {
		t.Errorf("preview should diff the field:\n%s", preview.Preview)
	}

	if _, err := applyConfirmed(ctx, client, "update_listing", preview.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	var wrote apiListing
	for path, body := range fake.puts {
		if strings.Contains(path, "/listings/en-US") {
			if err := json.Unmarshal([]byte(body), &wrote); err != nil {
				t.Fatalf("decode PUT: %v", err)
			}
		}
	}
	if wrote.Title != "New" {
		t.Errorf("title was not written: %+v", wrote)
	}
	for _, field := range []string{wrote.ShortDescription, wrote.FullDescription, wrote.Video} {
		if field == "" {
			t.Fatalf("a partial update blanked a field it did not mention: %+v", wrote)
		}
	}
}

func TestUpdateListingRejectsNoOpAndOverLength(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{listings: map[string]apiListing{"en-US": {Language: "en-US", Title: "Same"}}}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	_, err := runUpdateListing(ctx, client, UpdateListingArgs{
		Locale: "en-US", Title: "Same", supplied: map[string]bool{"title": true},
	})
	if err == nil || !strings.Contains(err.Error(), "already matches") {
		t.Errorf("err = %v, want a no-op to be reported rather than staged", err)
	}

	_, err = runUpdateListing(ctx, client, UpdateListingArgs{
		Locale: "de-DE", Title: strings.Repeat("x", 31), supplied: map[string]bool{"title": true},
	})
	if err == nil || !strings.Contains(err.Error(), "de-DE") {
		t.Errorf("err = %v, want one naming the locale", err)
	}

	if _, err := runUpdateListing(ctx, client, UpdateListingArgs{Locale: "en-US"}); err == nil {
		t.Error("a call that changes nothing should be rejected")
	}
	if _, err := runUpdateListing(ctx, client, UpdateListingArgs{Title: "x"}); err == nil {
		t.Error("locale should be required")
	}
}

// TestUpdateListingFromFileDistinguishesEmptyFromAbsent: a field present-but-
// empty in the file clears it; a field the file omits is left alone.
func TestUpdateListingFromFileDistinguishesEmptyFromAbsent(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{listings: map[string]apiListing{
		"en-US": {Language: "en-US", Title: "Old", Video: "https://youtu.be/x"},
	}}
	api := fake.handler(t)
	client := newTestClient(t, api)

	path := filepath.Join(t.TempDir(), "listing.json")
	if err := os.WriteFile(path, []byte(`{"title":"New","video":""}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	preview, err := runUpdateListing(context.Background(), client, UpdateListingArgs{Locale: "en-US", FromFile: path})
	if err != nil {
		t.Fatalf("runUpdateListing: %v", err)
	}
	if !strings.Contains(preview.Preview, "video") {
		t.Errorf("an explicitly empty field should show as a change:\n%s", preview.Preview)
	}
}

func TestDeleteListingTakesTwoConfirmations(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{listings: map[string]apiListing{"en-US": {Language: "en-US", Title: "Example"}}}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runDeleteListing(ctx, client, DeleteListingArgs{Locale: "en-US"})
	if err != nil {
		t.Fatalf("runDeleteListing: %v", err)
	}
	first, err := applyConfirmed(ctx, client, "delete_listing", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.Applied {
		t.Fatal("deleting a listing must take two confirmations")
	}
	if len(fake.deletes) != 0 {
		t.Fatal("nothing may be deleted before the second confirmation")
	}

	if _, err := applyConfirmed(ctx, client, "delete_listing", first.ConfirmToken); err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	if len(fake.deletes) != 1 || !strings.HasSuffix(fake.deletes[0], "/listings/en-US") {
		t.Errorf("unexpected deletions: %v", fake.deletes)
	}
}

// TestDeleteAllListingsNamesTheLocales: "delete everything" is worth spelling
// out before it happens.
func TestDeleteAllListingsNamesTheLocales(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{listings: map[string]apiListing{
		"en-US": {Language: "en-US"}, "nl-NL": {Language: "nl-NL"},
	}}
	api := fake.handler(t)
	client := newTestClient(t, api)

	preview, err := runDeleteListing(context.Background(), client, DeleteListingArgs{All: true})
	if err != nil {
		t.Fatalf("runDeleteListing: %v", err)
	}
	for _, want := range []string{"2 locales", "en-US", "nl-NL"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}

	_, err = runDeleteListing(context.Background(), client, DeleteListingArgs{Locale: "en-US", All: true})
	if err == nil {
		t.Error("--locale and --all together should be rejected")
	}
}

func TestUpdateDetailsPatchesOnlyWhatIsGiven(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUpdateDetails(ctx, client, UpdateDetailsArgs{ContactEmail: "new@example.com"})
	if err != nil {
		t.Fatalf("runUpdateDetails: %v", err)
	}
	if !strings.Contains(preview.Preview, "dev@example.com") {
		t.Errorf("preview should show the current value:\n%s", preview.Preview)
	}
	if _, err := applyConfirmed(ctx, client, "update_details", preview.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	var patched map[string]any
	for path, body := range fake.patches {
		if strings.HasSuffix(path, "/details") {
			_ = json.Unmarshal([]byte(body), &patched)
		}
	}
	if len(patched) != 1 || patched["contactEmail"] != "new@example.com" {
		t.Errorf("unexpected patch: %+v", patched)
	}

	if _, err := runUpdateDetails(ctx, client, UpdateDetailsArgs{}); err == nil {
		t.Error("a call that changes nothing should be rejected")
	}
}

func TestUploadImagesPreviewReportsDimensions(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	small := makePNG(t, filepath.Join(t.TempDir(), "icon.png"), 256, 256)
	preview, err := runUploadImages(ctx, client, UploadImagesArgs{
		Locale: "en-US", Type: "icon", Files: []string{small},
	})
	if err != nil {
		t.Fatalf("runUploadImages: %v", err)
	}
	for _, want := range []string{"icon.png", "png 256×256", "warning", "512×512"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}

	if _, err := applyConfirmed(ctx, client, "upload_images", preview.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(fake.uploads) != 1 || !strings.Contains(fake.uploads[0], "/listings/en-US/icon") {
		t.Errorf("unexpected uploads: %v", fake.uploads)
	}
}

// TestUploadImagesWarnsAboutTheResultingCount: uploads add to a type rather
// than replacing it, so the total is what has to fit Play's limit.
func TestUploadImagesWarnsAboutTheResultingCount(t *testing.T) {
	isolateState(t)
	existing := make([]ImageInfo, 7)
	for i := range existing {
		existing[i] = ImageInfo{Type: "phoneScreenshots", ID: "img", SHA256: "x"}
	}
	fake := &listingWriteAPI{images: map[string][]ImageInfo{"en-US": existing}}
	api := fake.handler(t)
	client := newTestClient(t, api)

	dir := t.TempDir()
	preview, err := runUploadImages(context.Background(), client, UploadImagesArgs{
		Locale: "en-US", Type: "phoneScreenshots",
		Files: []string{
			makePNG(t, filepath.Join(dir, "a.png"), 1080, 1920),
			makePNG(t, filepath.Join(dir, "b.png"), 1080, 1920),
		},
	})
	if err != nil {
		t.Fatalf("runUploadImages: %v", err)
	}
	if !strings.Contains(preview.Preview, "would hold 9") {
		t.Errorf("preview should warn about the resulting count:\n%s", preview.Preview)
	}
}

func TestUploadImagesRefusesAChangedFile(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	path := makePNG(t, filepath.Join(t.TempDir(), "shot.png"), 1080, 1920)
	preview, err := runUploadImages(ctx, client, UploadImagesArgs{
		Locale: "en-US", Type: "phoneScreenshots", Files: []string{path},
	})
	if err != nil {
		t.Fatalf("runUploadImages: %v", err)
	}
	makePNG(t, path, 1080, 1921) // the design tool ran again

	_, err = applyConfirmed(ctx, client, "upload_images", preview.ConfirmToken)
	if err == nil || !strings.Contains(err.Error(), "changed since it was previewed") {
		t.Fatalf("err = %v, want the changed file refused", err)
	}
}

func TestDeleteImagesAllTakesTwoConfirmations(t *testing.T) {
	isolateState(t)
	fake := &listingWriteAPI{images: map[string][]ImageInfo{
		"en-US": {{Type: "phoneScreenshots", ID: "img-1", SHA256: "a"}},
	}}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runDeleteImages(ctx, client, DeleteImagesArgs{Locale: "en-US", Type: "phoneScreenshots", All: true})
	if err != nil {
		t.Fatalf("runDeleteImages: %v", err)
	}
	first, err := applyConfirmed(ctx, client, "delete_images", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.Applied {
		t.Fatal("deleting every image of a type must take two confirmations")
	}

	// Deleting one image by id is a single confirmation.
	preview, err = runDeleteImages(ctx, client, DeleteImagesArgs{Locale: "en-US", Type: "phoneScreenshots", ID: "img-1"})
	if err != nil {
		t.Fatalf("runDeleteImages: %v", err)
	}
	res, err := applyConfirmed(ctx, client, "delete_images", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !res.Applied {
		t.Error("deleting a single image should apply on the first confirmation")
	}
}

func TestSyncListingAppliesEverythingInOneEdit(t *testing.T) {
	isolateState(t)
	dir := writeMetadataTree(t)
	fake := &listingWriteAPI{listings: map[string]apiListing{
		"en-US": {Language: "en-US", Title: "Older", ShortDescription: "A short one", FullDescription: "A long one"},
	}}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runSyncListing(ctx, client, SyncListingArgs{Dir: dir, Images: true})
	if err != nil {
		t.Fatalf("runSyncListing: %v", err)
	}
	for _, want := range []string{"en-US", "nl-NL", "upload icon.png", "one edit"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}

	if _, err := applyConfirmed(ctx, client, "sync_listing", preview.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// One edit for the whole sync, committed once.
	if fake.commits != 1 {
		t.Errorf("committed %d times, want 1", fake.commits)
	}
	var inserts int
	for _, call := range api.calls() {
		if call == "POST /androidpublisher/v3/applications/com.example.app/edits" {
			inserts++
		}
	}
	// One edit to read the current state, one to write.
	if inserts != 2 {
		t.Errorf("opened %d edits, want one to plan and one to apply", inserts)
	}
	if len(fake.uploads) != 3 {
		t.Errorf("uploaded %d images, want 3: %v", len(fake.uploads), fake.uploads)
	}
}

// TestSyncFailureCommitsNothing: a sync that updates six locales and fails on
// the seventh has left the store inconsistent in a way nobody asked for.
func TestSyncFailureCommitsNothing(t *testing.T) {
	isolateState(t)
	dir := writeMetadataTree(t)

	var commits int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, ":commit"):
			commits++
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/listings/nl-NL"):
			writeJSON(w, http.StatusBadRequest, `{"error":{"code":400,"message":"Invalid listing","status":"INVALID_ARGUMENT"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.HasSuffix(r.URL.Path, "/listings"):
			writeJSON(w, http.StatusOK, `{"listings":[]}`)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runSyncListing(ctx, client, SyncListingArgs{Dir: dir})
	if err != nil {
		t.Fatalf("runSyncListing: %v", err)
	}
	_, err = applyConfirmed(ctx, client, "sync_listing", preview.ConfirmToken)
	if err == nil {
		t.Fatal("expected the failing locale to fail the sync")
	}
	// "The sync failed" is not actionable; the locale is.
	if !strings.Contains(err.Error(), "nl-NL") {
		t.Errorf("error should name the failing locale: %v", err)
	}
	if commits != 0 {
		t.Error("a partially applied sync must not commit")
	}
	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("the aborted edit was not deleted: %v", api.calls())
	}
}

func TestSyncListingReportsNothingToDo(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "en-US", "title.txt"), "Example")

	fake := &listingWriteAPI{listings: map[string]apiListing{"en-US": {Language: "en-US", Title: "Example"}}}
	api := fake.handler(t)
	client := newTestClient(t, api)

	_, err := runSyncListing(context.Background(), client, SyncListingArgs{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "already matches") {
		t.Errorf("err = %v, want a no-op sync to say so", err)
	}
}

// TestSyncRefusesAFileChangedSincePreview: the whole plan is staged, including
// hashes, so a confirm minutes later cannot publish something nobody looked at.
func TestSyncRefusesAFileChangedSincePreview(t *testing.T) {
	isolateState(t)
	dir := writeMetadataTree(t)
	fake := &listingWriteAPI{}
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runSyncListing(ctx, client, SyncListingArgs{Dir: dir, Images: true})
	if err != nil {
		t.Fatalf("runSyncListing: %v", err)
	}
	makePNG(t, filepath.Join(dir, "en-US", "images", "icon.png"), 512, 513)

	_, err = applyConfirmed(ctx, client, "sync_listing", preview.ConfirmToken)
	if err == nil || !strings.Contains(err.Error(), "changed since it was previewed") {
		t.Fatalf("err = %v, want the changed image refused", err)
	}
}
