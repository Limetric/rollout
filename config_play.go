package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Play API endpoints. Each is overridable so tests can point at an httptest
// server; a loopback override is what puts the config in test mode.
const (
	// defaultPlayBaseURL serves the Android Publisher API v3.
	defaultPlayBaseURL = "https://androidpublisher.googleapis.com"
	// defaultPlayReportingBaseURL serves the Play Developer Reporting API
	// v1beta1 — a separate service with its own scope and its own quota.
	defaultPlayReportingBaseURL = "https://playdeveloperreporting.googleapis.com"
	// defaultPlayStorageBaseURL serves the JSON API for the GCS bucket holding
	// the CSV report exports — a third service, reachable only when a reports
	// bucket is configured.
	defaultPlayStorageBaseURL = "https://storage.googleapis.com"
)

// credentialMode names how rollout authenticates to Google Play.
type credentialMode string

const (
	// credentialServiceAccount is the headless path Google recommends for the
	// Publisher API: a JSON key granted access in Play Console → Users &
	// permissions. It is what CI should use.
	credentialServiceAccount credentialMode = "serviceAccount"
	// credentialOAuthUser is a human signing in with their own Play Console
	// account via `rollout login play`.
	credentialOAuthUser credentialMode = "oauthUser"
	// credentialNone means nothing usable is configured.
	credentialNone credentialMode = "none"
)

// PlayConfig holds everything needed to talk to Google Play. It is the play
// platform's slice of the shared configuration: the core loader (config.go)
// hands it the `[play]` TOML table, and it overlays its own PLAY_* env vars on
// top.
type PlayConfig struct {
	// ServiceAccountFile is the path to a service-account JSON key.
	ServiceAccountFile string `toml:"service_account_file"`
	// ClientID / ClientSecret are the Desktop-app OAuth credentials used by
	// `rollout login play` for the user sign-in flow.
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	// PackageName is the app every command defaults to, so `--package` can be
	// omitted. Set it with `rollout config play set-package`.
	PackageName string `toml:"package_name"`
	// DeveloperID is the Play Console developer account ID. Only the users and
	// grants tools need it.
	DeveloperID string `toml:"developer_id"`
	// ReportsBucket is the `pubsite_prod_rev_<id>` GCS bucket holding the CSV
	// report exports (installs, ratings, financials). Setting it is what adds
	// the read-only Cloud Storage scope to the credential.
	ReportsBucket string `toml:"reports_bucket"`
	// BaseURL overrides the Android Publisher endpoint; a loopback value puts
	// the config in test mode.
	BaseURL string `toml:"api_base_url"`
	// ReportingBaseURL overrides the Play Developer Reporting endpoint.
	ReportingBaseURL string `toml:"reporting_base_url"`
	// StorageBaseURL overrides the Cloud Storage endpoint the reports bucket
	// is read through.
	StorageBaseURL string `toml:"storage_base_url"`

	// --- unexported bookkeeping ---

	// serviceAccountJSON is an inline service-account key supplied through
	// PLAY_SERVICE_ACCOUNT_JSON. CI systems hand out secrets as values rather
	// than files, and writing one to disk just to read it back is a worse
	// default than accepting it directly. It never comes from TOML: a key
	// pasted into a config file is a key that gets committed.
	serviceAccountJSON string
	// sourcePath is the config file the TOML values came from, empty when no
	// file was found.
	sourcePath string
}

// playConfigTable is the TOML table PlayConfig occupies in the shared file.
const playConfigTable = "play"

// playConfigFile is the shape of the shared config file as far as Play is
// concerned: one `[play]` table, everything else ignored.
type playConfigFile struct {
	Play PlayConfig `toml:"play"`
}

// loadPlayConfig reads Play's configuration from the given file (optional) and
// overlays environment variables on top. An empty path means "use the default
// path if it exists, otherwise env only".
func loadPlayConfig(path string) (*PlayConfig, error) {
	var file playConfigFile
	resolved, err := decodeConfigFile(path, &file)
	if err != nil {
		return nil, err
	}
	cfg := file.Play
	cfg.sourcePath = resolved
	cfg.finalize()
	return &cfg, nil
}

