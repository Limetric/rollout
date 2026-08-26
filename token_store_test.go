package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// isolateTokenStore points the store at a temp directory and returns it.
func isolateTokenStore(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tokens")
	t.Setenv(tokenStoreEnv, dir)
	return dir
}

// captureWarnings redirects warnOnce output and returns a reader for it. The
// once-per-process dedup is reset so tests don't silence each other.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	warnedMu.Lock()
	original := warnWriter
	warnWriter = &buf
	warned = map[string]bool{}
	warnedMu.Unlock()
	t.Cleanup(func() {
		warnedMu.Lock()
		warnWriter = original
		warned = map[string]bool{}
		warnedMu.Unlock()
	})
	return &buf
}

func TestTokenStoreRoundtrip(t *testing.T) {
	dir := isolateTokenStore(t)

	if tok, err := readStoredToken(playPlatformName); err != nil || tok != nil {
		t.Fatalf("an empty store should read as not-signed-in: %v, %v", tok, err)
	}

	want := &storedToken{
		RefreshToken: "1//refresh",
		UpdatedAt:    time.Now().UTC(),
		Source:       "rollout login play",
		ClientID:     "abc.apps.googleusercontent.com",
	}
	if err := writeStoredToken(playPlatformName, want); err != nil {
		t.Fatalf("writeStoredToken: %v", err)
	}

	got, err := readStoredToken(playPlatformName)
	if err != nil {
		t.Fatalf("readStoredToken: %v", err)
	}
	if got.RefreshToken != want.RefreshToken || got.ClientID != want.ClientID || got.Source != want.Source {
		t.Errorf("round-tripped as %+v, want %+v", got, want)
	}

	// The file holds a live credential.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, playPlatformName+".json"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("token file mode = %v, want 0600", perm)
		}
		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("token dir mode = %v, want 0700", perm)
		}
	}
}

// TestTokenStoreEnvOverride is how a container or CI mounts a writable store,
// and how two profiles keep separate sign-ins.
func TestTokenStoreEnvOverride(t *testing.T) {
	dir := isolateTokenStore(t)
	path, err := tokenStorePath(playPlatformName)
	if err != nil {
		t.Fatalf("tokenStorePath: %v", err)
	}
	if path != filepath.Join(dir, "play.json") {
		t.Errorf("token store path = %q, want it under %s", path, dir)
	}
}

