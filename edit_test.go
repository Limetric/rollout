package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// editHandler answers the four calls an edit lifecycle makes, so a test only
// has to say which of them should fail.
type editHandler struct {
	insertStatus   int
	validateStatus int
	commitStatus   int
	deleteStatus   int
}

func (h editHandler) handler() http.HandlerFunc {
	status := func(v int) int {
		if v == 0 {
			return http.StatusOK
		}
		return v
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(status(h.deleteStatus))
		case strings.HasSuffix(r.URL.Path, ":validate"):
			writeJSON(w, status(h.validateStatus), `{"error":{"code":400,"message":"Release notes exceed 500 characters","status":"INVALID_ARGUMENT"}}`)
		case strings.HasSuffix(r.URL.Path, ":commit"):
			writeJSON(w, status(h.commitStatus), `{"id":"edit-1"}`)
		case r.Method == http.MethodPost:
			writeJSON(w, status(h.insertStatus), `{"id":"edit-1","expiryTimeSeconds":"1700000000"}`)
		default:
			writeJSON(w, http.StatusOK, `{"id":"edit-1","expiryTimeSeconds":"1700000000"}`)
		}
	}
}

func TestWithEditReadModeAlwaysDeletes(t *testing.T) {
	api := newFakePlayAPI(t, editHandler{}.handler())
	client := newTestClient(t, api)

	var ran bool
	editID, err := client.withEdit(context.Background(), "com.example.app", false, commitOptions{}, func(e *editSession) error {
		ran = true
		if e.id != "edit-1" {
			t.Errorf("session id = %q", e.id)
		}
		return nil
	})
	if err != nil || !ran || editID != "edit-1" {
		t.Fatalf("withEdit: %v (ran=%v id=%q)", err, ran, editID)
	}

	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("a read must delete its edit; calls were %v", api.calls())
	}
	// A read must never commit: committing an untouched edit still publishes a
	// change in the Console's activity log.
	for _, call := range api.calls() {
		if strings.HasSuffix(call, ":commit") {
			t.Errorf("a read committed its edit: %v", api.calls())
		}
	}
}

// TestWithEditReadModeDeletesAfterAFailure: the body failing is exactly when a
// stale edit would be left behind.
func TestWithEditReadModeDeletesAfterAFailure(t *testing.T) {
	api := newFakePlayAPI(t, editHandler{}.handler())
	client := newTestClient(t, api)

	want := errors.New("decode track")
	_, err := client.withEdit(context.Background(), "com.example.app", false, commitOptions{}, func(*editSession) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("withEdit returned %v, want the body's error", err)
	}
	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("the edit was not deleted; calls were %v", api.calls())
	}
}

func TestWithEditWriteModeValidatesThenCommits(t *testing.T) {
	api := newFakePlayAPI(t, editHandler{}.handler())
	client := newTestClient(t, api)

	if _, err := client.withEdit(context.Background(), "com.example.app", true, commitOptions{ChangesNotSentForReview: true}, func(*editSession) error {
		return nil
	}); err != nil {
		t.Fatalf("withEdit: %v", err)
	}

	calls := api.calls()
	var validateAt, commitAt = -1, -1
	for i, c := range calls {
		if strings.HasSuffix(c, ":validate") {
			validateAt = i
		}
		if strings.HasSuffix(c, ":commit") {
			commitAt = i
		}
	}
	if validateAt < 0 || commitAt < 0 {
		t.Fatalf("expected a validate and a commit, got %v", calls)
	}
	// The dry run is only useful before the real thing.
	if validateAt > commitAt {
		t.Errorf("validate must precede commit, got %v", calls)
	}
	// A committed edit is gone; deleting it afterwards would be a 4xx.
	if api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("a committed edit must not be deleted: %v", calls)
	}
	// The review flag has to reach the wire, or the write silently queues for
	// review instead of publishing.
	for _, r := range api.seen() {
		if strings.HasSuffix(r.Path, ":commit") && !strings.Contains(r.Query, "changesNotSentForReview=true") {
			t.Errorf("commit query = %q", r.Query)
		}
	}
}

