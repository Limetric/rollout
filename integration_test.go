//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Live smoke tests against a real Play Console app.
//
// They are behind the `integration` build tag and skip themselves when the
// credentials are absent, so `go test -tags integration ./...` is safe to run
// anywhere. CI triggers them only on manual dispatch.
//
// Two rules the suite holds to, because it runs against a real developer
// account: it never uploads an artifact, and it never touches the production
// track. The one write it performs is a no-op — setting a testing track's
// tester groups to the value they already have — which still exercises the
// whole staging, confirm, validate, and commit path.

// integrationClient builds a real client, or skips the test.
func integrationClient(t *testing.T) (*Client, string) {
	t.Helper()
	if os.Getenv("PLAY_SERVICE_ACCOUNT_JSON") == "" && os.Getenv("PLAY_SERVICE_ACCOUNT_FILE") == "" {
		t.Skip("set PLAY_SERVICE_ACCOUNT_JSON or PLAY_SERVICE_ACCOUNT_FILE to run the integration suite")
	}
	pkg := os.Getenv("PLAY_PACKAGE_NAME")
	if pkg == "" {
		t.Skip("set PLAY_PACKAGE_NAME to run the integration suite")
	}
	// A loopback base URL would put the config in test mode and defeat the
	// point, so refuse rather than silently pass.
	if base := os.Getenv("PLAY_API_BASE_URL"); base != "" && isLoopbackURL(base) {
		t.Fatalf("PLAY_API_BASE_URL points at %s — the integration suite must run against the real API", base)
	}

	cfg, err := loadPlayConfig("")
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	client, err := NewClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, pkg
}

// TestIntegrationDoctor is the first thing to check: everything below assumes
// the credential can open an edit on this app.
func TestIntegrationDoctor(t *testing.T) {
	_, pkg := integrationClient(t)

	cfg, err := loadPlayConfig("")
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	var out strings.Builder
	res, err := playDoctor(context.Background(), &out, false)
	t.Log(out.String())
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if res != liveOK {
		t.Fatalf("doctor verdict = %v, want READY", res)
	}
	if cfg.PackageName != pkg {
		t.Errorf("doctor probed %s, want %s", cfg.PackageName, pkg)
	}
}

func TestIntegrationReads(t *testing.T) {
	client, pkg := integrationClient(t)
	ctx := context.Background()

	t.Run("apps", func(t *testing.T) {
		res, err := runApps(ctx, client, AppsArgs{})
		if err != nil {
			t.Fatalf("apps: %v", err)
		}
		if len(res.Apps) == 0 {
			t.Fatal("no apps returned")
		}
		if res.Message != "" {
			// Not a failure: a release-only credential legitimately cannot
			// enumerate. Worth seeing in the log either way.
			t.Logf("apps note: %s", res.Message)
		}
		if res.Apps[0].PackageName != pkg {
			t.Errorf("the configured app should be first, got %s", res.Apps[0].PackageName)
		}
	})

	t.Run("tracks", func(t *testing.T) {
		res, err := runTracks(ctx, client, TracksArgs{})
		if err != nil {
			t.Fatalf("tracks: %v", err)
		}
		if len(res.Tracks) == 0 {
			t.Fatal("no tracks returned — every app has at least the four standard tracks")
		}
		for _, track := range res.Tracks {
			t.Logf("track %s: %d release(s)", track.Track, len(track.Releases))
		}
	})

	t.Run("listing", func(t *testing.T) {
		res, err := runListing(ctx, client, ListingArgs{})
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		t.Logf("listing locales: %d", len(res.Listings))
	})

	t.Run("edit status", func(t *testing.T) {
		res, err := runEditStatus(ctx, client, EditStatusArgs{})
		if err != nil {
			t.Fatalf("edit status: %v", err)
		}
		if !res.CanEdit {
			t.Fatalf("cannot open an edit: %s", res.Reason)
		}
	})

	t.Run("vitals summary", func(t *testing.T) {
		res, err := runVitalsSummary(ctx, client, VitalsSummaryArgs{Days: 7})
		if err != nil {
			// A credential without "View app information" fails here and
			// nowhere else, which is exactly the signal worth logging.
			t.Skipf("vitals unavailable for this credential: %v", err)
		}
		for _, metric := range res.Metrics {
			t.Logf("%s: status=%s", metric.Metric, metric.Status)
		}
	})
}