// finalize overlays environment variables on top of any file values and applies
// defaults and normalization.
func (c *PlayConfig) finalize() {
	overlayEnv(map[string]*string{
		"PLAY_SERVICE_ACCOUNT_FILE": &c.ServiceAccountFile,
		"PLAY_CLIENT_ID":            &c.ClientID,
		"PLAY_CLIENT_SECRET":        &c.ClientSecret,
		"PLAY_PACKAGE_NAME":         &c.PackageName,
		"PLAY_DEVELOPER_ID":         &c.DeveloperID,
		"PLAY_REPORTS_BUCKET":       &c.ReportsBucket,
		"PLAY_API_BASE_URL":         &c.BaseURL,
		"PLAY_REPORTING_BASE_URL":   &c.ReportingBaseURL,
		"PLAY_STORAGE_BASE_URL":     &c.StorageBaseURL,
	})
	c.serviceAccountJSON = strings.TrimSpace(os.Getenv("PLAY_SERVICE_ACCOUNT_JSON"))
	// GOOGLE_APPLICATION_CREDENTIALS is the Google-wide convention, so a
	// machine already set up for another Google tool works without new
	// configuration. It is only a fallback: PLAY_SERVICE_ACCOUNT_FILE names
	// *this* tool's key, and must win where both are set.
	if c.ServiceAccountFile == "" && c.serviceAccountJSON == "" {
		c.ServiceAccountFile = strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
	}
	c.PackageName = strings.TrimSpace(c.PackageName)
	c.DeveloperID = strings.TrimSpace(c.DeveloperID)
	// The Console shows the bucket as a `gs://…` URI, and that is what lands in
	// a hand-written config or a CI variable. Trim it here rather than only in
	// `config play set-reports-bucket`, so every way of setting it works.
	c.ReportsBucket = trimBucketURI(c.ReportsBucket)
	c.BaseURL = normalizeBaseURL(c.BaseURL, defaultPlayBaseURL)
	c.ReportingBaseURL = normalizeBaseURL(c.ReportingBaseURL, defaultPlayReportingBaseURL)
	c.StorageBaseURL = normalizeBaseURL(c.StorageBaseURL, defaultPlayStorageBaseURL)
}

// credentialMode reports how rollout will authenticate. A service-account key
// wins when both modes are configured: it is the headless path, it never
// prompts, and someone who has set one up meant to use it.
func (c *PlayConfig) credentialMode() credentialMode {
	switch {
	case c.ServiceAccountFile != "" || c.serviceAccountJSON != "":
		return credentialServiceAccount
	case c.ClientID != "":
		return credentialOAuthUser
	default:
		return credentialNone
	}
}

// serviceAccountKey is the subset of a Google service-account JSON key rollout
// reads: enough to authenticate and to tell the user which account to grant
// access to in the Play Console.
type serviceAccountKey struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	// raw is the key bytes as read, which is what the JWT config is built from.
	raw []byte
}

// readServiceAccountKey loads and validates the configured service-account key,
// from the inline JSON when given and otherwise from the key file.
//
// The parse is strict about the two fields that matter, because the common
// failure is pointing at the wrong JSON: an OAuth client secrets file
// downloaded from the same Cloud Console page looks similar and has neither.
func (c *PlayConfig) readServiceAccountKey() (*serviceAccountKey, error) {
	var data []byte
	var origin string
	switch {
	case c.serviceAccountJSON != "":
		data, origin = []byte(c.serviceAccountJSON), "PLAY_SERVICE_ACCOUNT_JSON"
	case c.ServiceAccountFile != "":
		b, err := os.ReadFile(c.ServiceAccountFile)
		if err != nil {
			return nil, fmt.Errorf("read service-account key %q: %w", c.ServiceAccountFile, err)
		}
		data, origin = b, c.ServiceAccountFile
	default:
		return nil, fmt.Errorf("no service-account key configured — set PLAY_SERVICE_ACCOUNT_FILE or run `rollout login play`")
	}

	var key serviceAccountKey
	if err := json.Unmarshal(data, &key); err != nil {
		return nil, fmt.Errorf("parse service-account key from %s: %w", origin, err)
	}
	if key.ClientEmail == "" || key.PrivateKey == "" {
		return nil, fmt.Errorf("%s is not a service-account key (no client_email/private_key) — download the key from Cloud Console → IAM & Admin → Service Accounts → Keys, not the OAuth client secrets file", origin)
	}
	key.raw = data
	return &key, nil
}

