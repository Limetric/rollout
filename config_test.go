package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig drops a TOML config in a temp dir and returns its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// clearPlayEnv unsets everything that could leak the developer's real setup
// into a test, and points the config and token store at a temp directory so
// nothing reads — or probes — the real ones.
func clearPlayEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(tokenStoreEnv, filepath.Join(dir, "tokens"))
	for _, key := range []string{
		"PLAY_SERVICE_ACCOUNT_FILE", "PLAY_SERVICE_ACCOUNT_JSON", "PLAY_CLIENT_ID",
		"PLAY_CLIENT_SECRET", "PLAY_PACKAGE_NAME", "PLAY_DEVELOPER_ID",
		"PLAY_REPORTS_BUCKET", "PLAY_API_BASE_URL", "PLAY_REPORTING_BASE_URL",
		"PLAY_STORAGE_BASE_URL",
		"GOOGLE_APPLICATION_CREDENTIALS",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadPlayConfigFromFile(t *testing.T) {
	clearPlayEnv(t)
	path := writeConfig(t, `
[play]
service_account_file = "/keys/play.json"
package_name = "com.example.app"
developer_id = "1234567890"
reports_bucket = "pubsite_prod_rev_01234"
`)
	cfg, err := loadPlayConfig(path)
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.ServiceAccountFile != "/keys/play.json" || cfg.PackageName != "com.example.app" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.DeveloperID != "1234567890" || cfg.ReportsBucket != "pubsite_prod_rev_01234" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	// Unset endpoints fall back to production.
	if cfg.BaseURL != defaultPlayBaseURL || cfg.ReportingBaseURL != defaultPlayReportingBaseURL {
		t.Errorf("endpoints did not default: %q / %q", cfg.BaseURL, cfg.ReportingBaseURL)
	}
}

// TestEnvOverridesFile pins the documented resolution order. An MCP host config
// and CI both supply everything through the environment, and a stale config
// file on the same machine must not win.
func TestEnvOverridesFile(t *testing.T) {
	clearPlayEnv(t)
	path := writeConfig(t, `
[play]
package_name = "com.example.fromfile"
service_account_file = "/keys/from-file.json"
`)
	t.Setenv("PLAY_PACKAGE_NAME", "com.example.fromenv")

	cfg, err := loadPlayConfig(path)
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.PackageName != "com.example.fromenv" {
		t.Errorf("package name = %q, want the environment value", cfg.PackageName)
	}
	// Only the overridden key changes; the rest of the file still applies.
	if cfg.ServiceAccountFile != "/keys/from-file.json" {
		t.Errorf("service account file = %q, want the file value", cfg.ServiceAccountFile)
	}
}

func TestMissingConfigFileIsEnvOnly(t *testing.T) {
	clearPlayEnv(t)
	t.Setenv("PLAY_PACKAGE_NAME", "com.example.app")

	cfg, err := loadPlayConfig("")
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.PackageName != "com.example.app" {
		t.Errorf("package name = %q, want the environment value", cfg.PackageName)
	}
	if cfg.sourcePath != "" {
		t.Errorf("no file should have been read, got %q", cfg.sourcePath)
	}
}

func TestUnreadableConfigFileIsAnError(t *testing.T) {
	clearPlayEnv(t)
	path := writeConfig(t, "this is not = valid = toml")
	if _, err := loadPlayConfig(path); err == nil {
		t.Fatal("expected a parse error for malformed TOML")
	}
}

func TestCredentialMode(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want credentialMode
	}{
		{"nothing configured", nil, credentialNone},
		{"service account file", map[string]string{"PLAY_SERVICE_ACCOUNT_FILE": "/keys/play.json"}, credentialServiceAccount},
		{"inline service account json", map[string]string{"PLAY_SERVICE_ACCOUNT_JSON": `{"client_email":"a@b"}`}, credentialServiceAccount},
		{"oauth client", map[string]string{"PLAY_CLIENT_ID": "abc.apps.googleusercontent.com"}, credentialOAuthUser},
		{
			// Documented precedence: the headless credential wins, because
			// someone who set one up meant to use it.
			name: "service account wins over oauth client",
			env: map[string]string{
				"PLAY_SERVICE_ACCOUNT_FILE": "/keys/play.json",
				"PLAY_CLIENT_ID":            "abc.apps.googleusercontent.com",
			},
			want: credentialServiceAccount,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearPlayEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := loadPlayConfig(writeConfig(t, ""))
			if err != nil {
				t.Fatalf("loadPlayConfig: %v", err)
			}
			if got := cfg.credentialMode(); got != tc.want {
				t.Errorf("credentialMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGoogleApplicationCredentialsIsOnlyAFallback: a machine already set up for
// another Google tool should work, but PLAY_SERVICE_ACCOUNT_FILE names *this*
// tool's key and has to win.
func TestGoogleApplicationCredentialsIsOnlyAFallback(t *testing.T) {
	clearPlayEnv(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/keys/adc.json")

	cfg, err := loadPlayConfig(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.ServiceAccountFile != "/keys/adc.json" {
		t.Errorf("ADC should seed the key path, got %q", cfg.ServiceAccountFile)
	}

	t.Setenv("PLAY_SERVICE_ACCOUNT_FILE", "/keys/play.json")
	cfg, err = loadPlayConfig(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.ServiceAccountFile != "/keys/play.json" {
		t.Errorf("PLAY_SERVICE_ACCOUNT_FILE should win, got %q", cfg.ServiceAccountFile)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     PlayConfig
		wantErr string
	}{
		{
			name:    "nothing configured names both fixes",
			cfg:     PlayConfig{},
			wantErr: "PLAY_SERVICE_ACCOUNT_FILE",
		},
		{
			// Half an OAuth client is the classic copy-paste failure, and the
			// generic "no credentials" message would send the user looking in
			// the wrong place.
			name:    "oauth client without a secret",
			cfg:     PlayConfig{ClientID: "abc.apps.googleusercontent.com"},
			wantErr: "PLAY_CLIENT_SECRET",
		},
		{
			name: "service account is enough",
			cfg:  PlayConfig{ServiceAccountFile: "/keys/play.json"},
		},
		{
			name: "complete oauth client is enough",
			cfg:  PlayConfig{ClientID: "abc.apps.googleusercontent.com", ClientSecret: "secret"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.BaseURL = defaultPlayBaseURL
			err := cfg.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}

// TestIsTestModeOnlyForLoopback is the guard against silently swapping in fake
// credentials: only a loopback endpoint is a test server. A regional endpoint,
// a proxy, or a future API version is a real target.
func TestIsTestModeOnlyForLoopback(t *testing.T) {
	tests := []struct {
		baseURL string
		want    bool
	}{
		{"", false},
		{defaultPlayBaseURL, false},
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"http://[::1]:8080", true},
		{"https://androidpublisher.example.com", false},
		{"http://10.0.0.5:8080", false},
		{"https://eu-androidpublisher.googleapis.com", false},
	}
	for _, tc := range tests {
		cfg := PlayConfig{BaseURL: tc.baseURL}
		if got := cfg.isTest(); got != tc.want {
			t.Errorf("isTest() for %q = %v, want %v", tc.baseURL, got, tc.want)
		}
	}
}

func TestScopesFollowConfiguredFeatures(t *testing.T) {
	const storage = "https://www.googleapis.com/auth/devstorage.read_only"

	cfg := PlayConfig{}
	if slicesContains(cfg.scopes(), storage) {
		t.Error("the storage scope must not be requested when no reports bucket is configured")
	}
	cfg.ReportsBucket = "pubsite_prod_rev_01234"
	if !slicesContains(cfg.scopes(), storage) {
		t.Error("a configured reports bucket should add the storage scope")
	}
}

func TestResolvePackage(t *testing.T) {
	cfg := PlayConfig{PackageName: "com.example.default"}
	if got, err := cfg.resolvePackage("com.example.explicit"); err != nil || got != "com.example.explicit" {
		t.Errorf("explicit argument should win: %q, %v", got, err)
	}
	if got, err := cfg.resolvePackage("  "); err != nil || got != "com.example.default" {
		t.Errorf("configured default should apply: %q, %v", got, err)
	}

	empty := PlayConfig{}
	_, err := empty.resolvePackage("")
	if err == nil {
		t.Fatal("expected an error when no package is available")
	}
	// The message has to name every way to fix it, or the user has to go read
	// the docs to answer a one-word question.
	for _, want := range []string{"--package", "PLAY_PACKAGE_NAME", "set-package"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestReadServiceAccountKey(t *testing.T) {
	clearPlayEnv(t)

	t.Run("inline json", func(t *testing.T) {
		cfg := PlayConfig{serviceAccountJSON: `{"type":"service_account","client_email":"bot@p.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n"}`}
		key, err := cfg.readServiceAccountKey()
		if err != nil {
			t.Fatalf("readServiceAccountKey: %v", err)
		}
		if key.ClientEmail != "bot@p.iam.gserviceaccount.com" {
			t.Errorf("client email = %q", key.ClientEmail)
		}
	})

	t.Run("from a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "key.json")
		if err := os.WriteFile(path, []byte(`{"client_email":"bot@p.iam.gserviceaccount.com","private_key":"k"}`), 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
		cfg := PlayConfig{ServiceAccountFile: path}
		if _, err := cfg.readServiceAccountKey(); err != nil {
			t.Fatalf("readServiceAccountKey: %v", err)
		}
	})

	t.Run("missing file names the path", func(t *testing.T) {
		cfg := PlayConfig{ServiceAccountFile: "/nope/key.json"}
		_, err := cfg.readServiceAccountKey()
		if err == nil || !strings.Contains(err.Error(), "/nope/key.json") {
			t.Fatalf("error should name the file: %v", err)
		}
	})

	t.Run("an oauth client secrets file is rejected", func(t *testing.T) {
		// This is the common mistake: both JSONs come from the same Cloud
		// Console page, and the wrong one would otherwise fail much later with
		// an opaque JWT error.
		cfg := PlayConfig{serviceAccountJSON: `{"installed":{"client_id":"abc","client_secret":"s"}}`}
		_, err := cfg.readServiceAccountKey()
		if err == nil || !strings.Contains(err.Error(), "client_email") {
			t.Fatalf("error should say what is missing: %v", err)
		}
	})
}

func TestPlayConfiguredDetectsPartialSetup(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing set up", nil, false},
		{"a credential", map[string]string{"PLAY_SERVICE_ACCOUNT_FILE": "/keys/play.json"}, true},
		// A package name alone is enough: the user has started, and doctor
		// should tell them what is still missing rather than skip them.
		{"only a package name", map[string]string{"PLAY_PACKAGE_NAME": "com.example.app"}, true},
		{"only a developer id", map[string]string{"PLAY_DEVELOPER_ID": "1234567890"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearPlayEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := playConfigured(); got != tc.want {
				t.Errorf("playConfigured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestReportsBucketAcceptsAGsURI: the Console shows the bucket as a `gs://…`
// URI, and that is what lands in a hand-written config or a CI variable. Left
// verbatim it would be sent as the bucket name and every report call would 404.
func TestReportsBucketAcceptsAGsURI(t *testing.T) {
	clearPlayEnv(t)
	t.Setenv("PLAY_REPORTS_BUCKET", "gs://pubsite_prod_rev_1234567890/stats/")
	cfg, err := loadPlayConfig("")
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.ReportsBucket != "pubsite_prod_rev_1234567890" {
		t.Errorf("reports bucket = %q", cfg.ReportsBucket)
	}

	clearPlayEnv(t)
	path := writeConfig(t, "[play]\nreports_bucket = \"gs://pubsite_prod_rev_99\"\n")
	cfg, err = loadPlayConfig(path)
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.ReportsBucket != "pubsite_prod_rev_99" {
		t.Errorf("reports bucket from TOML = %q", cfg.ReportsBucket)
	}
}
