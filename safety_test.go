package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolateState points the config/state directory at a temp dir so tests never
// touch the developer's real pending tokens or audit log.
func isolateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	// os.UserConfigDir is platform-specific (XDG_CONFIG_HOME on Linux,
	// ~/Library/Application Support on macOS), so ask for the resolved path
	// rather than assuming one layout.
	state, err := stateDirPath()
	if err != nil {
		t.Fatalf("stateDirPath: %v", err)
	}
	if !strings.HasPrefix(state, dir) {
		t.Fatalf("state dir %q escaped the test HOME %q", state, dir)
	}
	return state
}

func stagePlayWrite(t *testing.T, w pendingWrite) *PendingMutation {
	t.Helper()
	if w.Platform == "" {
		w.Platform = playPlatformName
	}
	p, err := stageWrite(w)
	if err != nil {
		t.Fatalf("stageWrite: %v", err)
	}
	return p
}

// fakeApplier records what it was asked to apply, so tests can assert that an
// apply happened — or, more importantly, that it did not.
type fakeApplier struct {
	platform string
	applied  []*PendingMutation
	err      error
}

func (f *fakeApplier) platformName() string {
	if f.platform == "" {
		return playPlatformName
	}
	return f.platform
}

func (f *fakeApplier) applyMutation(_ context.Context, p *PendingMutation) (*applyOutcome, error) {
	f.applied = append(f.applied, p)
	if f.err != nil {
		return nil, f.err
	}
	return &applyOutcome{EditID: "edit-1", Detail: "applied " + p.Tool}, nil
}

func TestStageAndConfirmRoundtrip(t *testing.T) {
	stateDirPath := isolateState(t)
	p := stagePlayWrite(t, pendingWrite{
		Tool: "update_release", PackageName: "com.example.app",
		Summary: "set production to 42 at 10%", Dispatch: "track",
		Payload: json.RawMessage(`{"track":"production"}`),
	})

	if !validToken(p.Token) {
		t.Fatalf("staged token %q is not the documented shape", p.Token)
	}
	// The pending file holds a payload; it must not be world-readable.
	info, err := os.Stat(filepath.Join(stateDirPath, "pending-"+p.Token+".json"))
	if err != nil {
		t.Fatalf("stat pending file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("pending file mode = %v, want 0600", perm)
	}

	applier := &fakeApplier{}
	res, err := applyConfirmed(context.Background(), applier, "update_release", p.Token)
	if err != nil {
		t.Fatalf("applyConfirmed: %v", err)
	}
	if !res.Applied || res.EditID != "edit-1" {
		t.Errorf("unexpected result: %+v", res)
	}
	if len(applier.applied) != 1 {
		t.Fatalf("expected exactly one apply, got %d", len(applier.applied))
	}
	if got := applier.applied[0].Dispatch; got != "track" {
		t.Errorf("dispatch = %q, want it preserved through staging", got)
	}
}

// TestConfirmTokenIsSingleUse: a token that has been applied must not apply
// again. Re-running a confirm is the easiest way to promote a release twice.
func TestConfirmTokenIsSingleUse(t *testing.T) {
	isolateState(t)
	p := stagePlayWrite(t, pendingWrite{Tool: "promote_release", PackageName: "com.example.app", Summary: "promote"})

	applier := &fakeApplier{}
	if _, err := applyConfirmed(context.Background(), applier, "promote_release", p.Token); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if _, err := applyConfirmed(context.Background(), applier, "promote_release", p.Token); err == nil {
		t.Fatal("expected the second confirm to be rejected")
	}
	if len(applier.applied) != 1 {
		t.Errorf("mutation applied %d times, want 1", len(applier.applied))
	}
}

func TestConfirmRejectsWrongTool(t *testing.T) {
	isolateState(t)
	p := stagePlayWrite(t, pendingWrite{Tool: "halt_release", PackageName: "com.example.app", Summary: "halt production"})

	applier := &fakeApplier{}
	_, err := applyConfirmed(context.Background(), applier, "promote_release", p.Token)
	if err == nil {
		t.Fatal("a halt token must not be confirmable through promote")
	}
	if !strings.Contains(err.Error(), "halt_release") {
		t.Errorf("error should name the staging tool: %v", err)
	}
	if len(applier.applied) != 0 {
		t.Fatal("nothing may be applied when the tool binding fails")
	}
	// The token is discarded either way — a mismatched confirm must not leave a
	// staged write lying around for a second attempt.
	if _, err := peekMutation(p.Token); err == nil {
		t.Error("the staged write should have been discarded")
	}
}

func TestConfirmRejectsWrongPlatform(t *testing.T) {
	isolateState(t)
	p := stagePlayWrite(t, pendingWrite{Tool: "update_listing", PackageName: "com.example.app", Summary: "update listing"})

	applier := &fakeApplier{platform: "appstore"}
	_, err := applyConfirmed(context.Background(), applier, "update_listing", p.Token)
	if err == nil {
		t.Fatal("a play token must not be applied through another platform's API")
	}
	if !strings.Contains(err.Error(), playPlatformName) {
		t.Errorf("error should name the staging platform: %v", err)
	}
	if len(applier.applied) != 0 {
		t.Fatal("nothing may be applied when the platform binding fails")
	}
}

func TestConsumeRejectsExpiredToken(t *testing.T) {
	state := isolateState(t)
	p := stagePlayWrite(t, pendingWrite{Tool: "update_release", PackageName: "com.example.app", Summary: "stale"})

	// Rewrite the staged file as if it had been created before the TTL.
	path := filepath.Join(state, "pending-"+p.Token+".json")
	p.CreatedAt = time.Now().UTC().Add(-confirmTTL - time.Minute)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := consumeMutation(p.Token); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected an expiry error, got %v", err)
	}
}

