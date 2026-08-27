package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

const dataSafetyCSV = "question,response\nData collected,Yes\nData shared,No\n"

func TestUpdateDataSafetySendsTheCSVAsOneField(t *testing.T) {
	var posted string
	var body []byte
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/dataSafety") {
			posted = r.URL.Path
			body, _ = io.ReadAll(r.Body)
		}
		writeJSON(w, http.StatusOK, `{}`)
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	path := writeTempTextFile(t, "data-safety.csv", dataSafetyCSV)
	preview, err := runUpdateDataSafety(ctx, client, UpdateDataSafetyArgs{File: path})
	if err != nil {
		t.Fatalf("runUpdateDataSafety: %v", err)
	}
	if preview.Applied || preview.ConfirmToken == "" {
		t.Fatalf("first call must not apply: %+v", preview)
	}
	// There is no read for this form, so the preview cannot diff against what
	// is live — and has to say so rather than implying it did.
	if !strings.Contains(preview.Preview, "no read endpoint") {
		t.Errorf("preview should say the current declaration cannot be shown:\n%s", preview.Preview)
	}
	if !strings.Contains(preview.Preview, "3 rows") {
		t.Errorf("preview should size the file:\n%s", preview.Preview)
	}

	applyPreview(t, ctx, client, "update_data_safety", preview.ConfirmToken)

	if posted != "/androidpublisher/v3/applications/com.example.app/dataSafety" {
		t.Errorf("posted to %q", posted)
	}
	var sent struct {
		SafetyLabels string `json:"safetyLabels"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	// The endpoint takes the whole document as one string; a truncated or
	// re-serialized CSV is a different declaration.
	if sent.SafetyLabels != dataSafetyCSV {
		t.Errorf("safetyLabels = %q, want the file verbatim", sent.SafetyLabels)
	}
}

// TestUpdateDataSafetyRefusesSomethingThatIsNotCSV: the API rejects a malformed
// document with a message about the field it was handed, not the line that
// broke.
func TestUpdateDataSafetyRefusesSomethingThatIsNotCSV(t *testing.T) {
	client := newTestClient(t, newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	}))
	ctx := context.Background()

	_, err := runUpdateDataSafety(ctx, client, UpdateDataSafetyArgs{
		File: writeTempTextFile(t, "broken.csv", "a,\"unterminated\nb,c\n\"x\"y\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "not readable as CSV") {
		t.Errorf("expected a malformed CSV to be named: %v", err)
	}

	_, err = runUpdateDataSafety(ctx, client, UpdateDataSafetyArgs{
		File: writeTempTextFile(t, "empty.csv", "   \n"),
	})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected an empty file to be refused: %v", err)
	}

	_, err = runUpdateDataSafety(ctx, client, UpdateDataSafetyArgs{})
	if err == nil || !strings.Contains(err.Error(), "App content") {
		t.Errorf("a missing file should say where the CSV comes from: %v", err)
	}
}

// TestUpdateDataSafetyNeedsNoEdit: the declaration is not part of the
// publishing transaction, so wrapping it in an edit would commit an empty one.
func TestUpdateDataSafetyNeedsNoEdit(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	})
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runUpdateDataSafety(ctx, client, UpdateDataSafetyArgs{
		File: writeTempTextFile(t, "data-safety.csv", dataSafetyCSV),
	})
	if err != nil {
		t.Fatalf("runUpdateDataSafety: %v", err)
	}
	applyPreview(t, ctx, client, "update_data_safety", preview.ConfirmToken)

	for _, call := range api.calls() {
		if strings.Contains(call, "/edits") {
			t.Fatalf("the data safety write opened an edit: %v", api.calls())
		}
	}
}
