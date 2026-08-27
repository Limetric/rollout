package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakePrompter answers the wizard from a script. An exhausted script returns
// io.EOF, which is what a user pressing Ctrl-D looks like.
type fakePrompter struct {
	answers []string
	asked   []string
}

func (f *fakePrompter) next(prompt string) (string, error) {
	f.asked = append(f.asked, prompt)
	if len(f.answers) == 0 {
		return "", io.EOF
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}

func (f *fakePrompter) line(prompt string) (string, error)   { return f.next(prompt) }
func (f *fakePrompter) secret(prompt string) (string, error) { return f.next(prompt) }
func (f *fakePrompter) confirm(prompt string, def bool) (bool, error) {
	s, err := f.next(prompt)
	if err != nil {
		return false, err
	}
	if s == "" {
		return def, nil
	}
	return strings.EqualFold(s, "y") || strings.EqualFold(s, "yes"), nil
}

func TestParseCredentialsJSON(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantKind string
		wantErr  string
	}{
		{
			name:     "desktop app client",
			body:     `{"installed":{"client_id":"abc","client_secret":"s"}}`,
			wantKind: "installed",
		},
		{
			name:     "web client is accepted with a warning path",
			body:     `{"web":{"client_id":"abc","client_secret":"s"}}`,
			wantKind: "web",
		},
		{
			// Both files come off the same Cloud Console page, so the error has
			// to name the flag that does want this one.
			name:    "a service-account key names the right flag",
			body:    `{"type":"service_account","client_email":"bot@p","private_key":"k"}`,
			wantErr: "--service-account",
		},
		{
			name:    "anything else says what is expected",
			body:    `{"something":"else"}`,
			wantErr: "Desktop app",
		},
		{
			name:    "malformed json",
			body:    `{`,
			wantErr: "parse credentials JSON",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			creds, err := parseCredentialsJSON([]byte(tc.body))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCredentialsJSON: %v", err)
			}
			if creds.kind != tc.wantKind || creds.clientID != "abc" {
				t.Errorf("unexpected creds: %+v", creds)
			}
		})
	}
}

// TestLoopbackOAuthCapturesTheCode drives the real loopback server the way a
// browser would.
func TestLoopbackOAuthCapturesTheCode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	conf := &oauth2.Config{
		ClientID:    "abc",
		Endpoint:    oauth2.Endpoint{AuthURL: "https://accounts.example.com/auth", TokenURL: "https://oauth2.example.com/token"},
		RedirectURL: loopbackRedirectURL(port),
	}

	// The fake "browser": parse the state out of the consent URL and call back.
	visit := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		go func() {
			resp, err := http.Get(loopbackRedirectURL(port) + "?state=" + u.Query().Get("state") + "&code=auth-code")
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}

	code, err := runLoopbackOAuth(context.Background(), conf, visit, ln)
	if err != nil {
		t.Fatalf("runLoopbackOAuth: %v", err)
	}
	if code != "auth-code" {
		t.Errorf("captured code = %q", code)
	}
}

// TestLoopbackOAuthRejectsStateMismatch is the CSRF guard.
func TestLoopbackOAuthRejectsStateMismatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	conf := &oauth2.Config{ClientID: "abc", RedirectURL: loopbackRedirectURL(port),
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.com/auth"}}

	visit := func(string) error {
		go func() {
			resp, err := http.Get(loopbackRedirectURL(port) + "?state=wrong&code=auth-code")
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	if _, err := runLoopbackOAuth(context.Background(), conf, visit, ln); err == nil {
		t.Fatal("expected a state mismatch to abort the sign-in")
	}
}

// TestLoopbackOAuthIgnoresStrayRequests: a browser preconnect or a favicon
// fetch hitting the callback port must not abort the login before the real
// redirect arrives.
func TestLoopbackOAuthIgnoresStrayRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	conf := &oauth2.Config{ClientID: "abc", RedirectURL: loopbackRedirectURL(port),
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.com/auth"}}

	visit := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		go func() {
			// A stray request first, then the real callback.
			if resp, err := http.Get(loopbackRedirectURL(port) + "/favicon.ico"); err == nil {
				resp.Body.Close()
			}
			if resp, err := http.Get(loopbackRedirectURL(port) + "?state=" + u.Query().Get("state") + "&code=real-code"); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}

	code, err := runLoopbackOAuth(context.Background(), conf, visit, ln)
	if err != nil {
		t.Fatalf("runLoopbackOAuth: %v", err)
	}
	if code != "real-code" {
		t.Errorf("captured code = %q", code)
	}
}

// TestExchangeRefreshTokenExplainsAMissingRefreshToken: the usual cause is a
// misconfigured OAuth client, which the raw response does not say.
func TestExchangeRefreshTokenExplainsAMissingRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer"})
	}))
	defer srv.Close()

	conf := &oauth2.Config{ClientID: "abc", Endpoint: oauth2.Endpoint{TokenURL: srv.URL}}
	_, err := exchangeRefreshToken(context.Background(), conf, "code")
	if err == nil || !strings.Contains(err.Error(), "Desktop app") {
		t.Fatalf("err = %v, want one naming the usual cause", err)
	}
}