// TestPendingPathRejectsMalformedTokens: the token is caller-supplied input and
// must never steer which file is read or removed.
func TestPendingPathRejectsMalformedTokens(t *testing.T) {
	isolateState(t)
	for _, token := range []string{"", "../../etc/passwd", "ZZZZZZZZZZZZZZZZ", "abc", strings.Repeat("a", 17)} {
		if _, err := pendingPath(token); err == nil {
			t.Errorf("token %q should have been rejected", token)
		}
	}
}

func TestDoubleConfirmRequiresSecondToken(t *testing.T) {
	isolateState(t)
	p := stagePlayWrite(t, pendingWrite{
		Tool: "halt_release", PackageName: "com.example.app",
		Summary: "halt the production rollout", RequiresDouble: true,
	})

	applier := &fakeApplier{}
	first, err := applyConfirmed(context.Background(), applier, "halt_release", p.Token)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.Applied {
		t.Fatal("a destructive write must not apply on its first confirmation")
	}
	if first.ConfirmToken == "" || first.ConfirmToken == p.Token {
		t.Fatalf("expected a fresh second token, got %q", first.ConfirmToken)
	}
	if !strings.Contains(first.Preview, "DESTRUCTIVE") {
		t.Errorf("second-confirmation preview should say why: %q", first.Preview)
	}
	if len(applier.applied) != 0 {
		t.Fatal("nothing may be applied before the second confirmation")
	}

	second, err := applyConfirmed(context.Background(), applier, "halt_release", first.ConfirmToken)
	if err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	if !second.Applied {
		t.Fatal("the second confirmation should have applied the write")
	}
	if len(applier.applied) != 1 {
		t.Errorf("expected exactly one apply, got %d", len(applier.applied))
	}
}

func TestAuditLogRecordsAppliesAndFailures(t *testing.T) {
	isolateState(t)

	ok := stagePlayWrite(t, pendingWrite{Tool: "update_release", PackageName: "com.example.app", Summary: "ship 42"})
	if _, err := applyConfirmed(context.Background(), &fakeApplier{}, "update_release", ok.Token); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	bad := stagePlayWrite(t, pendingWrite{Tool: "update_listing", PackageName: "com.example.app", Summary: "update listing"})
	failing := &fakeApplier{err: errors.New("editAlreadyCommitted")}
	if _, err := applyConfirmed(context.Background(), failing, "update_listing", bad.Token); err == nil {
		t.Fatal("expected the apply to fail")
	}

	entries, err := readAuditLog()
	if err != nil {
		t.Fatalf("readAuditLog: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected an audit line per confirmed write, got %d: %v", len(entries), entries)
	}

	var applied struct {
		Tool    string `json:"tool"`
		Package string `json:"package"`
		Applied bool   `json:"applied"`
		EditID  string `json:"edit_id"`
	}
	if err := json.Unmarshal([]byte(entries[0]), &applied); err != nil {
		t.Fatalf("audit line is not JSON: %v", err)
	}
	if !applied.Applied || applied.Tool != "update_release" || applied.Package != "com.example.app" || applied.EditID != "edit-1" {
		t.Errorf("unexpected success entry: %+v", applied)
	}

	var failedEntry struct {
		Applied bool   `json:"applied"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(entries[1]), &failedEntry); err != nil {
		t.Fatalf("audit line is not JSON: %v", err)
	}
	// A failed apply is exactly the event someone reconstructing a release
	// wants to find, so it has to carry the reason.
	if failedEntry.Applied || !strings.Contains(failedEntry.Error, "editAlreadyCommitted") {
		t.Errorf("unexpected failure entry: %+v", failedEntry)
	}
}

func TestSweepExpiredRemovesStalePendingFiles(t *testing.T) {
	state := isolateState(t)
	p := stagePlayWrite(t, pendingWrite{Tool: "update_release", PackageName: "com.example.app", Summary: "stale"})
	path := filepath.Join(state, "pending-"+p.Token+".json")

	old := time.Now().Add(-confirmTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	sweepExpired(state)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expired pending file should have been swept: %v", err)
	}
}

func TestStageWriteRequiresAPlatform(t *testing.T) {
	isolateState(t)
	// A staged write with no platform could never be routed by
	// `rollout confirm`, so it must fail at staging rather than at apply.
	if _, err := stageWrite(pendingWrite{Tool: "update_release", Summary: "no platform"}); err == nil {
		t.Fatal("expected staging without a platform to fail")
	}
}

func TestPreviewTextNamesToolPackageAndToken(t *testing.T) {
	isolateState(t)
	p := stagePlayWrite(t, pendingWrite{
		Tool: "update_release", PackageName: "com.example.app",
		Summary: "set production to version code 42 at 10%",
	})
	preview := previewResult(p).Preview
	for _, want := range []string{"update_release", "com.example.app", "version code 42", p.Token, "rollout confirm"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview)
		}
	}
	// The preview must not claim anything has happened yet.
	if !strings.Contains(preview, "Nothing has been changed yet") {
		t.Errorf("preview should say nothing changed:\n%s", preview)
	}
}