// TestWithEditWriteModeDeletesOnValidateFailure is the reason validate exists:
// a rejected write must leave nothing staged, and must not commit.
func TestWithEditWriteModeDeletesOnValidateFailure(t *testing.T) {
	api := newFakePlayAPI(t, editHandler{validateStatus: http.StatusBadRequest}.handler())
	client := newTestClient(t, api)

	_, err := client.withEdit(context.Background(), "com.example.app", true, commitOptions{}, func(*editSession) error { return nil })
	if err == nil {
		t.Fatal("expected the validate failure to fail the write")
	}
	// The API's own message is the useful part; it names what is wrong.
	if !strings.Contains(err.Error(), "Release notes exceed 500 characters") {
		t.Errorf("error lost the API's reason: %v", err)
	}
	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("a failed validate must delete the edit; calls were %v", api.calls())
	}
	for _, call := range api.calls() {
		if strings.HasSuffix(call, ":commit") {
			t.Fatalf("a failed validate must not commit: %v", api.calls())
		}
	}
}

func TestWithEditWriteModeDeletesOnCommitFailure(t *testing.T) {
	api := newFakePlayAPI(t, editHandler{commitStatus: http.StatusConflict}.handler())
	client := newTestClient(t, api)

	_, err := client.withEdit(context.Background(), "com.example.app", true, commitOptions{}, func(*editSession) error { return nil })
	if err == nil {
		t.Fatal("expected the commit failure to fail the write")
	}
	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("a failed commit must clean up; calls were %v", api.calls())
	}
}

// TestWithEditCleansUpAfterACancelledContext: the common reason a write fails
// is cancellation, and cleaning up with the same dead context would skip the
// cleanup exactly when it matters.
func TestWithEditCleansUpAfterACancelledContext(t *testing.T) {
	api := newFakePlayAPI(t, editHandler{}.handler())
	client := newTestClient(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	_, err := client.withEdit(ctx, "com.example.app", true, commitOptions{}, func(*editSession) error {
		cancel()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("expected the cancelled body to fail the write")
	}
	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("the edit was not cleaned up after cancellation; calls were %v", api.calls())
	}
}

// TestFailedDeleteDoesNotMaskTheRealError: an abandoned edit expires on its
// own, so a cleanup failure is a warning, never the error the user sees.
func TestFailedDeleteDoesNotMaskTheRealError(t *testing.T) {
	captureWarnings(t)
	api := newFakePlayAPI(t, editHandler{
		validateStatus: http.StatusBadRequest,
		deleteStatus:   http.StatusInternalServerError,
	}.handler())
	client := newTestClient(t, api)

	_, err := client.withEdit(context.Background(), "com.example.app", true, commitOptions{}, func(*editSession) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Release notes exceed 500 characters") {
		t.Fatalf("err = %v, want the validate failure", err)
	}
}

func TestOpenEditRejectsAResponseWithoutAnID(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	})
	client := newTestClient(t, api)

	if _, err := client.openEdit(context.Background(), "com.example.app"); err == nil {
		t.Fatal("expected an error when the API returns no edit id")
	}
}

func TestEditGetReadsExpiry(t *testing.T) {
	api := newFakePlayAPI(t, editHandler{}.handler())
	client := newTestClient(t, api)

	e := &editSession{c: client, pkg: "com.example.app", id: "edit-1"}
	edit, err := e.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if edit.ExpiryTimeSeconds != "1700000000" {
		t.Errorf("expiry = %q", edit.ExpiryTimeSeconds)
	}
}

func TestEditPathBuildsResourcePaths(t *testing.T) {
	e := &editSession{pkg: "com.example.app", id: "edit-1"}
	if got := e.path("tracks/production"); got != "applications/com.example.app/edits/edit-1/tracks/production" {
		t.Errorf("path = %q", got)
	}
	if got := e.path(""); got != "applications/com.example.app/edits/edit-1" {
		t.Errorf("path = %q", got)
	}
}

// TestCommitWithoutTheReviewFlagSendsNoQuery: the flag is a per-call decision —
// Google rejects it for apps where review is mandatory — so it must not leak in
// by default.
func TestCommitWithoutTheReviewFlagSendsNoQuery(t *testing.T) {
	api := newFakePlayAPI(t, editHandler{}.handler())
	client := newTestClient(t, api)

	e := &editSession{c: client, pkg: "com.example.app", id: "edit-1"}
	if _, err := e.commit(context.Background(), commitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	for _, r := range api.seen() {
		if strings.HasSuffix(r.Path, ":commit") && r.Query != "" {
			t.Errorf("commit sent query %q, want none", r.Query)
		}
	}
}
