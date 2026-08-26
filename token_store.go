package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// Refresh tokens live in a per-platform token store — one 0600 JSON file per
// platform under stateDir()/tokens — and never in config.toml or an
// environment variable.
//
// The reason is rotation. Google's refresh token is static, so one pasted into
// an env var would work forever. Other providers (Microsoft, and therefore App
// Store Connect's neighbours) mint a *new* refresh token on every refresh and
// invalidate the old one, and a process cannot write back to its own
// environment: the first run would refresh to RT2, the next would present the
// dead RT1, and sign-in would fail. Rather than special-case rotation later,
// every platform reads and writes the same store from the start, so there is
// one auth path to maintain when a second one arrives.

// tokenStoreEnv points rollout at a different store directory. Containers and
// CI mount a writable volume here; it is also how two configs that sign in as
// different users keep separate tokens.
const tokenStoreEnv = "ROLLOUT_TOKEN_STORE"

// storedToken is one platform's persisted OAuth grant. Only the refresh token
// is kept: access tokens are short-lived and cheap to re-mint, and not writing
// one on every refresh keeps the common path read-only.
type storedToken struct {
	// RefreshToken is the long-lived grant access tokens are minted from.
	RefreshToken string `json:"refresh_token"`
	// UpdatedAt is when this value was written — the token age `doctor` reports.
	UpdatedAt time.Time `json:"updated_at"`
	// Source records where the value came from (`rollout login play`, or a
	// token refresh), so a user who finds an unexpected token can tell how it
	// got there.
	Source string `json:"source,omitempty"`
	// ClientID is the OAuth client this grant was issued to. A refresh token is
	// only usable with the client that minted it, so recording it lets a
	// mismatch be named instead of surfacing as a bare invalid_grant — see
	// checkClientBinding.
	ClientID string `json:"client_id,omitempty"`
}

// tokenPolicy is what the shared auth path needs to know about one platform's
// refresh token. A platform supplies one (see playTokenPolicy) instead of this
// file knowing which platforms exist.
type tokenPolicy struct {
	// Platform is the namespace name, and the store slot's filename.
	Platform string
	// Rotates is true when the platform issues a new refresh token on every
	// refresh and invalidates the old one. It decides how loudly a store we
	// cannot write fails: for a static token an unwritable store costs nothing,
	// but a rotating platform loses its only valid credential.
	Rotates bool
}

// loginCommand is the command that re-authorizes this platform, quoted into
// error messages so every auth failure ends with the fix.
func (p tokenPolicy) loginCommand() string { return "rollout login " + p.Platform }

// checkClientBinding warns when the saved sign-in belongs to a different OAuth
// client than the one now configured, which makes the pair unusable.
//
// Two setups land here. One config file's sign-in reached the store and another
// config, pointing at a different OAuth client, then reads it — the store has a
// single slot per platform, so alternating `--config` profiles share it unless
// they also set ROLLOUT_TOKEN_STORE. Or a sign-in half-committed: the client
// credentials were written and the token was not.
//
// It warns rather than fails: the store is still the best token available, and
// the request that follows will say what Google thinks. The point is that the
// user reads this first.
func checkClientBinding(policy tokenPolicy, stored *storedToken, configuredClientID string) {
	if stored == nil || stored.ClientID == "" || configuredClientID == "" || stored.ClientID == configuredClientID {
		return
	}
	warnOnce("the saved %s sign-in was issued to OAuth client %s, but %s is configured — a refresh token only works with the client that minted it. Run `%s` to sign in with the configured client, or point %s at a store of its own.",
		policy.Platform, stored.ClientID, configuredClientID, policy.loginCommand(), tokenStoreEnv)
}

// --- store paths and files ---

// validPlatformName reports whether name is safe to use as a store filename.
// Platform names are compile-time constants, but the store path is the one
// place a name reaches the filesystem, so it is checked rather than trusted.
func validPlatformName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// tokenStoreDir resolves the store directory without creating it.
func tokenStoreDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv(tokenStoreEnv)); v != "" {
		return v, nil
	}
	state, err := stateDirPath()
	if err != nil {
		return "", fmt.Errorf("no usable config directory (%v) — set HOME/XDG_CONFIG_HOME, or point %s at a writable directory", err, tokenStoreEnv)
	}
	return filepath.Join(state, "tokens"), nil
}

