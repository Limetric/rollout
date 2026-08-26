package main

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// playReadAPI answers a read tool's calls: the edit lifecycle plus whatever
// routes the test supplies, keyed by a substring of the request path.
func playReadAPI(t *testing.T, routes map[string]string) *fakePlayAPI {
	t.Helper()
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits") {
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
			return
		}
		for fragment, body := range routes {
			if strings.Contains(r.URL.Path, fragment) {
				writeJSON(w, http.StatusOK, body)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"Not found","status":"NOT_FOUND"}}`)
	})
}

// assertEditDeleted is the invariant every edit-based read has to satisfy:
// leaving edits behind burns quota and litters the app's edit list.
func assertEditDeleted(t *testing.T, api *fakePlayAPI) {
	t.Helper()
	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("the read did not delete its edit; calls were %v", api.calls())
	}
}

func TestRunTracks(t *testing.T) {
	const allTracks = `{"tracks":[
		{"track":"production","releases":[
			{"name":"1.4.0","versionCodes":["41"],"status":"completed"},
			{"name":"1.5.0","versionCodes":["42"],"status":"inProgress","userFraction":0.1,"inAppUpdatePriority":3,
			 "releaseNotes":[{"language":"en-US","text":"Bug fixes"},{"language":"nl-NL","text":"Foutoplossingen"}]}
		]},
		{"track":"internal","releases":[{"versionCodes":["43"],"status":"draft"}]}
	]}`

	t.Run("lists every track", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{"/tracks": allTracks})
		client := newTestClient(t, api)

		res, err := runTracks(context.Background(), client, TracksArgs{})
		if err != nil {
			t.Fatalf("runTracks: %v", err)
		}
		if res.PackageName != "com.example.app" || len(res.Tracks) != 2 {
			t.Fatalf("unexpected result: %+v", res)
		}

		staged := res.Tracks[0].Releases[1]
		// userFraction is what the API returns; percent is what the Console
		// shows and what people say out loud.
		if staged.RolloutPercent == nil || *staged.RolloutPercent != 10 {
			t.Errorf("rollout percent = %v, want 10", staged.RolloutPercent)
		}
		if staged.InAppUpdatePriority != 3 {
			t.Errorf("in-app update priority = %d", staged.InAppUpdatePriority)
		}
		if len(staged.ReleaseNoteLocales) != 2 || staged.ReleaseNoteLocales[0] != "en-US" {
			t.Errorf("release note locales = %v", staged.ReleaseNoteLocales)
		}
		// The wire object survives, so an agent can read fields this tool has
		// no opinion about.
		if !strings.Contains(string(staged.Raw), "Bug fixes") {
			t.Errorf("raw release was dropped: %s", staged.Raw)
		}
		// A completed release carries no userFraction, and inventing 0% for it
		// would read as "nobody has this".
		if res.Tracks[0].Releases[0].RolloutPercent != nil {
			t.Error("a completed release should have no rollout percentage")
		}
		assertEditDeleted(t, api)
	})

	t.Run("narrows to one track", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{
			"/tracks/production": `{"track":"production","releases":[{"versionCodes":["42"],"status":"completed"}]}`,
		})
		client := newTestClient(t, api)

		res, err := runTracks(context.Background(), client, TracksArgs{Track: "production"})
		if err != nil {
			t.Fatalf("runTracks: %v", err)
		}
		if len(res.Tracks) != 1 || res.Tracks[0].Track != "production" {
			t.Fatalf("unexpected result: %+v", res)
		}
		// The narrowed read must hit the single-track endpoint, not list them
		// all and filter — a track list can be large and it costs quota.
		if !api.sawCall(http.MethodGet, "/androidpublisher/v3/applications/com.example.app/edits/edit-1/tracks/production") {
			t.Errorf("expected a single-track GET, calls were %v", api.calls())
		}
	})

	t.Run("keeps a release it cannot parse", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{
			"/tracks": `{"tracks":[{"track":"production","releases":[{"somethingNew":true}]}]}`,
		})
		client := newTestClient(t, api)

		res, err := runTracks(context.Background(), client, TracksArgs{})
		if err != nil {
			t.Fatalf("runTracks: %v", err)
		}
		// Dropping it would hide a live release because we did not recognize
		// a field.
		if len(res.Tracks[0].Releases) != 1 {
			t.Fatalf("an unrecognized release was dropped: %+v", res)
		}
	})

	t.Run("table output names the columns a human scans", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{"/tracks": allTracks})
		client := newTestClient(t, api)
		res, err := runTracks(context.Background(), client, TracksArgs{})
		if err != nil {
			t.Fatalf("runTracks: %v", err)
		}

		var buf strings.Builder
		if err := printResult(&buf, "table", res); err != nil {
			t.Fatalf("printResult: %v", err)
		}
		for _, want := range []string{"track", "status", "version_codes", "rollout_percent", "production", "inProgress", "10"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("table output missing %q:\n%s", want, buf.String())
			}
		}
	})
}

// TestReadToolsRequireAPackage: every read resolves the app the same way, and
// the error has to name every way to supply one.
func TestReadToolsRequireAPackage(t *testing.T) {
	api := playReadAPI(t, nil)
	clearPlayEnv(t)
	cfg := &PlayConfig{}
	cfg.BaseURL, cfg.ReportingBaseURL = api.URL, api.URL
	client, err := NewClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = runTracks(context.Background(), client, TracksArgs{})
	if err == nil {
		t.Fatal("expected an error with no package configured")
	}
	for _, want := range []string{"--package", "PLAY_PACKAGE_NAME", "set-package"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if len(api.calls()) != 0 {
		t.Errorf("nothing should reach the API without a package: %v", api.calls())
	}
}

func TestRunReleasesPrefersTheEditFreeEndpoint(t *testing.T) {
	api := playReadAPI(t, map[string]string{
		"/applications/com.example.app/tracks": `{"tracks":[
			{"track":"production","releases":[{"versionCodes":["42"],"status":"completed"}]},
			{"track":"beta","releases":[{"versionCodes":["43"],"status":"inProgress","userFraction":0.5}]}
		]}`,
	})
	client := newTestClient(t, api)

	res, err := runReleases(context.Background(), client, ReleasesArgs{})
	if err != nil {
		t.Fatalf("runReleases: %v", err)
	}
	if len(res.Releases) != 2 {
		t.Fatalf("got %d releases: %+v", len(res.Releases), res)
	}
	// No edit at all: this listing costs nothing against the edit quota.
	for _, call := range api.calls() {
		if strings.Contains(call, "/edits") {
			t.Errorf("the edit-free listing opened an edit: %v", api.calls())
		}
	}
}

// TestRunReleasesFallsBackToAnEdit: not every app exposes the edit-free
// listing, and the answer must not depend on that.
func TestRunReleasesFallsBackToAnEdit(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.HasSuffix(r.URL.Path, "/edits/edit-1/tracks"):
			writeJSON(w, http.StatusOK, `{"tracks":[{"track":"beta","releases":[{"versionCodes":["43"],"status":"draft"}]}]}`)
		default:
			writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"Not found","status":"NOT_FOUND"}}`)
		}
	})
	client := newTestClient(t, api)

	res, err := runReleases(context.Background(), client, ReleasesArgs{})
	if err != nil {
		t.Fatalf("runReleases: %v", err)
	}
	if len(res.Releases) != 1 || res.Releases[0].Track != "beta" {
		t.Fatalf("unexpected releases: %+v", res)
	}
	assertEditDeleted(t, api)
}

