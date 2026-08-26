package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trackFixture is the state a release write reads before it stages: a track
// with a completed release still serving most users and a staged rollout in
// flight. Every clobber regression is caught by writing against this.
const trackFixture = `{"track":"production","releases":[
	{"name":"1.4.0","versionCodes":["41"],"status":"completed"},
	{"name":"1.5.0","versionCodes":["42"],"status":"inProgress","userFraction":0.1,
	 "releaseNotes":[{"language":"en-US","text":"Bug fixes"}]}
]}`

// releaseWriteAPI serves the track read a preview does and captures the PUT an
// apply makes.
func releaseWriteAPI(t *testing.T, current string, written *track) *fakePlayAPI {
	t.Helper()
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.HasSuffix(r.URL.Path, ":validate") || strings.HasSuffix(r.URL.Path, ":commit"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tracks/"):
			writeJSON(w, http.StatusOK, current)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tracks"):
			writeJSON(w, http.StatusOK, `{"tracks":[`+current+`]}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/tracks/"):
			if written != nil {
				if err := json.NewDecoder(r.Body).Decode(written); err != nil {
					t.Errorf("decode track PUT: %v", err)
				}
			}
			writeJSON(w, http.StatusOK, `{"track":"production"}`)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
}

// applyPreview confirms a staged write through the per-tool confirm path.
func applyPreview(t *testing.T, ctx context.Context, c *Client, tool, token string) WriteResult {
	t.Helper()
	res, err := applyConfirmed(ctx, c, tool, token)
	if err != nil {
		t.Fatalf("confirm %s: %v", tool, err)
	}
	return res
}

func float64Ptr(f float64) *float64 { return &f }

// TestUpdateReleaseKeepsOtherReleases is the regression the whole design exists
// for: the reference implementation this was modelled against rewrote the whole
// releases[] array and silently dropped whatever it did not mention.
func TestUpdateReleaseKeepsOtherReleases(t *testing.T) {
	isolateState(t)
	var written track
	api := releaseWriteAPI(t, trackFixture, &written)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUpdateRelease(ctx, client, UpdateReleaseArgs{
		Track: "production", VersionCodes: []string{"42"},
		releaseFields: releaseFields{Rollout: float64Ptr(0.5)},
	})
	if err != nil {
		t.Fatalf("runUpdateRelease: %v", err)
	}
	if preview.Applied {
		t.Fatal("a preview must not apply")
	}
	// The preview says what is there now, so the reader can tell they are
	// dialling the release they meant.
	if !strings.Contains(preview.Preview, "10% of users") {
		t.Errorf("preview should describe the current release:\n%s", preview.Preview)
	}
	if !strings.Contains(preview.Preview, "50%") {
		t.Errorf("preview should describe the change:\n%s", preview.Preview)
	}

	res := applyPreview(t, ctx, client, "update_release", preview.ConfirmToken)
	if !res.Applied {
		t.Fatal("expected the confirm to apply")
	}

	if len(written.Releases) != 2 {
		t.Fatalf("wrote %d releases, want both kept: %s", len(written.Releases), mustJSON(t, written))
	}
	if !strings.Contains(string(written.Releases[0]), `"41"`) {
		t.Errorf("the completed release was dropped: %s", mustJSON(t, written))
	}
	if !strings.Contains(string(written.Releases[1]), "0.5") {
		t.Errorf("the rollout was not changed: %s", written.Releases[1])
	}
	// A patch changes only the named fields; release notes nobody mentioned
	// must survive.
	if !strings.Contains(string(written.Releases[1]), "Bug fixes") {
		t.Errorf("a patch discarded fields it was not asked about: %s", written.Releases[1])
	}
}

// TestUpdateReleaseWithoutVersionCodesRefusesToGuess: picking one of several
// releases to change would be the worst possible default for a publishing tool.
func TestUpdateReleaseWithoutVersionCodesRefusesToGuess(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUpdateRelease(ctx, client, UpdateReleaseArgs{
		Track: "production", releaseFields: releaseFields{Rollout: float64Ptr(0.5)},
	})
	if err != nil {
		t.Fatalf("runUpdateRelease: %v", err)
	}
	_, err = applyConfirmed(ctx, client, "update_release", preview.ConfirmToken)
	if err == nil {
		t.Fatal("expected an ambiguous patch to fail")
	}
	if !strings.Contains(err.Error(), "--version-codes") {
		t.Errorf("error should say how to disambiguate: %v", err)
	}
}

// TestUpdateReleaseWithASingleReleaseNeedsNoVersionCodes: the common case is a
// track with one release, and making people look up a version code first would
// be busywork.
func TestUpdateReleaseWithASingleReleaseNeedsNoVersionCodes(t *testing.T) {
	isolateState(t)
	var written track
	api := releaseWriteAPI(t, `{"track":"beta","releases":[{"versionCodes":["42"],"status":"inProgress","userFraction":0.1}]}`, &written)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUpdateRelease(ctx, client, UpdateReleaseArgs{
		Track: "beta", releaseFields: releaseFields{Rollout: float64Ptr(0.25)},
	})
	if err != nil {
		t.Fatalf("runUpdateRelease: %v", err)
	}
	applyPreview(t, ctx, client, "update_release", preview.ConfirmToken)
	if !strings.Contains(string(written.Releases[0]), "0.25") {
		t.Errorf("the rollout was not changed: %s", mustJSON(t, written))
	}
}

// TestRolloutOfOneBecomesCompleted: "--rollout 1" is what people type when they
// mean "ship it to everyone", and the API rejects it outright.
func TestRolloutOfOneBecomesCompleted(t *testing.T) {
	isolateState(t)
	var written track
	api := releaseWriteAPI(t, trackFixture, &written)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUpdateRelease(ctx, client, UpdateReleaseArgs{
		Track: "beta", VersionCodes: []string{"42"},
		releaseFields: releaseFields{Rollout: float64Ptr(1)},
	})
	if err != nil {
		t.Fatalf("runUpdateRelease: %v", err)
	}
	if !strings.Contains(preview.Preview, statusCompleted) {
		t.Errorf("preview should say the release completes:\n%s", preview.Preview)
	}
	applyPreview(t, ctx, client, "update_release", preview.ConfirmToken)

	body := string(written.Releases[1])
	if !strings.Contains(body, statusCompleted) {
		t.Errorf("status was not set to completed: %s", body)
	}
	// The API rejects `completed` carrying a fraction, so completing means
	// removing userFraction rather than setting it to 1.
	if strings.Contains(body, "userFraction") {
		t.Errorf("a completed release must not carry a user fraction: %s", body)
	}
}

func TestCompleteReleaseClearsTheFraction(t *testing.T) {
	isolateState(t)
	var written track
	api := releaseWriteAPI(t, trackFixture, &written)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runCompleteRelease(ctx, client, ReleaseStatusArgs{Track: "beta", VersionCodes: []string{"42"}})
	if err != nil {
		t.Fatalf("runCompleteRelease: %v", err)
	}
	applyPreview(t, ctx, client, "complete_release", preview.ConfirmToken)
	body := string(written.Releases[1])
	if !strings.Contains(body, statusCompleted) || strings.Contains(body, "userFraction") {
		t.Errorf("unexpected release after complete: %s", body)
	}
}

// TestCompleteOnProductionTakesTwoConfirmations: every remaining user gets the
// release and there is no lower fraction to fall back to.
func TestCompleteOnProductionTakesTwoConfirmations(t *testing.T) {
	isolateState(t)
	var written track
	api := releaseWriteAPI(t, trackFixture, &written)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runCompleteRelease(ctx, client, ReleaseStatusArgs{Track: productionTrack, VersionCodes: []string{"42"}})
	if err != nil {
		t.Fatalf("runCompleteRelease: %v", err)
	}
	first, err := applyConfirmed(ctx, client, "complete_release", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.Applied {
		t.Fatal("completing a production rollout must not apply on the first confirmation")
	}
	if !strings.Contains(first.Preview, "DESTRUCTIVE") {
		t.Errorf("the second-confirmation preview should say why:\n%s", first.Preview)
	}
	if len(written.Releases) != 0 {
		t.Fatal("nothing may be written before the second confirmation")
	}

	second := applyPreview(t, ctx, client, "complete_release", first.ConfirmToken)
	if !second.Applied {
		t.Fatal("the second confirmation should have applied")
	}
}

// TestHaltTakesTwoConfirmationsOnAnyTrack: a halt pulls a release from users
// who already have it, and resuming does not undo the time it was gone.
func TestHaltTakesTwoConfirmationsOnAnyTrack(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runHaltRelease(ctx, client, ReleaseStatusArgs{Track: "beta", VersionCodes: []string{"42"}})
	if err != nil {
		t.Fatalf("runHaltRelease: %v", err)
	}
	first, err := applyConfirmed(ctx, client, "halt_release", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.Applied {
		t.Fatal("a halt must take two confirmations")
	}
}

func TestCreateRelease(t *testing.T) {
	isolateState(t)
	var written track
	api := releaseWriteAPI(t, trackFixture, &written)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runCreateRelease(ctx, client, CreateReleaseArgs{
		Track: "beta", VersionCodes: []string{"43"},
		releaseFields: releaseFields{
			Status: statusInProgress, Rollout: float64Ptr(0.1),
			ReleaseName: "1.6.0", Notes: []string{"en-US=New things"},
		},
	})
	if err != nil {
		t.Fatalf("runCreateRelease: %v", err)
	}
	for _, want := range []string{"beta", "43", "10%", "1.6.0", "en-US"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}

	applyPreview(t, ctx, client, "create_release", preview.ConfirmToken)
	// Appended, not substituted: the two existing releases stay.
	if len(written.Releases) != 3 {
		t.Fatalf("wrote %d releases: %s", len(written.Releases), mustJSON(t, written))
	}
	if !strings.Contains(string(written.Releases[2]), "New things") {
		t.Errorf("the new release is wrong: %s", written.Releases[2])
	}
}

func TestCreateReleaseDefaultsToDraft(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)

	// An artifact existing on a track and an artifact reaching users are
	// separate decisions.
	preview, err := runCreateRelease(context.Background(), client, CreateReleaseArgs{
		Track: "beta", VersionCodes: []string{"43"},
	})
	if err != nil {
		t.Fatalf("runCreateRelease: %v", err)
	}
	if !strings.Contains(preview.Preview, statusDraft) {
		t.Errorf("preview should show a draft release:\n%s", preview.Preview)
	}
}

func TestCreateReleaseValidatesClientSide(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)
	ctx := context.Background()

	tests := []struct {
		name    string
		args    CreateReleaseArgs
		wantErr string
	}{
		{"no track", CreateReleaseArgs{VersionCodes: []string{"43"}}, "track is required"},
		{"no version codes", CreateReleaseArgs{Track: "beta"}, "version code"},
		{
			name:    "a staged rollout needs a fraction",
			args:    CreateReleaseArgs{Track: "beta", VersionCodes: []string{"43"}, releaseFields: releaseFields{Status: statusInProgress}},
			wantErr: "--rollout",
		},
		{
			name:    "a completed release takes no fraction",
			args:    CreateReleaseArgs{Track: "beta", VersionCodes: []string{"43"}, releaseFields: releaseFields{Status: statusCompleted, Rollout: float64Ptr(0.5)}},
			wantErr: "does not take",
		},
		{
			// Play rejects the whole commit for one over-length locale, with a
			// message that names none of them.
			name: "over-length release notes name the locale",
			args: CreateReleaseArgs{Track: "beta", VersionCodes: []string{"43"},
				releaseFields: releaseFields{Notes: []string{"de-DE=" + strings.Repeat("x", 501)}}},
			wantErr: "de-DE",
		},
		{
			name: "notes and notes-dir are exclusive",
			args: CreateReleaseArgs{Track: "beta", VersionCodes: []string{"43"},
				releaseFields: releaseFields{Notes: []string{"en-US=x"}, NotesDir: "/tmp"}},
			wantErr: "not both",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCreateRelease(ctx, client, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCreateReleaseReadsNotesFromADirectory(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "en-US.txt"), "Bug fixes")
	writeFile(t, filepath.Join(dir, "nl-NL.txt"), "Foutoplossingen")

	preview, err := runCreateRelease(context.Background(), client, CreateReleaseArgs{
		Track: "beta", VersionCodes: []string{"43"},
		releaseFields: releaseFields{NotesDir: dir},
	})
	if err != nil {
		t.Fatalf("runCreateRelease: %v", err)
	}
	for _, want := range []string{"en-US", "nl-NL"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview should list the note locales (%q):\n%s", want, preview.Preview)
		}
	}
}

func TestPromoteRelease(t *testing.T) {
	isolateState(t)
	var written track
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.Contains(r.URL.Path, "/tracks/internal"):
			writeJSON(w, http.StatusOK, `{"track":"internal","releases":[
				{"name":"1.5.0","versionCodes":["42"],"status":"completed","inAppUpdatePriority":2,
				 "releaseNotes":[{"language":"en-US","text":"Bug fixes"}]},
				{"name":"1.4.0","versionCodes":["41"],"status":"completed"}
			]}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tracks/beta"):
			writeJSON(w, http.StatusOK, `{"track":"beta","releases":[]}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/tracks/beta"):
			_ = json.NewDecoder(r.Body).Decode(&written)
			writeJSON(w, http.StatusOK, `{}`)
		default:
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		}
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runPromoteRelease(ctx, client, PromoteReleaseArgs{FromTrack: "internal", ToTrack: "beta"})
	if err != nil {
		t.Fatalf("runPromoteRelease: %v", err)
	}
	// Highest version code, not whatever the API listed first: "promote what
	// we just tested" means the newest build.
	if !strings.Contains(preview.Preview, "42") {
		t.Errorf("preview should promote the newest release:\n%s", preview.Preview)
	}
	applyPreview(t, ctx, client, "promote_release", preview.ConfirmToken)

	body := string(written.Releases[0])
	// A promotion ships the thing that was tested, so notes, name and priority
	// come across untouched.
	for _, want := range []string{"Bug fixes", "1.5.0", "inAppUpdatePriority"} {
		if !strings.Contains(body, want) {
			t.Errorf("promotion dropped %q: %s", want, body)
		}
	}
	// Promoting to a testing track ships it; only production defaults to draft.
	if !strings.Contains(body, statusCompleted) {
		t.Errorf("promotion to beta should complete: %s", body)
	}
}

func TestPromoteToProductionDefaultsToDraft(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, `{"track":"beta","releases":[{"versionCodes":["42"],"status":"completed"}]}`, nil)
	client := newTestClient(t, api)

	preview, err := runPromoteRelease(context.Background(), client, PromoteReleaseArgs{FromTrack: "beta", ToTrack: productionTrack})
	if err != nil {
		t.Fatalf("runPromoteRelease: %v", err)
	}
	// An unreviewed automatic rollout is the wrong default for production.
	if !strings.Contains(preview.Preview, statusDraft) {
		t.Errorf("promotion to production should default to draft:\n%s", preview.Preview)
	}
}

func TestPromoteRejectsBadArguments(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)
	ctx := context.Background()

	if _, err := runPromoteRelease(ctx, client, PromoteReleaseArgs{FromTrack: "beta"}); err == nil {
		t.Error("both tracks should be required")
	}
	_, err := runPromoteRelease(ctx, client, PromoteReleaseArgs{FromTrack: "beta", ToTrack: "beta"})
	if err == nil || !strings.Contains(err.Error(), "between tracks") {
		t.Errorf("err = %v, want one explaining a promotion moves between tracks", err)
	}
}

func TestSetTestersDiffsTheReplacement(t *testing.T) {
	isolateState(t)
	var putBody string
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/testers/"):
			writeJSON(w, http.StatusOK, `{"googleGroups":["old@googlegroups.com","keep@googlegroups.com"]}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/testers/"):
			body := make([]byte, 1024)
			n, _ := r.Body.Read(body)
			putBody = string(body[:n])
			writeJSON(w, http.StatusOK, `{}`)
		default:
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		}
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runSetTesters(ctx, client, SetTestersArgs{
		Track: "internal", Groups: []string{"keep@googlegroups.com,new@googlegroups.com"},
	})
	if err != nil {
		t.Fatalf("runSetTesters: %v", err)
	}
	// The API replaces the whole list; a preview that did not say what it drops
	// is exactly how a full-replacement endpoint bites.
	if !strings.Contains(preview.Preview, "removing old@googlegroups.com") {
		t.Errorf("preview should name the removed group:\n%s", preview.Preview)
	}
	if !strings.Contains(preview.Preview, "adding new@googlegroups.com") {
		t.Errorf("preview should name the added group:\n%s", preview.Preview)
	}

	applyPreview(t, ctx, client, "set_testers", preview.ConfirmToken)
	if !strings.Contains(putBody, "new@googlegroups.com") || strings.Contains(putBody, "old@googlegroups.com") {
		t.Errorf("unexpected testers PUT: %s", putBody)
	}
}

func TestNormalizeGoogleGroups(t *testing.T) {
	groups, err := normalizeGoogleGroups([]string{"b@googlegroups.com, a@googlegroups.com", "a@googlegroups.com"})
	if err != nil {
		t.Fatalf("normalizeGoogleGroups: %v", err)
	}
	if len(groups) != 2 || groups[0] != "a@googlegroups.com" {
		t.Fatalf("unexpected groups: %v", groups)
	}
	// The API accepts a non-group address and then tests nothing with it.
	if _, err := normalizeGoogleGroups([]string{"qa-team"}); err == nil {
		t.Error("a non-address should be rejected")
	}
	if _, err := normalizeGoogleGroups(nil); err == nil {
		t.Error("an empty list should be rejected")
	}
}

func TestSetCountriesPatchesTheRelease(t *testing.T) {
	isolateState(t)
	var written track
	api := releaseWriteAPI(t, trackFixture, &written)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runSetCountries(ctx, client, SetCountriesArgs{
		Track: "production", VersionCodes: []string{"42"},
		Countries: []string{"nl,de"}, RestOfWorld: true,
	})
	if err != nil {
		t.Fatalf("runSetCountries: %v", err)
	}
	if !strings.Contains(preview.Preview, "DE, NL") {
		t.Errorf("preview should name the countries:\n%s", preview.Preview)
	}
	applyPreview(t, ctx, client, "set_countries", preview.ConfirmToken)

	body := string(written.Releases[1])
	if !strings.Contains(body, "countryTargeting") || !strings.Contains(body, "includeRestOfWorld") {
		t.Errorf("country targeting was not written: %s", body)
	}
	// Only the targeted release changes.
	if strings.Contains(string(written.Releases[0]), "countryTargeting") {
		t.Errorf("the other release was modified: %s", written.Releases[0])
	}
}

func TestNormalizeCountryCodes(t *testing.T) {
	codes, err := normalizeCountryCodes([]string{"nl, de", "NL"})
	if err != nil {
		t.Fatalf("normalizeCountryCodes: %v", err)
	}
	if len(codes) != 2 || codes[0] != "DE" || codes[1] != "NL" {
		t.Fatalf("unexpected codes: %v", codes)
	}
	for _, bad := range []string{"Netherlands", "N1", "n"} {
		if _, err := normalizeCountryCodes([]string{bad}); err == nil {
			t.Errorf("%q should have been rejected", bad)
		}
	}
}

// TestRolloutCapAppliesToReleaseWrites: the guard has to see the fraction a
// release write is asking for, which means the staging call has to declare it.
func TestRolloutCapAppliesToReleaseWrites(t *testing.T) {
	isolateState(t)
	t.Setenv("PLAY_MAX_ROLLOUT_FRACTION", "0.2")
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)

	_, err := runUpdateRelease(context.Background(), client, UpdateReleaseArgs{
		Track: "production", VersionCodes: []string{"42"},
		releaseFields: releaseFields{Rollout: float64Ptr(0.5)},
	})
	if err == nil || !strings.Contains(err.Error(), "max_rollout_fraction") {
		t.Fatalf("err = %v, want the cap to refuse the write", err)
	}

	// A completed release is also over the cap: it reaches every user.
	_, err = runCompleteRelease(context.Background(), client, ReleaseStatusArgs{Track: productionTrack, VersionCodes: []string{"42"}})
	if err == nil {
		t.Error("completing a rollout should be refused under a cap")
	}
}

func TestUploadArtifactPreview(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)

	path := writeTempFile(t, "app.aab", 4096)
	preview, err := runUploadArtifact(context.Background(), client, UploadArtifactArgs{File: path})
	if err != nil {
		t.Fatalf("runUploadArtifact: %v", err)
	}
	// The preview identifies the artifact by hash, which is also what the
	// apply checks against.
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("fileSHA256: %v", err)
	}
	for _, want := range []string{"app.aab", "bundle", "4.0 KiB", sum[:12], "internal", statusDraft} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}
}