// TestSignInLoopbackSendsPKCE: the client secret in a Desktop app is not
// secret, so the verifier is what actually binds the code to this process.
func TestSignInLoopbackSendsPKCE(t *testing.T) {
	clearPlayEnv(t)

	var authQuery url.Values
	var tokenForm url.Values
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "refresh_token": "1//rt", "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()

	original := playOAuthEndpoint
	playOAuthEndpoint = oauth2.Endpoint{AuthURL: "https://accounts.example.com/auth", TokenURL: tokenSrv.URL}
	t.Cleanup(func() { playOAuthEndpoint = original })

	// Pick a free port the sign-in can bind.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	visit := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		authQuery = u.Query()
		go func() {
			if resp, err := http.Get(loopbackRedirectURL(port) + "?state=" + authQuery.Get("state") + "&code=auth-code"); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}

	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL}
	creds := clientCreds{clientID: "abc", clientSecret: "s", kind: "installed"}
	refreshToken, err := signInLoopback(context.Background(), cfg, creds, visit, port)
	if err != nil {
		t.Fatalf("signInLoopback: %v", err)
	}
	if refreshToken != "1//rt" {
		t.Errorf("refresh token = %q", refreshToken)
	}
	if authQuery.Get("code_challenge") == "" || authQuery.Get("code_challenge_method") != "S256" {
		t.Errorf("authorization request did not carry a PKCE challenge: %v", authQuery)
	}
	if tokenForm.Get("code_verifier") == "" {
		t.Errorf("token exchange did not carry the PKCE verifier: %v", tokenForm)
	}
	// access_type=offline and prompt=consent are what make Google return a
	// refresh token at all.
	if authQuery.Get("access_type") != "offline" || authQuery.Get("prompt") != "consent" {
		t.Errorf("authorization request is missing offline consent: %v", authQuery)
	}
}

func TestSavePlayCredentialsSplitsClientAndToken(t *testing.T) {
	clearPlayEnv(t)
	isolateTokenStore(t)
	path := filepath.Join(t.TempDir(), "config.toml")

	creds := clientCreds{clientID: "abc.apps.googleusercontent.com", clientSecret: "GOCSPX-secret", kind: "installed"}
	if err := savePlayCredentials(path, creds, "1//rt"); err != nil {
		t.Fatalf("savePlayCredentials: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	// The client identity is stable configuration; the refresh token is a live
	// credential and must never land in the config file.
	if !strings.Contains(string(data), "abc.apps.googleusercontent.com") {
		t.Errorf("config should hold the client id:\n%s", data)
	}
	if strings.Contains(string(data), "1//rt") {
		t.Fatalf("the refresh token must not be written to the config file:\n%s", data)
	}

	stored, err := readStoredToken(playPlatformName)
	if err != nil || stored == nil {
		t.Fatalf("token store: %v, %v", stored, err)
	}
	if stored.RefreshToken != "1//rt" || stored.ClientID != creds.clientID {
		t.Errorf("stored token = %+v", stored)
	}
}

// TestSavePlayCredentialsRollsBackOnStoreFailure: a refresh token is only
// usable alongside the client that minted it, so half a save would replace a
// working pair with a broken one.
func TestSavePlayCredentialsRollsBackOnStoreFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not gate writes for root")
	}
	clearPlayEnv(t)
	dir := t.TempDir()
	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv(tokenStoreEnv, filepath.Join(readonly, "tokens"))

	path := filepath.Join(dir, "config.toml")
	before := "[play]\nclient_id = \"old.apps.googleusercontent.com\"\nclient_secret = \"old-secret\"\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	creds := clientCreds{clientID: "new.apps.googleusercontent.com", clientSecret: "new-secret"}
	err := savePlayCredentials(path, creds, "1//rt")
	if err == nil {
		t.Fatal("expected the save to fail when the store is unwritable")
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read config: %v", readErr)
	}
	if !strings.Contains(string(after), "old.apps.googleusercontent.com") {
		t.Errorf("the working client should have survived a failed sign-in:\n%s", after)
	}
}