// tokenStorePath returns the file holding one platform's token.
func tokenStorePath(platform string) (string, error) {
	if !validPlatformName(platform) {
		return "", fmt.Errorf("invalid platform name %q", platform)
	}
	dir, err := tokenStoreDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, platform+".json"), nil
}

// readStoredToken loads a platform's saved token. A store file that does not
// exist yet is not an error — it means nobody has signed in — and neither is
// one holding an empty token; both return (nil, nil).
func readStoredToken(platform string) (*storedToken, error) {
	path, err := tokenStorePath(platform)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read token store %q: %w", path, err)
	}
	var t storedToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("corrupt token store %q: %w — delete the file and run `rollout login %s`", path, err, platform)
	}
	if t.RefreshToken == "" {
		return nil, nil
	}
	return &t, nil
}

// writeStoredToken persists a platform's token at 0600 in a 0700 directory.
// Its errors always name the path: a store that cannot be written is a
// filesystem problem, and reporting it as one is the difference between a
// one-line fix and an auth failure nobody can explain.
func writeStoredToken(platform string, t *storedToken) error {
	path, err := tokenStorePath(platform)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token store directory %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token store %q: %w", path, err)
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write token store %q: %w", path, err)
	}
	return nil
}

// requireWritableStore reports why a platform's store cannot be written, or nil.
//
// A sign-in has to know before it acts rather than after: it must not overwrite
// the client half of a credential pair whose token half it cannot then commit.
// A rotating platform has the same need at refresh time — the provider kills
// the old refresh token the moment the new one is issued, so discovering the
// problem afterwards means the sign-in is already gone.
func requireWritableStore(platform string) error {
	path, err := tokenStorePath(platform)
	if err != nil {
		return err
	}
	// The write follows symlinks, so the probe has to as well — otherwise a
	// store file linked into an unwritable directory passes here and fails
	// later, which for a rotating platform is after the token is already spent.
	if err := probeWritable(filepath.Dir(resolveWritePath(path))); err != nil {
		return fmt.Errorf("token store %q is not writable: %w", path, err)
	}
	return nil
}

// --- writing rotated tokens back ---

// persistingTokenSource wraps a platform's oauth2.TokenSource and saves the
// refresh token whenever the provider hands back a new one. This is what makes
// a rotating platform survive across separate process invocations: oauth2
// caches the access token in memory, but a rotated refresh token only lives as
// long as the process unless it reaches the store.
//
// Google never trips this — its refresh responses carry no refresh_token and
// oauth2 carries the old one forward unchanged, so the common path never
// writes. Keeping the write-back anyway is what makes the store platform-neutral
// rather than something the next platform has to rebuild.
type persistingTokenSource struct {
	policy   tokenPolicy
	src      oauth2.TokenSource
	clientID string // carried through rotations so the binding is not lost

	mu      sync.Mutex
	current string // the refresh token already known to be in the store
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, p.policy.authError(err)
	}
	if tok.RefreshToken == "" {
		return tok, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.RefreshToken == p.current {
		return tok, nil
	}
	err = writeStoredToken(p.policy.Platform, &storedToken{
		RefreshToken: tok.RefreshToken,
		UpdatedAt:    time.Now().UTC(),
		Source:       "token refresh",
		ClientID:     p.clientID,
	})
	if err != nil {
		// The token we were handed replaces one that may already be dead, so
		// dropping it silently would break the next run with an auth error that
		// points nowhere. Fail here instead, while the path is still in hand.
		return nil, fmt.Errorf("could not save the refreshed %s sign-in: %w — the next run would fail to sign in; make that path writable, or set %s to a writable directory",
			p.policy.Platform, err, tokenStoreEnv)
	}
	p.current = tok.RefreshToken
	return tok, nil
}

