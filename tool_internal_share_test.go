package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

// internalShareAPI fakes the resumable upload the internal-sharing endpoint
// takes, and answers with whatever artifact the test supplies.
func internalShareAPI(t *testing.T, artifact string, initiated *string) *fakePlayAPI {
	t.Helper()
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/upload/"):
			*initiated = r.URL.Path
			w.Header().Set("Location", "http://"+r.Host+"/resumable-session")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/resumable-session":
			writeJSON(w, http.StatusOK, artifact)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
}

// TestInternalShareUploadsWithoutAnEdit is the property this dispatch exists
// for: an internal-sharing artifact is not published, so opening an edit around
// it would commit an empty transaction against the app.
func TestInternalShareUploadsWithoutAnEdit(t *testing.T) {
	var initiated string
	api := internalShareAPI(t, `{"downloadUrl":"https://play.google.com/apps/test/abc","sha256":"abc","certificateFingerprint":"AA:BB"}`, &initiated)
	client := newTestClient(t, api)
	ctx := context.Background()

	path := writeTempFile(t, "app.aab", 2048)
	preview, err := runInternalShare(ctx, client, InternalShareArgs{File: path})
	if err != nil {
		t.Fatalf("runInternalShare: %v", err)
	}
	if preview.Applied || preview.ConfirmToken == "" {
		t.Fatalf("first call must not upload: %+v", preview)
	}
	// The link cannot be withdrawn through this API, and the preview is where
	// that has to be said.
	if !strings.Contains(preview.Preview, "cannot withdraw") {
		t.Errorf("preview should say the link is not retractable:\n%s", preview.Preview)
	}

	res := applyPreview(t, ctx, client, "internal_share", preview.ConfirmToken)
	if !strings.HasSuffix(initiated, "/applications/internalappsharing/com.example.app/artifacts/bundle") {
		t.Errorf("uploaded to %q", initiated)
	}
	for _, call := range api.calls() {
		if strings.Contains(call, "/edits") {
			t.Errorf("internal sharing opened an edit: %v", api.calls())
			break
		}
	}
	// The link is the entire product of the call — burying it would make the
	// tool useless.
	if !strings.Contains(res.Detail, "https://play.google.com/apps/test/abc") {
		t.Errorf("detail does not carry the install link: %q", res.Detail)
	}
	if res.EditID != "" {
		t.Errorf("no edit carried this write, but one was reported: %q", res.EditID)
	}
}

// TestInternalShareFailsWithoutADownloadURL: the upload without its link is an
// artifact nobody can install, and reporting success would hide that.
func TestInternalShareFailsWithoutADownloadURL(t *testing.T) {
	var initiated string
	api := internalShareAPI(t, `{"sha256":"abc"}`, &initiated)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runInternalShare(ctx, client, InternalShareArgs{File: writeTempFile(t, "app.apk", 1024)})
	if err != nil {
		t.Fatalf("runInternalShare: %v", err)
	}
	if _, err := applyConfirmed(ctx, client, "internal_share", preview.ConfirmToken); err == nil {
		t.Fatal("expected a missing download URL to fail the write")
	} else if !strings.Contains(err.Error(), "download URL") {
		t.Errorf("error should name what is missing: %v", err)
	}
	if !strings.HasSuffix(initiated, "/artifacts/apk") {
		t.Errorf("an .apk went to %q, want the apk endpoint", initiated)
	}
}

// TestInternalShareRefusesARebuiltFile: a confirm token outlives the command
// that produced it, and the obvious way to spend that window is another build.
func TestInternalShareRefusesARebuiltFile(t *testing.T) {
	var initiated string
	api := internalShareAPI(t, `{"downloadUrl":"https://example.com/x"}`, &initiated)
	client := newTestClient(t, api)
	ctx := context.Background()

	path := writeTempFile(t, "app.aab", 1024)
	preview, err := runInternalShare(ctx, client, InternalShareArgs{File: path})
	if err != nil {
		t.Fatalf("runInternalShare: %v", err)
	}
	if err := os.WriteFile(path, []byte("a different build entirely"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if _, err := applyConfirmed(ctx, client, "internal_share", preview.ConfirmToken); err == nil {
		t.Fatal("expected the changed file to be refused")
	} else if !strings.Contains(err.Error(), "changed since it was previewed") {
		t.Errorf("error should name the mismatch: %v", err)
	}
	if initiated != "" {
		t.Errorf("the rebuilt file was uploaded anyway: %q", initiated)
	}
}

func TestInternalShareRefusesSomethingThatIsNotAnArtifact(t *testing.T) {
	var initiated string
	client := newTestClient(t, internalShareAPI(t, `{}`, &initiated))

	_, err := runInternalShare(context.Background(), client, InternalShareArgs{
		File: writeTempFile(t, "notes.txt", 16),
	})
	if err == nil || !strings.Contains(err.Error(), ".aab") {
		t.Errorf("expected a non-artifact to be refused by extension: %v", err)
	}
}