// TestServiceAccountLoginWithoutAPackageListsTheApps: the user's next step is
// choosing a default app, and the sign-in has just proved which ones this key
// can reach — making them run another command to find out is a wasted round.
func TestServiceAccountLoginWithoutAPackageListsTheApps(t *testing.T) {
	clearPlayEnv(t)
	isolateTokenStore(t)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	if err := os.WriteFile(keyPath, []byte(generateServiceAccountKey(t, "bot@p.iam.gserviceaccount.com")), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[{"packageName":"com.example.app"},{"packageName":"com.example.other"}]}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	cfg := &PlayConfig{BaseURL: srv.URL, ReportingBaseURL: srv.URL, StorageBaseURL: srv.URL}
	if err := runServiceAccountLogin(context.Background(), &out, cfg, filepath.Join(dir, "config.toml"), keyPath); err != nil {
		t.Fatalf("runServiceAccountLogin: %v", err)
	}
	for _, want := range []string{"com.example.app", "com.example.other", "set-package", "READY"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("login output missing %q:\n%s", want, out.String())
		}
	}
}

// TestServiceAccountLoginSurvivesARateLimitedProbe: the token exchange has
// already proved the credential and the config is written by this point, so a
// quota refusal from the probe must not report the sign-in as failed.
func TestServiceAccountLoginSurvivesARateLimitedProbe(t *testing.T) {
	clearPlayEnv(t)
	isolateTokenStore(t)
	originalDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = originalDelay })

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	if err := os.WriteFile(keyPath, []byte(generateServiceAccountKey(t, "bot@p.iam.gserviceaccount.com")), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`))
	}))
	defer srv.Close()

	configFile := filepath.Join(dir, "config.toml")
	var out bytes.Buffer
	cfg := &PlayConfig{BaseURL: srv.URL, ReportingBaseURL: srv.URL, StorageBaseURL: srv.URL}
	if err := runServiceAccountLogin(context.Background(), &out, cfg, configFile, keyPath); err != nil {
		t.Fatalf("a throttled probe must not fail the sign-in: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "READY") {
		t.Errorf("login should still report success:\n%s", out.String())
	}
	data, err := os.ReadFile(configFile)
	if err != nil || !strings.Contains(string(data), keyPath) {
		t.Errorf("the credential should be persisted (err %v):\n%s", err, data)
	}
}

// TestServiceAccountLoginRecordsThePathNotTheKey: nothing is copied, so there
// is one copy of the private key on the machine, in the place the user chose.
func TestServiceAccountLoginRecordsThePathNotTheKey(t *testing.T) {
	clearPlayEnv(t)
	isolateTokenStore(t)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	keyJSON := generateServiceAccountKey(t, "bot@p.iam.gserviceaccount.com")
	if err := os.WriteFile(keyPath, []byte(keyJSON), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	configFile := filepath.Join(dir, "config.toml")

	// With no package there is no app to open an edit on, but the sign-in still
	// asks Reporting what this key can see — point that at a fake so the test
	// stays offline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apps":[]}`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	cfg := &PlayConfig{BaseURL: srv.URL, ReportingBaseURL: srv.URL, StorageBaseURL: srv.URL}
	if err := runServiceAccountLogin(context.Background(), &out, cfg, configFile, keyPath); err != nil {
		t.Fatalf("runServiceAccountLogin: %v", err)
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), keyPath) {
		t.Errorf("config should record the key path:\n%s", data)
	}
	if strings.Contains(string(data), "PRIVATE KEY") {
		t.Fatalf("the key itself must not be copied into the config:\n%s", data)
	}
	// The service-account email is what the user has to invite in the Console,
	// so the sign-in has to tell them what it is.
	if !strings.Contains(out.String(), "bot@p.iam.gserviceaccount.com") {
		t.Errorf("login output should name the service account:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Users & permissions") {
		t.Errorf("login output should point at the Console step:\n%s", out.String())
	}
}