// authError makes a token-endpoint rejection actionable. invalid_grant means
// the refresh token was revoked, expired, or (on a rotating platform) already
// replaced — none of which the user can fix by retrying, and all of which are
// fixed by signing in again.
//
// The original error is wrapped, so doctor's classification still sees the
// *oauth2.RetrieveError underneath and reports a broken setup rather than a
// transient one.
func (p tokenPolicy) authError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
		return fmt.Errorf("%s sign-in is no longer valid (invalid_grant — the refresh token was revoked, expired, or replaced): run `%s` to sign in again: %w",
			p.Platform, p.loginCommand(), err)
	}
	return err
}

// --- diagnostics ---

// tokenStoreStatus is what `doctor` and `config show` report about a platform's
// store slot: where it is, whether it can be written, and how old the saved
// sign-in is.
type tokenStoreStatus struct {
	Path     string
	PathErr  error
	WriteErr error
	Token    *storedToken
	ReadErr  error
}

// describeTokenStore inspects a platform's store slot. It never fails: every
// problem it finds is part of the report.
func describeTokenStore(platform string) tokenStoreStatus {
	st := tokenStoreStatus{}
	path, err := tokenStorePath(platform)
	if err != nil {
		st.PathErr = err
		return st
	}
	st.Path = path
	st.Token, st.ReadErr = readStoredToken(platform)
	// Resolved the same way the write resolves it, or the report would call a
	// store writable that the next sign-in cannot update.
	st.WriteErr = probeWritable(filepath.Dir(resolveWritePath(path)))
	return st
}

// probeWritable reports whether a store directory can actually be written.
// Permission bits alone don't answer this — read-only mounts, ACLs, and
// container user mappings all lie — so it creates a file and removes it.
//
// It does not create the directory. A directory that does not exist yet is
// writable exactly when the nearest existing ancestor is, and `config show`
// must be able to ask the question without answering it: creating the tree
// would turn a mistyped ROLLOUT_TOKEN_STORE into a real, empty, perfectly
// writable store and report the setup as fine.
func probeWritable(dir string) error {
	existing := dir
	for {
		if fi, err := os.Stat(existing); err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", existing)
			}
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return fmt.Errorf("no existing parent directory for %s", dir)
		}
		existing = parent
	}
	f, err := os.CreateTemp(existing, ".writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// location renders where the store is, and why it isn't usable when it isn't.
func (s tokenStoreStatus) location() string {
	switch {
	case s.PathErr != nil:
		return fmt.Sprintf("unavailable — %v", s.PathErr)
	case s.WriteErr != nil:
		return fmt.Sprintf("%s (NOT WRITABLE: %v)", s.Path, s.WriteErr)
	default:
		return s.Path
	}
}

// describe renders the saved sign-in for a human: its age and where it came
// from, never the token itself.
func (s tokenStoreStatus) describe(policy tokenPolicy) string {
	switch {
	case s.ReadErr != nil:
		return fmt.Sprintf("unreadable — %v", s.ReadErr)
	case s.Token == nil:
		return fmt.Sprintf("none saved — run `%s`", policy.loginCommand())
	case s.Token.Source != "":
		return fmt.Sprintf("saved %s via %s", humanizeAge(time.Since(s.Token.UpdatedAt)), s.Token.Source)
	default:
		return "saved " + humanizeAge(time.Since(s.Token.UpdatedAt))
	}
}

// humanizeAge renders how long ago something was written, at the coarsest unit
// that still says something useful.
func humanizeAge(d time.Duration) string {
	switch {
	case d < 0:
		return "in the future (check the system clock)"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// --- notices ---

// warnWriter receives degradation and setup notices. They go to stderr so they
// can never corrupt stdout, which carries JSON for jq pipelines on the CLI and
// the MCP protocol itself under `rollout mcp`.
var warnWriter io.Writer = os.Stderr

var (
	warnedMu sync.Mutex
	warned   = map[string]bool{}
)

// warnOnce prints a notice the first time it is produced in a process. A
// warning the user has already been told about on this run adds nothing, and
// these paths run on every tool call under an MCP host.
func warnOnce(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	warnedMu.Lock()
	if warned[msg] {
		warnedMu.Unlock()
		return
	}
	warned[msg] = true
	w := warnWriter
	warnedMu.Unlock()
	fmt.Fprintln(w, "warning: "+msg)
}
