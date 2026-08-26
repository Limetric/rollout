package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Credentials for Google Play. Two modes are supported:
//
//   - a service-account JSON key — the headless path Google recommends for the
//     Publisher and Reporting APIs, granted access in Play Console → Users &
//     permissions, and the only sensible choice for CI;
//   - an OAuth user sign-in, for a CLI a human runs on their own laptop with
//     their own Console login (see login.go).
//
// A loopback base URL is test mode: there is nothing to authenticate to, so a
// static dummy token is used and no network call is made.

// tokenSource builds the OAuth2 token source for the configured credential
// mode.
func tokenSource(ctx context.Context, cfg *PlayConfig) (oauth2.TokenSource, error) {
	if cfg.isTest() {
		// A fixed, obviously-fake token. Tests assert on the Authorization
		// header, and a real credential must never be reached from here.
		return oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: "test-access-token",
			TokenType:   "Bearer",
			Expiry:      time.Now().Add(time.Hour),
		}), nil
	}
	if cfg.credentialMode() == credentialServiceAccount {
		return serviceAccountTokenSource(ctx, cfg)
	}
	return nil, fmt.Errorf("no usable Google Play credentials — set PLAY_SERVICE_ACCOUNT_FILE to a service-account key granted access in Play Console → Users & permissions, or run `rollout login play`")
}

// serviceAccountTokenSource mints access tokens from a service-account key by
// the JWT bearer flow. The token source caches and refreshes on its own, so it
// is built once per client.
func serviceAccountTokenSource(ctx context.Context, cfg *PlayConfig) (oauth2.TokenSource, error) {
	key, err := cfg.readServiceAccountKey()
	if err != nil {
		return nil, err
	}
	jwtCfg, err := google.JWTConfigFromJSON(key.raw, cfg.scopes()...)
	if err != nil {
		return nil, fmt.Errorf("build a token source from the service-account key (%s): %w", key.ClientEmail, err)
	}
	return jwtCfg.TokenSource(ctx), nil
}

// newPlayHTTPClient returns an http.Client that authenticates every request
// with the configured credentials.
func newPlayHTTPClient(ctx context.Context, cfg *PlayConfig) (*http.Client, error) {
	ts, err := tokenSource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	client := oauth2.NewClient(ctx, ts)
	// A publishing call that hangs is worse than one that fails: a CLI run from
	// CI would sit there until the job times out. Uploads set their own,
	// longer, per-request deadline (see upload.go).
	client.Timeout = 60 * time.Second
	return client, nil
}