func TestServiceAccountLoginRejectsABadKey(t *testing.T) {
	clearPlayEnv(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	if err := os.WriteFile(keyPath, []byte(`{"installed":{"client_id":"abc"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL}
	err := runServiceAccountLogin(context.Background(), &out, cfg, filepath.Join(dir, "config.toml"), keyPath)
	if err == nil {
		t.Fatal("expected an error for a key that is not a service-account key")
	}
	// Nothing may be written when the key is rejected.
	if _, statErr := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(statErr) {
		t.Error("a rejected key must not be recorded in the config")
	}
}

// TestNoInputLoginFailsWithAMessage: `--no-input` in CI must say what is
// missing rather than block on a prompt nobody can answer.
func TestNoInputLoginFailsWithAMessage(t *testing.T) {
	clearPlayEnv(t)
	isolateTokenStore(t)

	var out bytes.Buffer
	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL}
	err := runNonInteractiveLogin(context.Background(), &out, cfg, filepath.Join(t.TempDir(), "config.toml"))
	if err == nil {
		t.Fatal("expected an error with no credentials available")
	}
	for _, want := range []string{"--credentials", "PLAY_CLIENT_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

// TestWizardServiceAccountPathRePromptsOnABadPath: a mistyped path should not
// force the user to restart the whole wizard.
func TestWizardServiceAccountPathRePromptsOnABadPath(t *testing.T) {
	clearPlayEnv(t)
	isolateTokenStore(t)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.json")
	if err := os.WriteFile(keyPath, []byte(generateServiceAccountKey(t, "bot@p.iam.gserviceaccount.com")), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	configFile := filepath.Join(dir, "config.toml")

	p := &fakePrompter{answers: []string{
		"y", "", // enable publisher API, press Enter
		"y", "", // enable reporting API, press Enter
		"y", "", // open credentials page, press Enter
		"/nope/key.json", // a bad path first
		keyPath,          // then the real one
		"y", "",          // open Users & permissions, press Enter
	}}
	var out bytes.Buffer
	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL}
	if err := wizardServiceAccount(context.Background(), &out, p, cfg, configFile, func(string) error { return nil }); err != nil {
		t.Fatalf("wizardServiceAccount: %v", err)
	}
	if cfg.ServiceAccountFile != keyPath {
		t.Errorf("configured key = %q, want %q", cfg.ServiceAccountFile, keyPath)
	}
	if !strings.Contains(out.String(), "/nope/key.json") {
		t.Errorf("the wizard should report the bad path before re-prompting:\n%s", out.String())
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	tests := []struct{ in, want string }{
		{"  /keys/play.json  ", "/keys/play.json"},
		{`"/keys/play.json"`, "/keys/play.json"},
		{"'/keys/play.json'", "/keys/play.json"},
		{"~/keys/play.json", filepath.Join(home, "keys/play.json")},
	}
	for _, tc := range tests {
		if got := expandHome(tc.in); got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSecretHintRevealsNothingUseful(t *testing.T) {
	if got := secretHint("short"); got != "…" {
		t.Errorf("secretHint(short) = %q", got)
	}
	got := secretHint("abc.apps.googleusercontent.com")
	if strings.Contains(got, "abc.apps") {
		t.Errorf("secretHint leaked the value: %q", got)
	}
}

func TestReadLine(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"hello\n", "hello", false},
		{"hello\r\n", "hello", false},
		{"trailing without newline", "trailing without newline", false},
		{"", "", true}, // an immediate EOF is an abort
	}
	for _, tc := range tests {
		got, err := readLine(strings.NewReader(tc.in))
		if tc.wantErr {
			if err == nil {
				t.Errorf("readLine(%q) should have returned an error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("readLine(%q) = %q, %v", tc.in, got, err)
		}
	}
}