// validate reports whether the config is usable for real API calls. It is
// intentionally lenient in test mode, where there is nothing to authenticate to.
func (c *PlayConfig) validate() error {
	if c.isTest() {
		return nil
	}
	switch c.credentialMode() {
	case credentialNone:
		return fmt.Errorf("no Google Play credentials — set PLAY_SERVICE_ACCOUNT_FILE (a service-account key granted access in Play Console → Users & permissions) or run `rollout login play`")
	case credentialOAuthUser:
		if c.ClientSecret == "" {
			return fmt.Errorf("PLAY_CLIENT_ID is set without PLAY_CLIENT_SECRET — both halves of the OAuth client are needed; re-run `rollout login play`")
		}
	}
	return nil
}

// resolvePackage picks the app a command acts on: an explicit argument first,
// then the configured default.
func (c *PlayConfig) resolvePackage(arg string) (string, error) {
	if pkg := strings.TrimSpace(arg); pkg != "" {
		return pkg, nil
	}
	if c.PackageName != "" {
		return c.PackageName, nil
	}
	return "", fmt.Errorf("no package name — pass --package com.example.app, set PLAY_PACKAGE_NAME, or run `rollout config play set-package com.example.app`")
}

// resolveDeveloperID picks the developer account a users-and-permissions
// command acts on: an explicit argument first, then the configured value.
//
// It is resolved on demand rather than required by validate(), because it is
// the one setting nothing else needs — every other surface is addressed by
// package name, and a user who only ships releases should never be asked for a
// developer ID they would have to go and look up.
func (c *PlayConfig) resolveDeveloperID(arg string) (string, error) {
	id := strings.TrimSpace(arg)
	if id == "" {
		id = c.DeveloperID
	}
	if id == "" {
		return "", fmt.Errorf("no developer account id — it is the number in the Play Console URL (…/developers/1234567890/…); pass --developer-id, set PLAY_DEVELOPER_ID, or run `rollout config play set-developer-id 1234567890`")
	}
	if !validDeveloperID(id) {
		return "", fmt.Errorf("developer id %q is not the all-digit id from the Play Console URL (…/developers/1234567890/…)", id)
	}
	return id, nil
}

// isTest reports whether we're pointed at a local/offline base URL, in which
// case auth and credential checks are relaxed.
func (c *PlayConfig) isTest() bool {
	if c.BaseURL == "" || c.BaseURL == defaultPlayBaseURL {
		return false
	}
	return isLoopbackURL(c.BaseURL)
}

// scopes are the OAuth scopes this configuration needs. The Cloud Storage scope
// is requested only when a reports bucket is configured: asking for storage
// access nobody uses makes the consent screen scarier than the tool is.
func (c *PlayConfig) scopes() []string {
	s := []string{
		"https://www.googleapis.com/auth/androidpublisher",
		"https://www.googleapis.com/auth/playdeveloperreporting",
	}
	if c.ReportsBucket != "" {
		s = append(s, "https://www.googleapis.com/auth/devstorage.read_only")
	}
	return s
}

// trimBucketURI reduces a `gs://bucket/path` URI to its bucket name, and leaves
// a bare name alone.
func trimBucketURI(s string) string {
	bucket := strings.Trim(strings.TrimPrefix(strings.TrimSpace(s), "gs://"), "/")
	if i := strings.Index(bucket, "/"); i >= 0 {
		bucket = bucket[:i]
	}
	return bucket
}

// playConfigured reports whether anything Play-specific has been set up: a
// credential, a default app, or a developer ID. Any one of them means the user
// intends to ship to Play and wants `rollout doctor` to say what is missing;
// none of them means they haven't started, and a plain `rollout doctor` should
// not report that as a broken setup.
func playConfigured() bool {
	cfg, err := loadPlayConfig(configPath)
	if err != nil {
		// A config file that cannot be read is a problem doctor should report,
		// not one it should skip over.
		return true
	}
	if cfg.credentialMode() != credentialNone || cfg.PackageName != "" || cfg.DeveloperID != "" {
		return true
	}
	// A saved sign-in counts too: `rollout login play` leaves the refresh token
	// in the store and the client in the config, but a user who then moved
	// their config file still has a setup doctor should report on.
	tok, err := readStoredToken(playTokenPolicy.Platform)
	return err == nil && tok != nil
}
