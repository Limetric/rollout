package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// stagedTrackWrite stages a release write the way a phase-2 tool will.
func stagedTrackWrite(t *testing.T, payload trackPayload, tool string) *PendingMutation {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &PendingMutation{
		Token: "0123456789abcdef", Platform: playPlatformName, Tool: tool,
		PackageName: "com.example.app", Summary: "set " + payload.Track,
		Dispatch: dispatchTrack, Payload: raw, Track: payload.Track,
	}
}

func release(versionCode, status string, extra string) json.RawMessage {
	body := `{"versionCodes":["` + versionCode + `"],"status":"` + status + `"`
	if extra != "" {
		body += "," + extra
	}
	return json.RawMessage(body + "}")
}

// TestTrackWriteNeverClobbersOtherReleases is the rule this dispatch exists
// for. A track holds several releases at once, and the API's tracks.update
// replaces the whole array — writing an array assembled from a tool's own
// arguments is how an in-progress rollout silently disappears.
func TestTrackWriteNeverClobbersOtherReleases(t *testing.T) {
	existing := `{"track":"production","releases":[
		{"versionCodes":["41"],"status":"completed"},
		{"versionCodes":["42"],"status":"inProgress","userFraction":0.1}
	]}`

	var written track
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tracks/production"):
			writeJSON(w, http.StatusOK, existing)
		case r.Method == http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&written)
			writeJSON(w, http.StatusOK, `{"track":"production"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
	client := newTestClient(t, api)

	p := stagedTrackWrite(t, trackPayload{
		Track:   "production",
		Release: release("42", "inProgress", `"userFraction":0.5`),
	}, "update_release")

	outcome, err := client.applyMutation(context.Background(), p)
	if err != nil {
		t.Fatalf("applyMutation: %v", err)
	}
	if outcome.EditID != "edit-1" {
		t.Errorf("edit id = %q", outcome.EditID)
	}

	if len(written.Releases) != 2 {
		t.Fatalf("wrote %d releases, want both kept: %s", len(written.Releases), mustJSON(t, written))
	}
	// The completed release still serving most users must survive untouched.
	if !strings.Contains(string(written.Releases[0]), `"41"`) || !strings.Contains(string(written.Releases[0]), "completed") {
		t.Errorf("the completed release was altered: %s", written.Releases[0])
	}
	// The one release named by the write is the one that changed.
	if !strings.Contains(string(written.Releases[1]), "0.5") {
		t.Errorf("the staged release did not land: %s", written.Releases[1])
	}
}

// TestTrackWriteAppendsANewRelease: a release the track has never seen is added
// rather than replacing whatever happened to be there.
func TestTrackWriteAppendsANewRelease(t *testing.T) {
	var written track
	api := newFakePlayAPI(t, trackAPI(t, `{"track":"beta","releases":[{"versionCodes":["41"],"status":"completed"}]}`, &written))
	client := newTestClient(t, api)

	p := stagedTrackWrite(t, trackPayload{Track: "beta", Release: release("42", "draft", "")}, "create_release")
	if _, err := client.applyMutation(context.Background(), p); err != nil {
		t.Fatalf("applyMutation: %v", err)
	}
	if len(written.Releases) != 2 {
		t.Fatalf("wrote %d releases, want the new one appended: %s", len(written.Releases), mustJSON(t, written))
	}
}

// TestTrackWriteKeepsUnparseableReleases: an existing release we cannot read is
// not ours to discard — dropping it would remove something live from the track.
func TestTrackWriteKeepsUnparseableReleases(t *testing.T) {
	var written track
	api := newFakePlayAPI(t, trackAPI(t, `{"track":"production","releases":[
		{"somethingNew":true},
		{"versionCodes":["42"],"status":"inProgress"}
	]}`, &written))
	client := newTestClient(t, api)

	p := stagedTrackWrite(t, trackPayload{Track: "production", Release: release("42", "completed", "")}, "complete_release")
	if _, err := client.applyMutation(context.Background(), p); err != nil {
		t.Fatalf("applyMutation: %v", err)
	}
	if len(written.Releases) != 2 {
		t.Fatalf("wrote %d releases: %s", len(written.Releases), mustJSON(t, written))
	}
	if !strings.Contains(string(written.Releases[0]), "somethingNew") {
		t.Errorf("an unrecognized release was dropped: %s", mustJSON(t, written))
	}
}

func TestTrackWriteRemovesSupersededDraftsOnly(t *testing.T) {
	var written track
	api := newFakePlayAPI(t, trackAPI(t, `{"track":"beta","releases":[
		{"versionCodes":["40"],"status":"draft"},
		{"versionCodes":["41"],"status":"inProgress"}
	]}`, &written))
	client := newTestClient(t, api)

	p := stagedTrackWrite(t, trackPayload{
		Track: "beta", Release: release("42", "draft", ""), RemoveOtherDrafts: true,
	}, "create_release")
	if _, err := client.applyMutation(context.Background(), p); err != nil {
		t.Fatalf("applyMutation: %v", err)
	}
	if len(written.Releases) != 2 {
		t.Fatalf("wrote %d releases: %s", len(written.Releases), mustJSON(t, written))
	}
	// The in-progress rollout survives; only the superseded draft goes.
	body := mustJSON(t, written)
	if strings.Contains(body, `"40"`) {
		t.Errorf("the superseded draft should have been removed: %s", body)
	}
	if !strings.Contains(body, `"41"`) {
		t.Errorf("an in-progress release must never be removed: %s", body)
	}
}

func TestTrackWriteMatchesMultiAPKReleasesRegardlessOfOrder(t *testing.T) {
	var written track
	api := newFakePlayAPI(t, trackAPI(t, `{"track":"production","releases":[
		{"versionCodes":["42","43"],"status":"inProgress"}
	]}`, &written))
	client := newTestClient(t, api)

	p := stagedTrackWrite(t, trackPayload{
		Track:   "production",
		Release: json.RawMessage(`{"versionCodes":["43","42"],"status":"completed"}`),
	}, "complete_release")
	if _, err := client.applyMutation(context.Background(), p); err != nil {
		t.Fatalf("applyMutation: %v", err)
	}
	if len(written.Releases) != 1 {
		t.Fatalf("a multi-APK release was not matched: %s", mustJSON(t, written))
	}
	if !strings.Contains(string(written.Releases[0]), "completed") {
		t.Errorf("the release was not replaced: %s", written.Releases[0])
	}
}

// TestEditWriteIsAllOrNothing: a multi-locale listing sync that fails part-way
// must commit nothing and name the locale that failed.
func TestEditWriteIsAllOrNothing(t *testing.T) {
	var committed bool
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":commit"):
			committed = true
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.Contains(r.URL.Path, "/listings/de-DE"):
			writeJSON(w, http.StatusBadRequest, `{"error":{"code":400,"message":"Full description exceeds 4000 characters","status":"INVALID_ARGUMENT"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
	client := newTestClient(t, api)

	payload := editPayload{Requests: []editRequest{
		{Method: http.MethodPut, Path: "listings/en-US", Body: json.RawMessage(`{"title":"Example"}`), Describe: "locale en-US"},
		{Method: http.MethodPut, Path: "listings/de-DE", Body: json.RawMessage(`{"title":"Beispiel"}`), Describe: "locale de-DE"},
	}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := &PendingMutation{
		Token: "0123456789abcdef", Platform: playPlatformName, Tool: "sync_listing",
		PackageName: "com.example.app", Summary: "sync 2 locales",
		Dispatch: dispatchEdit, Payload: raw,
	}

	_, err = client.applyMutation(context.Background(), p)
	if err == nil {
		t.Fatal("expected the failing locale to fail the whole sync")
	}
	// "The sync failed" is not enough to act on; the locale is.
	if !strings.Contains(err.Error(), "de-DE") {
		t.Errorf("error should name the failing locale: %v", err)
	}
	if committed {
		t.Error("a partially applied sync must not commit")
	}
	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("the aborted edit was not deleted: %v", api.calls())
	}
}

// TestDirectWriteSkipsTheEdit: reviews live outside the publishing
// transaction, so wrapping a reply in an edit would open and commit an empty
// edit for nothing.
func TestDirectWriteSkipsTheEdit(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"result":{"replyText":"Thanks!"}}`)
	})
	client := newTestClient(t, api)

	payload := editPayload{Requests: []editRequest{{
		Method: http.MethodPost,
		Path:   "applications/com.example.app/reviews/review-1:reply",
		Body:   json.RawMessage(`{"replyText":"Thanks!"}`),
	}}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := &PendingMutation{
		Token: "0123456789abcdef", Platform: playPlatformName, Tool: "reply_review",
		PackageName: "com.example.app", Summary: "reply to review-1",
		Dispatch: dispatchDirect, Payload: raw,
	}

	outcome, err := client.applyMutation(context.Background(), p)
	if err != nil {
		t.Fatalf("applyMutation: %v", err)
	}
	if outcome.EditID != "" {
		t.Errorf("a direct write must not open an edit, got %q", outcome.EditID)
	}
	for _, call := range api.calls() {
		if strings.HasSuffix(call, "/edits") {
			t.Errorf("a direct write opened an edit: %v", api.calls())
		}
	}
	if len(outcome.Results) != 1 {
		t.Errorf("the reply result was dropped: %+v", outcome)
	}
}

func TestApplyMutationRejectsAnUnknownDispatch(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, `{}`) })
	client := newTestClient(t, api)

	p := &PendingMutation{
		Token: "0123456789abcdef", Platform: playPlatformName, Tool: "future_tool",
		PackageName: "com.example.app", Dispatch: "something_new",
		Payload: json.RawMessage(`{}`),
	}
	_, err := client.applyMutation(context.Background(), p)
	if err == nil {
		t.Fatal("expected an unknown dispatch to fail")
	}
	// A token written by a newer binary is the realistic cause, and re-running
	// the command is the fix.
	if !strings.Contains(err.Error(), "re-run") {
		t.Errorf("error should say what to do: %v", err)
	}
	if len(api.calls()) != 0 {
		t.Errorf("nothing should have been called: %v", api.calls())
	}
}

func TestApplyMutationRejectsACorruptPayload(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, `{}`) })
	client := newTestClient(t, api)

	for _, dispatch := range []string{dispatchEdit, dispatchTrack, dispatchDirect} {
		p := &PendingMutation{
			Token: "0123456789abcdef", Platform: playPlatformName, Tool: "update_release",
			PackageName: "com.example.app", Dispatch: dispatch,
			Payload: json.RawMessage(`not json`),
		}
		if _, err := client.applyMutation(context.Background(), p); err == nil {
			t.Errorf("dispatch %q accepted a corrupt payload", dispatch)
		}
	}
}

// TestPreviewPlayWriteStampsThePlatform: the staged record is what routes
// `rollout confirm <token>` back to the right API.
func TestPreviewPlayWriteStampsThePlatform(t *testing.T) {
	isolateState(t)

	res, err := previewPlayWrite(stagePlayWriteRequest{
		Tool: "update_release", PackageName: "com.example.app",
		Summary:  "set production to 42 at 10%",
		Dispatch: dispatchTrack, Track: productionTrack,
		RolloutFraction: fraction(0.1),
		Payload:         trackPayload{Track: productionTrack, Release: release("42", "inProgress", "")},
	})
	if err != nil {
		t.Fatalf("previewPlayWrite: %v", err)
	}
	if res.Applied {
		t.Fatal("a preview must never apply")
	}
	if !strings.Contains(res.Preview, "rollout confirm") {
		t.Errorf("preview should say how to apply it:\n%s", res.Preview)
	}

	staged, err := peekMutation(res.ConfirmToken)
	if err != nil {
		t.Fatalf("peekMutation: %v", err)
	}
	if staged.Platform != playPlatformName || staged.Dispatch != dispatchTrack {
		t.Errorf("staged record = %+v", staged)
	}
	// The guard-relevant facts have to survive staging, or the confirm-time
	// re-check has nothing to evaluate.
	if staged.Track != productionTrack || staged.RolloutFraction == nil || *staged.RolloutFraction != 0.1 {
		t.Errorf("staged record lost its guard facts: %+v", staged)
	}
	// A staged write must never carry an edit ID: edits expire, and this token
	// may be confirmed from another process minutes later.
	if strings.Contains(string(staged.Payload), "edits/") {
		t.Errorf("staged payload references an edit: %s", staged.Payload)
	}
}

// trackAPI answers the calls a track write makes, capturing the PUT body.
func trackAPI(t *testing.T, current string, written *track) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tracks/"):
			writeJSON(w, http.StatusOK, current)
		case r.Method == http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(written); err != nil {
				t.Errorf("decode track PUT: %v", err)
			}
			writeJSON(w, http.StatusOK, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