func TestUploadArtifactRejectsBadInput(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)
	ctx := context.Background()

	if _, err := runUploadArtifact(ctx, client, UploadArtifactArgs{}); err == nil {
		t.Error("file should be required")
	}
	// Sending a bundle to the APK endpoint fails with a message that mentions
	// neither the file nor the extension.
	_, err := runUploadArtifact(ctx, client, UploadArtifactArgs{File: writeTempFile(t, "app.ipa", 10)})
	if err == nil || !strings.Contains(err.Error(), ".aab") {
		t.Errorf("err = %v, want one naming the accepted extensions", err)
	}
	_, err = runUploadArtifact(ctx, client, UploadArtifactArgs{
		File: writeTempFile(t, "app.aab", 10), NoRelease: true, Track: "beta",
	})
	if err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Errorf("err = %v, want the contradiction reported", err)
	}
}

// TestUploadRefusesAnArtifactThatChangedSincePreview: a confirm token outlives
// the command that produced it, and the obvious way to spend that window is
// another build.
func TestUploadRefusesAnArtifactThatChangedSincePreview(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)
	ctx := context.Background()

	path := writeTempFile(t, "app.aab", 4096)
	preview, err := runUploadArtifact(ctx, client, UploadArtifactArgs{File: path, NoRelease: true})
	if err != nil {
		t.Fatalf("runUploadArtifact: %v", err)
	}
	if err := os.WriteFile(path, []byte("a different build entirely"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	_, err = applyConfirmed(ctx, client, "upload_artifact", preview.ConfirmToken)
	if err == nil {
		t.Fatal("expected the changed artifact to be refused")
	}
	if !strings.Contains(err.Error(), "changed since it was previewed") {
		t.Errorf("error should say what happened: %v", err)
	}
}

func TestUploadDeobfuscationValidatesInput(t *testing.T) {
	isolateState(t)
	api := releaseWriteAPI(t, trackFixture, nil)
	client := newTestClient(t, api)
	ctx := context.Background()

	path := writeTempFile(t, "mapping.txt", 128)
	if _, err := runUploadDeobfuscation(ctx, client, UploadDeobfuscationArgs{File: path, VersionCode: "1.2.3"}); err == nil {
		t.Error("a version name should be rejected where a version code belongs")
	}
	_, err := runUploadDeobfuscation(ctx, client, UploadDeobfuscationArgs{File: path, VersionCode: "42", Type: "symbols"})
	if err == nil || !strings.Contains(err.Error(), "nativeCode") {
		t.Errorf("err = %v, want one listing the valid types", err)
	}

	preview, err := runUploadDeobfuscation(ctx, client, UploadDeobfuscationArgs{File: path, VersionCode: "42"})
	if err != nil {
		t.Fatalf("runUploadDeobfuscation: %v", err)
	}
	for _, want := range []string{"mapping.txt", "42", "proguard"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{4096, "4.0 KiB"},
		{5 << 20, "5.0 MiB"},
		{3 << 30, "3.0 GiB"},
	}
	for _, tc := range tests {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
