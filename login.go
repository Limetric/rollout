package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	"golang.org/x/term"
)

// `rollout login play` sets up credentials one of two ways:
//
//   - `--service-account <file>` validates a service-account JSON key and
//     records its path. This is the headless path, and what CI should use.
//   - the default interactive wizard runs Google's loopback authorization-code
//     flow with PKCE, saves the refresh token in the token store, and writes the
//     OAuth client into config.toml.
//
// Both paths end by running the same live doctor probe, so "signed in" means
// "the API answered", not "a file was written".

// loginCallbackTimeout bounds how long the loopback server waits for Google's
// redirect. First-time consent involves an account picker and often 2FA, so
// this is deliberately generous.
const loginCallbackTimeout = 5 * time.Minute

// loopbackRedirectURL builds the OAuth redirect URI for the loopback flow.
// Google's guidance is the literal IP http://127.0.0.1:<port> — "localhost"
// can resolve to ::1 while the listener binds the IPv4 loopback, in which case
// the redirect never arrives.
func loopbackRedirectURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// clientCreds is the OAuth client identity used to mint a refresh token. kind
// is "installed" (Desktop app), "web", or "config" (taken from env/TOML).
type clientCreds struct {
	clientID     string
	clientSecret string
	kind         string
}

type oauthClientBlock struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// parseCredentialsJSON reads a Google Cloud OAuth client JSON, accepting a
// Desktop-app ("installed") or Web ("web") client.
//
// A service-account key landing here is the mistake worth naming: both files
// come off the same Cloud Console page, and "unrecognized format" would send
// the user back to download the same wrong one.
func parseCredentialsJSON(data []byte) (clientCreds, error) {
	var doc struct {
		Installed *oauthClientBlock `json:"installed"`
		Web       *oauthClientBlock `json:"web"`
		Type      string            `json:"type"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return clientCreds{}, fmt.Errorf("parse credentials JSON: %w", err)
	}
	switch {
	case doc.Installed != nil:
		return clientCreds{clientID: doc.Installed.ClientID, clientSecret: doc.Installed.ClientSecret, kind: "installed"}, nil
	case doc.Web != nil:
		return clientCreds{clientID: doc.Web.ClientID, clientSecret: doc.Web.ClientSecret, kind: "web"}, nil
	case doc.Type == "service_account":
		return clientCreds{}, errors.New("this is a service-account key, not an OAuth client — pass it with `rollout login play --service-account <file>` instead")
	default:
		return clientCreds{}, errors.New("unrecognized credentials format — expected a Desktop-app OAuth client (an \"installed\" block). Download it from Google Cloud Console → APIs & Services → Credentials → OAuth 2.0 Client ID → Desktop app")
	}
}

// resolveLoginCreds picks the client credentials: an explicit --credentials
// file wins; otherwise fall back to the already-resolved env/TOML config.
func resolveLoginCreds(cfg *PlayConfig, credentialsPath string) (clientCreds, error) {
	if credentialsPath != "" {
		data, err := os.ReadFile(credentialsPath)
		if err != nil {
			return clientCreds{}, fmt.Errorf("read credentials file %q: %w", credentialsPath, err)
		}
		creds, err := parseCredentialsJSON(data)
		if err != nil {
			return clientCreds{}, fmt.Errorf("credentials file %q: %w", credentialsPath, err)
		}
		if creds.clientID == "" || creds.clientSecret == "" {
			return clientCreds{}, fmt.Errorf("credentials file %q is missing client_id/client_secret", credentialsPath)
		}
		return creds, nil
	}
	if cfg.ClientID != "" && cfg.ClientSecret != "" {
		return clientCreds{clientID: cfg.ClientID, clientSecret: cfg.ClientSecret, kind: "config"}, nil
	}
	return clientCreds{}, errors.New("no OAuth client credentials found — pass --credentials <desktop-app.json>, or set PLAY_CLIENT_ID and PLAY_CLIENT_SECRET. Create a Desktop-app client at Google Cloud Console → APIs & Services → Credentials → OAuth 2.0 Client ID → Desktop app")
}

// savePlayCredentials persists what a sign-in produced: the OAuth client
// id/secret into the `[play]` table of the TOML config, and the refresh token
// into the token store.
//
// They are split because they are different kinds of thing. The client identity
// is stable configuration; the refresh token is a live credential that other
// providers replace on every refresh, so it belongs somewhere rollout owns and
// can rewrite.
//
// A failed sign-in must not destroy a working one. A refresh token is only
// usable alongside the OAuth client that minted it, so committing one half
// without the other would replace a working pair with a broken one. The two
// writes therefore land together or not at all: the store is checked up front,
// and the config is rolled back if the token write still fails.
func savePlayCredentials(path string, c clientCreds, refreshToken string) error {
	if err := requireWritableStore(playTokenPolicy.Platform); err != nil {
		return fmt.Errorf("signed in, but the refresh token cannot be saved: %w — make that path writable, or set %s to a writable directory and sign in again", err, tokenStoreEnv)
	}
	restoreConfig, err := snapshotFile(path)
	if err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	if err := upsertConfigKey(path, playConfigTable, "client_id", c.clientID); err != nil {
		return err
	}
	if err := upsertConfigKey(path, playConfigTable, "client_secret", c.clientSecret); err != nil {
		if rerr := restoreConfig(); rerr != nil {
			return fmt.Errorf("%w (and %q could not be rolled back: %v)", err, path, rerr)
		}
		return err
	}
	if err := writeStoredToken(playTokenPolicy.Platform, &storedToken{
		RefreshToken: refreshToken,
		UpdatedAt:    time.Now().UTC(),
		Source:       playTokenPolicy.loginCommand(),
		ClientID:     c.clientID,
	}); err != nil {
		// The browser dance already succeeded, so say what was lost and why:
		// without this, the sign-in has to be repeated for a reason that has
		// nothing to do with signing in.
		saved := fmt.Errorf("signed in, but the refresh token could not be saved: %w — make that path writable, or set %s to a writable directory and sign in again", err, tokenStoreEnv)
		if rerr := restoreConfig(); rerr != nil {
			return fmt.Errorf("%w (and %q could not be rolled back to its previous contents: %v — its client_id/client_secret no longer match the saved refresh token)", saved, path, rerr)
		}
		return saved
	}
	return nil
}

// saveServiceAccountPath records a validated key file in the config. The key
// itself is never copied: leaving it where the user put it means there is one
// copy of the private key on the machine, in a place they chose.
func saveServiceAccountPath(path, keyPath string) error {
	abs, err := filepath.Abs(keyPath)
	if err != nil {
		abs = keyPath
	}
	return upsertConfigKey(path, playConfigTable, "service_account_file", abs)
}

// --- the loopback OAuth server ---

// runLoopbackOAuth opens the browser to Google's consent screen and captures
// the authorization code on a loopback HTTP server. conf.RedirectURL and ln
// must agree on the port. It returns once the callback arrives, errors, or
// times out.
func runLoopbackOAuth(ctx context.Context, conf *oauth2.Config, openFn func(string) error, ln net.Listener, opts ...oauth2.AuthCodeOption) (string, error) {
	state, err := randomState()
	if err != nil {
		return "", err
	}
	authURL := conf.AuthCodeURL(state, opts...)

	type result struct {
		code string
		err  error
	}
	resultCh := make(chan result, 1)
	// send delivers the first result and drops any later ones, so a stray
	// second callback (a browser retry, a favicon hitting the catch-all) can't
	// block its handler goroutine forever on a full channel.
	send := func(r result) {
		select {
		case resultCh <- r:
		default:
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// A request carrying none of the OAuth callback parameters is not the
		// callback — a browser preconnect, a favicon fetch, a port scanner, or
		// the user opening the bare URL. Treating those as failed callbacks
		// would abort the login before the real redirect arrives.
		if q.Get("error") == "" && q.Get("state") == "" && q.Get("code") == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch {
		case q.Get("error") != "":
			msg := q.Get("error") + ": " + q.Get("error_description")
			writeCallbackPage(w, false, msg)
			send(result{err: fmt.Errorf("authorization failed: %s", msg)})
		case q.Get("state") != state:
			writeCallbackPage(w, false, "state mismatch")
			send(result{err: errors.New("state parameter mismatch — aborting (possible CSRF)")})
		case q.Get("code") == "":
			writeCallbackPage(w, false, "no authorization code in callback")
			send(result{err: errors.New("no authorization code in callback")})
		default:
			writeCallbackPage(w, true, "")
			send(result{code: q.Get("code")})
		}
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	// Graceful shutdown lets the in-flight callback response (the
	// "Authorization successful" page) flush before we tear down. Shutdown also
	// closes ln, so the listener is still closed exactly once before returning.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := openFn(authURL); err != nil {
		return "", err
	}

	timer := time.NewTimer(loginCallbackTimeout)
	defer timer.Stop()
	select {
	case res := <-resultCh:
		return res.code, res.err
	case <-timer.C:
		return "", fmt.Errorf("no authorization received within %s — did you approve in the browser?", loginCallbackTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// signInLoopback runs the whole browser dance and returns the refresh token:
// listen, open the consent screen with PKCE, capture the code, exchange it.
func signInLoopback(ctx context.Context, cfg *PlayConfig, creds clientCreds, openFn func(string) error, port int) (string, error) {
	conf := playOAuthConfig(cfg, port)
	conf.ClientID, conf.ClientSecret = creds.clientID, creds.clientSecret

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("listen on %s: %w — is the port busy? pass --port", addr, err)
	}

	// PKCE. Google does not require it for a Desktop client with a secret, but
	// the secret in a Desktop client is not secret, and the verifier is what
	// actually binds the code to this process.
	verifier := oauth2.GenerateVerifier()
	code, err := runLoopbackOAuth(ctx, conf, openFn, ln,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.S256ChallengeOption(verifier),
	)
	if err != nil {
		return "", err
	}
	return exchangeRefreshToken(ctx, conf, code, oauth2.VerifierOption(verifier))
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func writeCallbackPage(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html")
	if ok {
		_, _ = io.WriteString(w, "<h1>Authorization successful</h1><p>You can close this tab and return to the terminal.</p>")
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, "<h1>Authorization failed</h1><p>"+html.EscapeString(msg)+"</p>")
}

// exchangeRefreshToken trades an authorization code for tokens and returns the
// refresh token. A missing refresh token almost always means a misconfigured
// OAuth client, so the error spells out the usual causes.
func exchangeRefreshToken(ctx context.Context, conf *oauth2.Config, code string, opts ...oauth2.AuthCodeOption) (string, error) {
	tok, err := conf.Exchange(ctx, code, opts...)
	if err != nil {
		return "", fmt.Errorf("exchange authorization code: %w", err)
	}
	if tok.RefreshToken == "" {
		return "", errors.New("no refresh_token in response — common causes: the wrong OAuth client type (a Desktop app is needed, not a Web application), the loopback redirect URI is not authorized, or the Google Play Android Developer API is not enabled in the project")
	}
	return tok.RefreshToken, nil
}

func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	return exec.Command(name, args...).Start()
}

// loadLoginConfig loads Play configuration for `login`. Unlike loadPlayConfig
// it tolerates an explicit --config path that does not exist yet: that file is
// the one login will create, so a missing target means "load env only".
func loadLoginConfig(path string) (*PlayConfig, error) {
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				cfg := &PlayConfig{}
				cfg.finalize()
				return cfg, nil
			}
			return nil, fmt.Errorf("stat config %q: %w", path, err)
		}
	}
	return loadPlayConfig(path)
}

// --- CLI front-end ---

var (
	loginCredentialsPath string
	loginServiceAccount  string
	loginPort            int
	loginNoBrowser       bool
	loginNoInput         bool
)

// isInteractiveLogin reports whether `rollout login play` should run the guided
// wizard: stdin is a real terminal, --no-input was not passed, and neither
// non-interactive shortcut was used.
func isInteractiveLogin() bool {
	if loginNoInput || loginCredentialsPath != "" || loginServiceAccount != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

var playLoginCmd = &cobra.Command{
	Use:   "play",
	Short: "Sign in to Google Play and save credentials",
	Long: "Set up Google Play credentials one of two ways.\n\n" +
		"  rollout login play --service-account key.json\n" +
		"      Validate a service-account JSON key and record its path. This is the\n" +
		"      headless path Google recommends for the Publisher API, and what CI\n" +
		"      should use. The key must still be invited in Play Console →\n" +
		"      Users & permissions.\n\n" +
		"  rollout login play\n" +
		"      Run Google's loopback OAuth flow with your own Play Console account:\n" +
		"      it opens your browser, captures the authorization code on localhost,\n" +
		"      exchanges it for a refresh token, and saves it in the token store.\n\n" +
		"Either way, the sign-in ends with a live check against the API.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		cfg, err := loadLoginConfig(configPath)
		if err != nil {
			return err
		}
		target, err := writableConfigPath(configPath)
		if err != nil {
			return err
		}

		if loginServiceAccount != "" {
			return runServiceAccountLogin(cmd.Context(), out, cfg, target, loginServiceAccount)
		}
		if isInteractiveLogin() {
			p := newTTYPrompter(cmd.InOrStdin(), out, int(os.Stdin.Fd()))
			return runLoginWizard(cmd.Context(), out, p, cfg, target, openBrowserOrPrint(out), loginPort)
		}
		return runNonInteractiveLogin(cmd.Context(), out, cfg, target)
	},
}

// runServiceAccountLogin validates a key file and records its path. Nothing is
// copied and nothing is exchanged — the whole "sign-in" is proving the key
// parses and that the API accepts it.
func runServiceAccountLogin(ctx context.Context, out io.Writer, cfg *PlayConfig, target, keyPath string) error {
	cfg.ServiceAccountFile = expandHome(keyPath)
	cfg.ClientID, cfg.ClientSecret = "", "" // this run authenticates as the service account
	key, err := cfg.readServiceAccountKey()
	if err != nil {
		return err
	}
	if err := saveServiceAccountPath(target, cfg.ServiceAccountFile); err != nil {
		return err
	}
	s := newStyles(out)
	fmt.Fprintf(out, "%s Service account %s recorded in %s\n", s.markOK(), s.accent(key.ClientEmail), s.accent(target))
	fmt.Fprintf(out, "\n%s\n\n", s.muted(fmt.Sprintf("If the check below fails with a permission error, invite\n  %s\nin Play Console → Users & permissions and grant it access to your app.", key.ClientEmail)))
	return verifyLogin(ctx, out, cfg)
}

// runNonInteractiveLogin is the OAuth flow without prompts: everything has to
// come from flags or the environment, and a missing piece is an error rather
// than a question.
func runNonInteractiveLogin(ctx context.Context, out io.Writer, cfg *PlayConfig, target string) error {
	creds, err := resolveLoginCreds(cfg, loginCredentialsPath)
	if err != nil {
		return err
	}
	s := newStyles(out)
	if creds.kind == "web" {
		fmt.Fprintln(out, s.warning("Warning: this is a Web-application client; loopback sign-in expects a Desktop-app client. Trying anyway."))
	}
	fmt.Fprintf(out, "Waiting for callback on %s …\n", s.url(loopbackRedirectURL(loginPort)))
	refreshToken, err := signInLoopback(ctx, cfg, creds, openBrowserOrPrint(out), loginPort)
	if err != nil {
		return err
	}
	if err := savePlayCredentials(target, creds, refreshToken); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s Signed in. Credentials written to %s\n", s.markOK(), s.accent(target))
	cfg.ClientID, cfg.ClientSecret = creds.clientID, creds.clientSecret
	cfg.ServiceAccountFile, cfg.serviceAccountJSON = "", ""
	return verifyLogin(ctx, out, cfg)
}

// verifyLogin runs the same live probe `rollout doctor play` does, so a
// successful login means the API answered rather than that a file was written.
func verifyLogin(ctx context.Context, out io.Writer, cfg *PlayConfig) error {
	s := newStyles(out)
	if cfg.PackageName == "" {
		fmt.Fprintf(out, "%s — set a default app with `rollout config play set-package com.example.app`, then try `rollout play tracks`.\n", s.success("READY"))
		return nil
	}
	res, err := runPlayDoctorLive(ctx, out, cfg)
	if err != nil {
		return err
	}
	if res != liveOK {
		return fmt.Errorf("signed in, but the live check did not pass — run `rollout doctor play` for the full report")
	}
	fmt.Fprintf(out, "\n%s — try `rollout play tracks`.\n", s.success("READY"))
	return nil
}

// openBrowserOrPrint opens the consent URL, falling back to printing it. A
// missing browser opener (a headless Linux box without xdg-open) must not abort
// the login.
func openBrowserOrPrint(out io.Writer) func(string) error {
	s := newStyles(out)
	if loginNoBrowser {
		return func(u string) error {
			fmt.Fprintf(out, "Open this URL in your browser:\n  %s\n", s.url(u))
			return nil
		}
	}
	return func(u string) error {
		if err := openBrowser(u); err != nil {
			fmt.Fprintf(out, "%s\nOpen this URL manually:\n  %s\n", s.warning(fmt.Sprintf("Could not open a browser (%v).", err)), s.url(u))
		}
		return nil
	}
}

func init() {
	playLoginCmd.Flags().StringVar(&loginServiceAccount, "service-account", "", "path to a service-account JSON key (the headless path; nothing is copied)")
	playLoginCmd.Flags().StringVar(&loginCredentialsPath, "credentials", "", "path to a Desktop-app OAuth client JSON downloaded from Google Cloud Console")
	playLoginCmd.Flags().IntVar(&loginPort, "port", 8085, "loopback port for the OAuth callback")
	playLoginCmd.Flags().BoolVar(&loginNoBrowser, "no-browser", false, "print the auth URL instead of opening a browser")
	playLoginCmd.Flags().BoolVar(&loginNoInput, "no-input", false, "never prompt; fail if credentials are missing (for scripts/CI)")
}
