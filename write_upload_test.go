package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestUploadAndReleaseShareOneEdit is the property the upload dispatch exists
// for: an artifact uploaded into an edit that is never committed does not
// exist, so the upload and the release that carries it cannot be split.
func TestUploadAndReleaseShareOneEdit(t *testing.T) {
	isolateState(t)

	var written track
	var uploadedTo string
	var inserts, commits int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			inserts++
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.HasSuffix(r.URL.Path, ":commit"):
			commits++
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.HasSuffix(r.URL.Path, ":validate"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.Contains(r.URL.Path, "/upload/"):
			// Resumable initiate.
			uploadedTo = r.URL.Path
			w.Header().Set("Location", "http://"+r.Host+"/resumable-session")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/resumable-session":
			writeJSON(w, http.StatusOK, `{"versionCode":44,"sha256":"abc"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tracks/"):
			writeJSON(w, http.StatusOK, `{"track":"internal","releases":[{"versionCodes":["43"],"status":"draft"}]}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/tracks/"):
			_ = json.NewDecoder(r.Body).Decode(&written)
			writeJSON(w, http.StatusOK, `{}`)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	path := writeTempFile(t, "app.aab", 2048)
	preview, err := runUploadArtifact(ctx, client, UploadArtifactArgs{File: path, Track: "internal"})
	if err != nil {
		t.Fatalf("runUploadArtifact: %v", err)
	}
	res, err := applyConfirmed(ctx, client, "upload_artifact", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !res.Applied {
		t.Fatal("expected the confirm to apply")
	}

	if inserts != 1 || commits != 1 {
		t.Errorf("opened %d edits and committed %d times, want 1 of each", inserts, commits)
	}
	if !strings.Contains(uploadedTo, "/edits/edit-1/bundles") {
		t.Errorf("the artifact went to %q, want the bundles endpoint inside the edit", uploadedTo)
	}
	// The version code is only known after the upload, so the release is
	// completed with it inside the same edit.
	if len(written.Releases) != 2 {
		t.Fatalf("wrote %d releases: %s", len(written.Releases), mustJSON(t, written))
	}
	if !strings.Contains(mustJSON(t, written), `"44"`) {
		t.Errorf("the uploaded version code did not reach the release: %s", mustJSON(t, written))
	}
	// The existing draft is left alone unless asked.
	if !strings.Contains(mustJSON(t, written), `"43"`) {
		t.Errorf("an existing release was dropped: %s", mustJSON(t, written))
	}
	if !strings.Contains(res.Detail, "44") {
		t.Errorf("the result should report the version code: %q", res.Detail)
	}
}

// TestUploadOnlyCommitsWithoutTouchingATrack exercises --no-release.
func TestUploadOnlyCommitsWithoutTouchingATrack(t *testing.T) {
	isolateState(t)

	var trackWrites int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.Contains(r.URL.Path, "/upload/"):
			w.Header().Set("Location", "http://"+r.Host+"/resumable-session")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/resumable-session":
			writeJSON(w, http.StatusOK, `{"versionCode":44}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/tracks/"):
			trackWrites++
			writeJSON(w, http.StatusOK, `{}`)
		default:
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		}
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUploadArtifact(ctx, client, UploadArtifactArgs{
		File: writeTempFile(t, "app.aab", 1024), NoRelease: true,
	})
	if err != nil {
		t.Fatalf("runUploadArtifact: %v", err)
	}
	if _, err := applyConfirmed(ctx, client, "upload_artifact", preview.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if trackWrites != 0 {
		t.Errorf("--no-release wrote to a track %d times", trackWrites)
	}
}

// TestFailedUploadAbandonsTheEdit: a failed upload must leave nothing staged,
// and must not commit a bare artifact.
func TestFailedUploadAbandonsTheEdit(t *testing.T) {
	isolateState(t)

	var commits int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.HasSuffix(r.URL.Path, ":commit"):
			commits++
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.Contains(r.URL.Path, "/upload/"):
			writeJSON(w, http.StatusForbidden, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUploadArtifact(ctx, client, UploadArtifactArgs{
		File: writeTempFile(t, "app.aab", 1024), NoRelease: true,
	})
	if err != nil {
		t.Fatalf("runUploadArtifact: %v", err)
	}
	if _, err := applyConfirmed(ctx, client, "upload_artifact", preview.ConfirmToken); err == nil {
		t.Fatal("expected the rejected upload to fail the write")
	}
	if commits != 0 {
		t.Error("a failed upload must not commit its edit")
	}
	if !api.sawCall(http.MethodDelete, "/androidpublisher/v3/applications/com.example.app/edits/edit-1") {
		t.Errorf("the edit was not abandoned: %v", api.calls())
	}
}

// TestDeobfuscationUploadTargetsTheVersionCode checks the path the file goes to.
func TestDeobfuscationUploadTargetsTheVersionCode(t *testing.T) {
	isolateState(t)

	var uploadedTo string
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/edits"):
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		case strings.Contains(r.URL.Path, "/upload/"):
			uploadedTo = r.URL.Path
			w.Header().Set("Location", "http://"+r.Host+"/resumable-session")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/resumable-session":
			writeJSON(w, http.StatusOK, `{"deobfuscationFile":{"symbolType":"proguard"}}`)
		default:
			writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
		}
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUploadDeobfuscation(ctx, client, UploadDeobfuscationArgs{
		File: writeTempFile(t, "mapping.txt", 512), VersionCode: "42",
	})
	if err != nil {
		t.Fatalf("runUploadDeobfuscation: %v", err)
	}
	if _, err := applyConfirmed(ctx, client, "upload_deobfuscation", preview.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !strings.Contains(uploadedTo, "/apks/42/deobfuscationFiles/proguard") {
		t.Errorf("uploaded to %q", uploadedTo)
	}
}
