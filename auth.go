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
	switch cfg.credentialMode() {
	case credentialServiceAccount:
		return serviceAccountTokenSource(ctx, cfg)
	case credentialOAuthUser:
		return userTokenSource(ctx, cfg)
	default:
		return nil, fmt.Errorf("no Google Play credentials — set PLAY_SERVICE_ACCOUNT_FILE to a service-account key granted access in Play Console → Users & permissions, or run `rollout login play`")
	}
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

// userTokenSource mints access tokens from a refresh token saved by
// `rollout login play`. The source is wrapped so a rotated refresh token is
// written back to the store — Google's never rotates, but the write-back is
// what keeps the store usable by the next platform rather than something it
// has to rebuild.
func userTokenSource(ctx context.Context, cfg *PlayConfig) (oauth2.TokenSource, error) {
	stored, err := readStoredToken(playTokenPolicy.Platform)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("an OAuth client is configured but nobody has signed in — run `%s`", playTokenPolicy.loginCommand())
	}
	checkClientBinding(playTokenPolicy, stored, cfg.ClientID)

	conf := playOAuthConfig(cfg, 0)
	base := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: stored.RefreshToken})
	return oauth2.ReuseTokenSource(nil, &persistingTokenSource{
		policy:   playTokenPolicy,
		src:      base,
		clientID: cfg.ClientID,
		current:  stored.RefreshToken,
	}), nil
}

// playOAuthConfig builds the OAuth2 config for the user sign-in flow. port is
// the loopback callback port; pass 0 when no redirect is needed (a refresh).
func playOAuthConfig(cfg *PlayConfig, port int) *oauth2.Config {
	conf := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     playOAuthEndpoint,
		Scopes:       cfg.scopes(),
	}
	if port > 0 {
		conf.RedirectURL = loopbackRedirectURL(port)
	}
	return conf
}

// playOAuthEndpoint is the production Google OAuth endpoint. It is a package
// var so tests can point the login and refresh flows at a fake token server —
// the only place Google's token endpoint is named.
var playOAuthEndpoint = google.Endpoint

// playTokenPolicy is Play's slice of the shared token store. Google's refresh
// tokens are long-lived and static — a refresh returns a new access token and
// the same refresh token — so an unwritable store costs nothing here and must
// not break a setup that works today.
var playTokenPolicy = tokenPolicy{Platform: playPlatformName, Rotates: false}
