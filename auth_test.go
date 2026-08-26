package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// TestTestModeNeverReachesTheNetwork: a loopback base URL means there is
// nothing to authenticate to, and a test that silently minted a real token
// would be reaching a live credential.
func TestTestModeNeverReachesTheNetwork(t *testing.T) {
	clearPlayEnv(t)
	cfg := &PlayConfig{BaseURL: "http://127.0.0.1:9", ClientID: "abc", ClientSecret: "s"}

	ts, err := tokenSource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("tokenSource: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "test-access-token" {
		t.Errorf("test mode returned %q", tok.AccessToken)
	}
}

func TestTokenSourceWithoutCredentials(t *testing.T) {
	clearPlayEnv(t)
	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL}
	_, err := tokenSource(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected an error with no credentials")
	}
	for _, want := range []string{"PLAY_SERVICE_ACCOUNT_FILE", "rollout login play"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

// TestServiceAccountTokenSourceUsesTheKey builds a JWT config from a real
// generated key: the key is created here and never committed.
func TestServiceAccountTokenSourceUsesTheKey(t *testing.T) {
	clearPlayEnv(t)
	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL, serviceAccountJSON: generateServiceAccountKey(t, "bot@p.iam.gserviceaccount.com")}

	if _, err := serviceAccountTokenSource(context.Background(), cfg); err != nil {
		t.Fatalf("serviceAccountTokenSource: %v", err)
	}
}

// TestServiceAccountTokenSourceRejectsAnUnparseableKey: the failure has to be
// reported as a key problem, not as an auth failure the user cannot place.
func TestServiceAccountTokenSourceRejectsAnUnparseableKey(t *testing.T) {
	clearPlayEnv(t)
	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL, serviceAccountJSON: `{"client_email":"bot@p","private_key":"not-a-key"}`}

	_, err := serviceAccountTokenSource(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected an error for a key that is not a private key")
	}
	if !strings.Contains(err.Error(), "bot@p") {
		t.Errorf("error should name the service account: %v", err)
	}
}

// TestUserTokenSourceRefreshesFromTheStore is the OAuth user path end to end:
// the refresh token comes from the store, not from configuration.
func TestUserTokenSourceRefreshesFromTheStore(t *testing.T) {
	clearPlayEnv(t)
	isolateTokenStore(t)

	var gotGrant, gotRefresh, gotClientID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant, gotRefresh = r.FormValue("grant_type"), r.FormValue("refresh_token")
		// oauth2 sends the client credentials either as form values or as HTTP
		// Basic auth, depending on what it has auto-detected for the endpoint.
		gotClientID = r.FormValue("client_id")
		if user, _, ok := r.BasicAuth(); ok && user != "" {
			gotClientID = user
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "minted", "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	original := playOAuthEndpoint
	playOAuthEndpoint = oauth2.Endpoint{TokenURL: srv.URL, AuthURL: srv.URL + "/auth"}
	t.Cleanup(func() { playOAuthEndpoint = original })

	if err := writeStoredToken(playPlatformName, &storedToken{
		RefreshToken: "1//saved", UpdatedAt: time.Now().UTC(), ClientID: "abc.apps.googleusercontent.com",
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL, ClientID: "abc.apps.googleusercontent.com", ClientSecret: "s"}
	ts, err := tokenSource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("tokenSource: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "minted" {
		t.Errorf("access token = %q", tok.AccessToken)
	}
	if gotGrant != "refresh_token" || gotRefresh != "1//saved" {
		t.Errorf("token request sent grant=%q refresh=%q", gotGrant, gotRefresh)
	}
	if gotClientID != "abc.apps.googleusercontent.com" {
		t.Errorf("token request sent client_id=%q", gotClientID)
	}
}

// TestUserTokenSourceWithoutASignIn: an OAuth client with no saved token is the
// "you configured half of it" case, and must say which half.
func TestUserTokenSourceWithoutASignIn(t *testing.T) {
	clearPlayEnv(t)
	isolateTokenStore(t)

	cfg := &PlayConfig{BaseURL: defaultPlayBaseURL, ClientID: "abc", ClientSecret: "s"}
	_, err := tokenSource(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "rollout login play") {
		t.Fatalf("expected a sign-in prompt, got %v", err)
	}
}

// TestScopesReachTheAuthorizationRequest: the Reporting API is a separate
// service, and a credential minted without its scope fails only when vitals are
// first read — long after sign-in.
func TestScopesReachTheAuthorizationRequest(t *testing.T) {
	cfg := &PlayConfig{ReportsBucket: "pubsite_prod_rev_01234"}
	conf := playOAuthConfig(cfg, 8085)

	want := map[string]bool{
		"https://www.googleapis.com/auth/androidpublisher":       false,
		"https://www.googleapis.com/auth/playdeveloperreporting": false,
		"https://www.googleapis.com/auth/devstorage.read_only":   false,
	}
	for _, s := range conf.Scopes {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for scope, seen := range want {
		if !seen {
			t.Errorf("authorization request is missing scope %s", scope)
		}
	}
	if conf.RedirectURL != "http://127.0.0.1:8085" {
		t.Errorf("redirect URL = %q — Google's loopback guidance is the literal IP", conf.RedirectURL)
	}
}

// TestNewPlayHTTPClientSendsBearerToken proves the wiring: every request the
// client makes carries the minted token.
func TestNewPlayHTTPClientSendsBearerToken(t *testing.T) {
	clearPlayEnv(t)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	cfg := &PlayConfig{BaseURL: srv.URL}
	client, err := newPlayHTTPClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newPlayHTTPClient: %v", err)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer test-access-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}