// TestIntegrationWriteRoundTrip exercises the whole write path — preview,
// confirm token, edit insert, validate, commit, audit line — with a change that
// changes nothing: the internal track's tester groups set to what they already
// are.
func TestIntegrationWriteRoundTrip(t *testing.T) {
	client, _ := integrationClient(t)
	ctx := context.Background()

	current, err := runTesters(ctx, client, TestersArgs{Track: "internal"})
	if err != nil {
		t.Fatalf("read testers: %v", err)
	}
	if len(current.GoogleGroups) == 0 {
		t.Skip("the internal track has no tester groups — the no-op write needs an existing value to restore")
	}

	preview, err := runSetTesters(ctx, client, SetTestersArgs{
		Track: "internal", Groups: current.GoogleGroups,
	})
	if err != nil {
		t.Fatalf("stage the write: %v", err)
	}
	if preview.Applied || preview.ConfirmToken == "" {
		t.Fatalf("expected a preview and a token, got %+v", preview)
	}
	if !strings.Contains(preview.Preview, "no change") {
		t.Errorf("a no-op should be described as one:\n%s", preview.Preview)
	}

	before, err := readAuditLog()
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}

	applied, err := applyConfirmed(ctx, client, "set_testers", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !applied.Applied {
		t.Fatalf("the write did not apply: %+v", applied)
	}
	if applied.EditID == "" {
		t.Error("the applied write should report the edit it committed in")
	}

	after, err := readAuditLog()
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("audit log grew by %d lines, want 1", len(after)-len(before))
	}

	var entry struct {
		Tool    string `json:"tool"`
		Applied bool   `json:"applied"`
		EditID  string `json:"edit_id"`
	}
	if err := json.Unmarshal([]byte(after[len(after)-1]), &entry); err != nil {
		t.Fatalf("audit line is not JSON: %v", err)
	}
	if entry.Tool != "set_testers" || !entry.Applied || entry.EditID != applied.EditID {
		t.Errorf("unexpected audit entry: %+v", entry)
	}

	// Confirm the groups survived the round trip unchanged.
	restored, err := runTesters(ctx, client, TestersArgs{Track: "internal"})
	if err != nil {
		t.Fatalf("re-read testers: %v", err)
	}
	added, removed := diffStrings(current.GoogleGroups, restored.GoogleGroups)
	if len(added) > 0 || len(removed) > 0 {
		t.Errorf("the no-op write changed the tester groups: added %v, removed %v", added, removed)
	}
}

// guardMarker separates the suite from the guard that inspects it. Everything
// below the marker is excluded from the scan, so the guard's own examples of
// forbidden code do not trip it.
const guardMarker = "// --- suite guard below this line ---"

// --- suite guard below this line ---

// TestIntegrationNeverTouchesProduction is a guard on the suite itself: a
// future test that reaches for the production track, or uploads an artifact,
// should fail here rather than in someone's release.
func TestIntegrationNeverTouchesProduction(t *testing.T) {
	source, err := os.ReadFile("integration_test.go")
	if err != nil {
		t.Fatalf("read this file: %v", err)
	}
	suite, _, found := strings.Cut(string(source), guardMarker)
	if !found {
		t.Fatal("the guard marker is missing — this test can no longer tell the suite from itself")
	}
	for _, forbidden := range []string{
		`Track: "production"`,
		"Track: productionTrack",
		"runUploadArtifact(",
		"runPromoteRelease(",
		"runCompleteRelease(",
		"runHaltRelease(",
	} {
		if strings.Contains(suite, forbidden) {
			t.Errorf("the integration suite must not use %q — it runs against a real developer account", forbidden)
		}
	}
}