func TestReadStoredTokenRejectsCorruptFile(t *testing.T) {
	dir := isolateTokenStore(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "play.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := readStoredToken(playPlatformName)
	if err == nil {
		t.Fatal("expected an error for a corrupt store")
	}
	// The message has to say what to do, or the user is left with a file they
	// don't know they can delete.
	if !strings.Contains(err.Error(), "rollout login play") {
		t.Errorf("error should name the fix: %v", err)
	}
}

// TestEmptyStoredTokenReadsAsNotSignedIn: a file left behind by a half-written
// sign-in must not look like a credential.
func TestEmptyStoredTokenReadsAsNotSignedIn(t *testing.T) {
	dir := isolateTokenStore(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "play.json"), []byte(`{"refresh_token":""}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	tok, err := readStoredToken(playPlatformName)
	if err != nil || tok != nil {
		t.Errorf("got %v, %v — want not-signed-in", tok, err)
	}
}

func TestTokenStorePathRejectsBadPlatformNames(t *testing.T) {
	isolateTokenStore(t)
	// The platform name is a compile-time constant, but this is the one place
	// it reaches the filesystem.
	for _, name := range []string{"", "../etc", "Play", "play_store", "play/x"} {
		if _, err := tokenStorePath(name); err == nil {
			t.Errorf("platform name %q should have been rejected", name)
		}
	}
}

// TestPersistingTokenSourceWritesRotatedTokens is the whole reason the store
// exists: a provider that mints a new refresh token has to have it saved, or
// the next process presents a dead one.
func TestPersistingTokenSourceWritesRotatedTokens(t *testing.T) {
	isolateTokenStore(t)
	if err := writeStoredToken(playPlatformName, &storedToken{RefreshToken: "old", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	src := &persistingTokenSource{
		policy:   playTokenPolicy,
		src:      oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "at", RefreshToken: "new"}),
		clientID: "abc.apps.googleusercontent.com",
		current:  "old",
	}
	if _, err := src.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}

	stored, err := readStoredToken(playPlatformName)
	if err != nil {
		t.Fatalf("readStoredToken: %v", err)
	}
	if stored.RefreshToken != "new" {
		t.Errorf("stored refresh token = %q, want the rotated one", stored.RefreshToken)
	}
	if stored.Source != "token refresh" || stored.ClientID != "abc.apps.googleusercontent.com" {
		t.Errorf("rotation lost its provenance: %+v", stored)
	}
}

// TestPersistingTokenSourceLeavesUnchangedTokensAlone: Google's refresh
// responses carry the same refresh token, and rewriting the store on every
// single API call would be pointless disk churn.
func TestPersistingTokenSourceLeavesUnchangedTokensAlone(t *testing.T) {
	dir := isolateTokenStore(t)
	if err := writeStoredToken(playPlatformName, &storedToken{RefreshToken: "same", UpdatedAt: time.Now().UTC(), Source: "rollout login play"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	before, err := os.Stat(filepath.Join(dir, "play.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	src := &persistingTokenSource{
		policy:  playTokenPolicy,
		src:     oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "at", RefreshToken: "same"}),
		current: "same",
	}
	if _, err := src.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}

	stored, err := readStoredToken(playPlatformName)
	if err != nil {
		t.Fatalf("readStoredToken: %v", err)
	}
	if stored.Source != "rollout login play" {
		t.Errorf("an unchanged token should not have been rewritten: %+v", stored)
	}
	after, err := os.Stat(filepath.Join(dir, "play.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the store was rewritten for an unchanged refresh token")
	}
}

// errTokenSource fails the way a revoked refresh token does.
type errTokenSource struct{ err error }

func (e errTokenSource) Token() (*oauth2.Token, error) { return nil, e.err }

func TestInvalidGrantIsActionable(t *testing.T) {
	isolateTokenStore(t)
	retrieve := &oauth2.RetrieveError{
		Response:  &http.Response{StatusCode: http.StatusBadRequest},
		ErrorCode: "invalid_grant",
	}
	src := &persistingTokenSource{policy: playTokenPolicy, src: errTokenSource{err: retrieve}}

	_, err := src.Token()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "rollout login play") {
		t.Errorf("invalid_grant should name the fix: %v", err)
	}
	// doctor classifies by the wrapped oauth2 error, so wrapping must survive:
	// a revoked token is a broken setup, not a transient failure.
	var wrapped *oauth2.RetrieveError
	if !errors.As(err, &wrapped) {
		t.Errorf("the oauth2 error must stay unwrappable: %v", err)
	}
	if liveVerdictFor(err) != liveFailed {
		t.Errorf("invalid_grant should be a definitive failure, got %v", liveVerdictFor(err))
	}
}

// TestClientBindingMismatchWarns names the failure instead of letting it
// surface as a bare invalid_grant on the next call.
func TestClientBindingMismatchWarns(t *testing.T) {
	warnings := captureWarnings(t)

	stored := &storedToken{RefreshToken: "rt", ClientID: "old.apps.googleusercontent.com"}
	checkClientBinding(playTokenPolicy, stored, "new.apps.googleusercontent.com")
	if !strings.Contains(warnings.String(), "old.apps.googleusercontent.com") {
		t.Errorf("expected a binding warning, got %q", warnings.String())
	}

	// A matching binding, an unknown binding, and no configured client are all
	// silent — a warning nobody can act on is noise.
	warnings.Reset()
	checkClientBinding(playTokenPolicy, stored, "old.apps.googleusercontent.com")
	checkClientBinding(playTokenPolicy, &storedToken{RefreshToken: "rt"}, "new.apps.googleusercontent.com")
	checkClientBinding(playTokenPolicy, stored, "")
	if warnings.String() != "" {
		t.Errorf("unexpected warnings: %q", warnings.String())
	}
}

func TestWarnOnceDeduplicates(t *testing.T) {
	warnings := captureWarnings(t)
	warnOnce("token store %s is unwritable", "/tmp/x")
	warnOnce("token store %s is unwritable", "/tmp/x")
	warnOnce("token store %s is unwritable", "/tmp/y")
	if got := strings.Count(warnings.String(), "\n"); got != 2 {
		t.Errorf("expected 2 distinct warnings, got %d:\n%s", got, warnings.String())
	}
}

func TestDescribeTokenStore(t *testing.T) {
	isolateTokenStore(t)

	empty := describeTokenStore(playPlatformName)
	if empty.Token != nil || empty.WriteErr != nil {
		t.Fatalf("unexpected status: %+v", empty)
	}
	if !strings.Contains(empty.describe(playTokenPolicy), "rollout login play") {
		t.Errorf("an empty store should point at the login command: %q", empty.describe(playTokenPolicy))
	}

	if err := writeStoredToken(playPlatformName, &storedToken{
		RefreshToken: "rt",
		UpdatedAt:    time.Now().UTC().Add(-3 * time.Hour),
		Source:       "rollout login play",
	}); err != nil {
		t.Fatalf("writeStoredToken: %v", err)
	}
	saved := describeTokenStore(playPlatformName)
	desc := saved.describe(playTokenPolicy)
	if !strings.Contains(desc, "3 hours ago") {
		t.Errorf("token age = %q, want it to report 3 hours", desc)
	}
	// The token itself must never reach a report.
	if strings.Contains(desc, "rt") && !strings.Contains(desc, "hours") {
		t.Errorf("store description leaked the token: %q", desc)
	}
}

func TestDescribeTokenStoreReportsUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not gate writes here")
	}
	dir := t.TempDir()
	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv(tokenStoreEnv, filepath.Join(readonly, "tokens"))

	st := describeTokenStore(playPlatformName)
	if st.WriteErr == nil {
		t.Fatal("expected the store to be reported as unwritable")
	}
	if !strings.Contains(st.location(), "NOT WRITABLE") {
		t.Errorf("location should say so: %q", st.location())
	}
	if err := requireWritableStore(playPlatformName); err == nil {
		t.Error("requireWritableStore should have failed")
	}
}

// TestProbeWritableDoesNotCreateTheDirectory: `config show` must be able to ask
// whether a store is writable without turning a mistyped ROLLOUT_TOKEN_STORE
// into a real, empty, perfectly writable one.
func TestProbeWritableDoesNotCreateTheDirectory(t *testing.T) {
	dir := isolateTokenStore(t)
	if err := probeWritable(dir); err != nil {
		t.Fatalf("probeWritable: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("probing should not have created %s", dir)
	}
}

func TestHumanizeAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Hour, "in the future (check the system clock)"},
		{30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{90 * time.Minute, "1 hour ago"},
		{3 * time.Hour, "3 hours ago"},
		{25 * time.Hour, "1 day ago"},
		{72 * time.Hour, "3 days ago"},
	}
	for _, tc := range tests {
		if got := humanizeAge(tc.d); got != tc.want {
			t.Errorf("humanizeAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestStoredTokenJSONShape(t *testing.T) {
	// The file is user-visible and gets copied between machines; its keys are
	// part of the contract.
	data, err := json.Marshal(storedToken{RefreshToken: "rt", ClientID: "cid", Source: "s"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"refresh_token"`, `"updated_at"`, `"source"`, `"client_id"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("stored token JSON missing %s: %s", want, data)
		}
	}
}