func TestRunReleasesFiltersByTrack(t *testing.T) {
	api := playReadAPI(t, map[string]string{
		"/applications/com.example.app/tracks": `{"tracks":[
			{"track":"production","releases":[{"versionCodes":["42"],"status":"completed"}]},
			{"track":"beta","releases":[{"versionCodes":["43"],"status":"draft"}]}
		]}`,
	})
	client := newTestClient(t, api)

	res, err := runReleases(context.Background(), client, ReleasesArgs{Track: "beta"})
	if err != nil {
		t.Fatalf("runReleases: %v", err)
	}
	if len(res.Releases) != 1 || res.Releases[0].Track != "beta" {
		t.Errorf("--track did not filter: %+v", res)
	}
}

func TestRunArtifacts(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/bundles"):
			writeJSON(w, http.StatusOK, `{"bundles":[{"versionCode":41,"sha256":"aaa"},{"versionCode":43,"sha256":"ccc"}]}`)
		case strings.HasSuffix(r.URL.Path, "/apks"):
			writeJSON(w, http.StatusOK, `{"apks":[{"versionCode":42,"binary":{"sha256":"bbb","sha1":"b1"}}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
	client := newTestClient(t, api)

	res, err := runArtifacts(context.Background(), client, ArtifactsArgs{})
	if err != nil {
		t.Fatalf("runArtifacts: %v", err)
	}
	if len(res.Artifacts) != 3 {
		t.Fatalf("got %d artifacts: %+v", len(res.Artifacts), res)
	}
	// Newest first: the version code someone is about to release is the
	// highest one, and it should not be at the bottom of a long list.
	if res.Artifacts[0].VersionCode != 43 || res.Artifacts[2].VersionCode != 41 {
		t.Errorf("artifacts are not newest-first: %+v", res.Artifacts)
	}
	// Bundles and APKs come from different endpoints with different hash
	// fields; the caller should not have to know that.
	byCode := map[int64]ArtifactInfo{}
	for _, a := range res.Artifacts {
		byCode[a.VersionCode] = a
	}
	if byCode[43].Type != "bundle" || byCode[42].Type != "apk" || byCode[42].SHA256 != "bbb" {
		t.Errorf("artifact types or hashes are wrong: %+v", res.Artifacts)
	}
	// Both listings share one edit, not two.
	assertEditDeleted(t, api)
	var inserts int
	for _, call := range api.calls() {
		if call == "POST /androidpublisher/v3/applications/com.example.app/edits" {
			inserts++
		}
	}
	if inserts != 1 {
		t.Errorf("opened %d edits, want 1", inserts)
	}
}

func TestRunListing(t *testing.T) {
	t.Run("all locales, sorted", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{
			"/listings": `{"listings":[
				{"language":"nl-NL","title":"Voorbeeld","shortDescription":"Kort"},
				{"language":"en-US","title":"Example","shortDescription":"Short","fullDescription":"Long","video":"https://youtu.be/x"}
			]}`,
		})
		client := newTestClient(t, api)

		res, err := runListing(context.Background(), client, ListingArgs{})
		if err != nil {
			t.Fatalf("runListing: %v", err)
		}
		if len(res.Listings) != 2 {
			t.Fatalf("got %+v", res)
		}
		// Sorted, so a diff between two runs is a diff in the data rather than
		// in whatever order the API happened to answer in.
		if res.Listings[0].Language != "en-US" {
			t.Errorf("listings are not sorted: %+v", res.Listings)
		}
		if res.Listings[0].Video != "https://youtu.be/x" {
			t.Errorf("video was dropped: %+v", res.Listings[0])
		}
		assertEditDeleted(t, api)
	})

	t.Run("a locale with no listing is an answer", func(t *testing.T) {
		// This is exactly what a caller asks before creating one, so a 404 must
		// not read as a failure.
		api := playReadAPI(t, nil)
		client := newTestClient(t, api)

		res, err := runListing(context.Background(), client, ListingArgs{Locale: "fr-FR"})
		if err != nil {
			t.Fatalf("a missing locale should not be an error: %v", err)
		}
		if len(res.Listings) != 0 {
			t.Errorf("expected no listings, got %+v", res.Listings)
		}
	})

	t.Run("a locale with no language field falls back to the request", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{"/listings/en-US": `{"title":"Example"}`})
		client := newTestClient(t, api)

		res, err := runListing(context.Background(), client, ListingArgs{Locale: "en-US"})
		if err != nil {
			t.Fatalf("runListing: %v", err)
		}
		if len(res.Listings) != 1 || res.Listings[0].Language != "en-US" {
			t.Errorf("unexpected listing: %+v", res.Listings)
		}
	})
}

func TestRunImages(t *testing.T) {
	t.Run("all eight types in one edit", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{
			"/phoneScreenshots": `{"images":[{"id":"img-1","sha256":"aaa","url":"https://example/1"}]}`,
			"/icon":             `{"images":[{"id":"icon-1","sha256":"bbb"}]}`,
		})
		client := newTestClient(t, api)

		res, err := runImages(context.Background(), client, ImagesArgs{Locale: "en-US"})
		if err != nil {
			t.Fatalf("runImages: %v", err)
		}
		if len(res.Images) != 2 {
			t.Fatalf("got %+v", res.Images)
		}
		// The API has no listing that spans image types, so all eight are
		// asked for — but inside one edit, not eight.
		var inserts int
		for _, call := range api.calls() {
			if call == "POST /androidpublisher/v3/applications/com.example.app/edits" {
				inserts++
			}
		}
		if inserts != 1 {
			t.Errorf("opened %d edits, want 1", inserts)
		}
		assertEditDeleted(t, api)
	})

	t.Run("one type", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{
			"/phoneScreenshots": `{"images":[{"id":"img-1","sha256":"aaa"}]}`,
		})
		client := newTestClient(t, api)

		res, err := runImages(context.Background(), client, ImagesArgs{Locale: "en-US", Type: "phoneScreenshots"})
		if err != nil {
			t.Fatalf("runImages: %v", err)
		}
		if len(res.Images) != 1 || res.Images[0].Type != "phoneScreenshots" {
			t.Errorf("unexpected images: %+v", res.Images)
		}
		var gets int
		for _, call := range api.calls() {
			if strings.HasPrefix(call, "GET ") {
				gets++
			}
		}
		if gets != 1 {
			t.Errorf("made %d GETs for one image type: %v", gets, api.calls())
		}
	})

	t.Run("a typo lists the alternatives", func(t *testing.T) {
		api := playReadAPI(t, nil)
		client := newTestClient(t, api)

		_, err := runImages(context.Background(), client, ImagesArgs{Locale: "en-US", Type: "phoneScreenshot"})
		if err == nil {
			t.Fatal("expected an unknown image type to be rejected")
		}
		if !strings.Contains(err.Error(), "phoneScreenshots") {
			t.Errorf("error should list the valid types: %v", err)
		}
		if len(api.calls()) != 0 {
			t.Errorf("a rejected type must not reach the API: %v", api.calls())
		}
	})

	t.Run("locale is required", func(t *testing.T) {
		api := playReadAPI(t, nil)
		client := newTestClient(t, api)

		if _, err := runImages(context.Background(), client, ImagesArgs{}); err == nil {
			t.Fatal("expected a missing locale to be rejected")
		}
	})
}

func TestRunDetails(t *testing.T) {
	api := playReadAPI(t, map[string]string{
		"/details": `{"defaultLanguage":"en-US","contactEmail":"dev@example.com","contactWebsite":"https://example.com","contactPhone":"+31 6 1234 5678"}`,
	})
	client := newTestClient(t, api)

	res, err := runDetails(context.Background(), client, DetailsArgs{})
	if err != nil {
		t.Fatalf("runDetails: %v", err)
	}
	if res.Details.DefaultLanguage != "en-US" || res.Details.ContactEmail != "dev@example.com" {
		t.Errorf("unexpected details: %+v", res.Details)
	}
	assertEditDeleted(t, api)
}

func TestRunTesters(t *testing.T) {
	t.Run("lists the groups", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{
			"/testers/internal": `{"googleGroups":["qa@googlegroups.com","beta@googlegroups.com"]}`,
		})
		client := newTestClient(t, api)

		res, err := runTesters(context.Background(), client, TestersArgs{Track: "internal"})
		if err != nil {
			t.Fatalf("runTesters: %v", err)
		}
		if len(res.GoogleGroups) != 2 {
			t.Errorf("unexpected groups: %+v", res)
		}
		assertEditDeleted(t, api)
	})

	t.Run("track is required", func(t *testing.T) {
		api := playReadAPI(t, nil)
		client := newTestClient(t, api)
		if _, err := runTesters(context.Background(), client, TestersArgs{}); err == nil {
			t.Fatal("expected a missing track to be rejected")
		}
	})
}

func TestRunCountries(t *testing.T) {
	api := playReadAPI(t, map[string]string{
		"/countryAvailability/production": `{"syncWithProduction":false,"restOfWorld":true,"countries":[{"countryCode":"NL"},{"countryCode":"DE"}]}`,
	})
	client := newTestClient(t, api)

	res, err := runCountries(context.Background(), client, CountriesArgs{Track: "production"})
	if err != nil {
		t.Fatalf("runCountries: %v", err)
	}
	if len(res.Countries) != 2 || res.Countries[0] != "NL" {
		t.Errorf("unexpected countries: %+v", res)
	}
	// syncWithProduction changes what an empty list means, so it has to be
	// reported rather than folded away.
	if res.SyncWithProduction || !res.RestOfWorld {
		t.Errorf("availability flags were lost: %+v", res)
	}
	assertEditDeleted(t, api)
}

func TestRunDeviceTiers(t *testing.T) {
	api := playReadAPI(t, map[string]string{
		"/deviceTierConfigs": `{"deviceTierConfigs":[{"deviceTierConfigId":"1234","deviceGroups":[{"name":"high"}]}]}`,
	})
	client := newTestClient(t, api)

	res, err := runDeviceTiers(context.Background(), client, DeviceTiersArgs{})
	if err != nil {
		t.Fatalf("runDeviceTiers: %v", err)
	}
	if len(res.Configs) != 1 || res.Configs[0].ID != "1234" {
		t.Fatalf("unexpected configs: %+v", res.Configs)
	}
	// The tier grammar is carried raw rather than reshaped, so nothing is lost.
	if !strings.Contains(string(res.Configs[0].Raw), "deviceGroups") {
		t.Errorf("raw config was dropped: %s", res.Configs[0].Raw)
	}
	// No edit: device tier configs hang off the application, not off a
	// publishing transaction.
	for _, call := range api.calls() {
		if strings.Contains(call, "/edits") {
			t.Errorf("device tiers opened an edit: %v", api.calls())
		}
	}
}

func TestRunEditStatus(t *testing.T) {
	t.Run("a credential that can write", func(t *testing.T) {
		api := playReadAPI(t, nil)
		client := newTestClient(t, api)

		res, err := runEditStatus(context.Background(), client, EditStatusArgs{})
		if err != nil {
			t.Fatalf("runEditStatus: %v", err)
		}
		if !res.CanEdit || res.EditID != "edit-1" {
			t.Errorf("unexpected result: %+v", res)
		}
		// The probe must not leave its edit behind.
		assertEditDeleted(t, api)
	})

	t.Run("a rejection is an answer, not an error", func(t *testing.T) {
		// A precondition check that errors out makes an agent treat a clear
		// "no" as a broken tool.
		api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusForbidden, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)
		})
		client := newTestClient(t, api)

		res, err := runEditStatus(context.Background(), client, EditStatusArgs{})
		if err != nil {
			t.Fatalf("a definitive rejection should be reported, not returned as an error: %v", err)
		}
		if res.CanEdit {
			t.Fatal("expected can_edit=false")
		}
		if !strings.Contains(res.Reason, "Users & permissions") {
			t.Errorf("reason should say what to fix: %q", res.Reason)
		}
	})

	t.Run("an unreachable API is still an error", func(t *testing.T) {
		// A 5xx says nothing about permissions, so answering "can_edit: false"
		// would be a lie.
		shrinkBackoff(t)
		api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusInternalServerError, `{"error":{"code":500,"message":"Internal","status":"INTERNAL"}}`)
		})
		client := newTestClient(t, api)

		if _, err := runEditStatus(context.Background(), client, EditStatusArgs{}); err == nil {
			t.Fatal("expected a transient failure to remain an error")
		}
	})
}

func TestRunApps(t *testing.T) {
	t.Run("marks and hoists the configured app", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{
			"/apps:search": `{"apps":[
				{"packageName":"com.other.app","displayName":"Other"},
				{"packageName":"com.example.app","displayName":"Example"}
			]}`,
		})
		client := newTestClient(t, api)

		res, err := runApps(context.Background(), client, AppsArgs{})
		if err != nil {
			t.Fatalf("runApps: %v", err)
		}
		if len(res.Apps) != 2 {
			t.Fatalf("got %+v", res.Apps)
		}
		// The default goes first: it is the one an agent will act on, and
		// burying it in an alphabetical list invites the wrong choice.
		if res.Apps[0].PackageName != "com.example.app" || !res.Apps[0].Default {
			t.Errorf("the configured app was not hoisted: %+v", res.Apps)
		}
		if res.Apps[1].Default {
			t.Errorf("only the configured app is the default: %+v", res.Apps)
		}
	})

	t.Run("a credential that cannot list still reports the configured app", func(t *testing.T) {
		// A service account with release permissions but no "View app
		// information" publishes fine and cannot list; failing the first call
		// an agent makes would be worse than saying so.
		api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusForbidden, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)
		})
		client := newTestClient(t, api)

		res, err := runApps(context.Background(), client, AppsArgs{})
		if err != nil {
			t.Fatalf("runApps: %v", err)
		}
		if len(res.Apps) != 1 || res.Apps[0].PackageName != "com.example.app" || !res.Apps[0].Default {
			t.Fatalf("unexpected apps: %+v", res.Apps)
		}
		if !strings.Contains(res.Message, "View app information") {
			t.Errorf("message should name the missing permission: %q", res.Message)
		}
	})

	t.Run("no configured app and no listing is a real error", func(t *testing.T) {
		api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusForbidden, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)
		})
		clearPlayEnv(t)
		cfg := &PlayConfig{}
		cfg.BaseURL, cfg.ReportingBaseURL = api.URL, api.URL
		client, err := NewClient(context.Background(), cfg)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if _, err := runApps(context.Background(), client, AppsArgs{}); err == nil {
			t.Fatal("with nothing to report, this has to fail")
		}
	})

	t.Run("a configured app the Reporting API cannot see is flagged", func(t *testing.T) {
		api := playReadAPI(t, map[string]string{
			"/apps:search": `{"apps":[{"packageName":"com.other.app","displayName":"Other"}]}`,
		})
		client := newTestClient(t, api)

		res, err := runApps(context.Background(), client, AppsArgs{})
		if err != nil {
			t.Fatalf("runApps: %v", err)
		}
		if res.Apps[0].PackageName != "com.example.app" {
			t.Errorf("the configured app should still be listed: %+v", res.Apps)
		}
		if res.Message == "" {
			t.Error("a configured app missing from the listing deserves a note")
		}
	})
}

// TestReadResultsRenderAsTables: --format table is only useful if every read
// result can produce rows.
func TestReadResultsRenderAsTables(t *testing.T) {
	results := []any{
		AppsResult{Apps: []AppInfo{{PackageName: "com.example.app", Default: true}}},
		TracksResult{Tracks: []TrackInfo{{Track: "production", Releases: []ReleaseInfo{{Track: "production", Status: "completed"}}}}},
		ReleasesResult{Releases: []ReleaseInfo{{Track: "beta", Status: "draft"}}},
		ArtifactsResult{Artifacts: []ArtifactInfo{{VersionCode: 42, Type: "bundle"}}},
		ListingResult{Listings: []Listing{{Language: "en-US", Title: "Example"}}},
		ImagesResult{Images: []ImageInfo{{Type: "icon", ID: "img-1"}}},
		DetailsResult{Details: AppDetails{DefaultLanguage: "en-US"}},
		TestersResult{Track: "internal", GoogleGroups: []string{"qa@googlegroups.com"}},
		CountriesResult{Track: "production", Countries: []string{"NL"}},
		DeviceTiersResult{Configs: []DeviceTierConfig{{ID: "1234"}}},
		EditStatusResult{PackageName: "com.example.app", CanEdit: true},
	}
	for _, res := range results {
		for _, format := range []string{"json", "table", "csv"} {
			var buf strings.Builder
			if err := printResult(&buf, format, res); err != nil {
				t.Errorf("%T as %s: %v", res, format, err)
			}
			if buf.Len() == 0 {
				t.Errorf("%T rendered nothing as %s", res, format)
			}
		}
	}
}

// TestReadArgsCarryToolDescriptions: the jsonschema tags are the only
// documentation an agent sees for a tool's inputs.
func TestReadArgsCarryToolDescriptions(t *testing.T) {
	for _, args := range []any{
		TracksArgs{}, ReleasesArgs{}, ArtifactsArgs{}, ListingArgs{},
		ImagesArgs{}, DetailsArgs{}, TestersArgs{}, CountriesArgs{},
		DeviceTiersArgs{}, EditStatusArgs{},
	} {
		assertDescribedFields(t, args)
	}
}

func assertDescribedFields(t *testing.T, args any) {
	t.Helper()
	rt := reflect.TypeOf(args)
	for i := range rt.NumField() {
		field := rt.Field(i)
		if field.Tag.Get("jsonschema") == "" {
			t.Errorf("%s.%s has no jsonschema description — it is the only documentation an agent gets", rt.Name(), field.Name)
		}
	}
}
