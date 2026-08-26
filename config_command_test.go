package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestUpsertConfigKeyCreatesAndPreserves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	if err := upsertConfigKey(path, playConfigTable, "package_name", "com.example.app"); err != nil {
		t.Fatalf("upsertConfigKey: %v", err)
	}
	// The file holds credentials; it must never be world-readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %v, want 0600", perm)
	}

	if err := upsertConfigKey(path, playConfigTable, "developer_id", "1234567890"); err != nil {
		t.Fatalf("second upsertConfigKey: %v", err)
	}

	var file playConfigFile
	if _, err := toml.DecodeFile(path, &file); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Writing the second key must not drop the first.
	if file.Play.PackageName != "com.example.app" || file.Play.DeveloperID != "1234567890" {
		t.Errorf("unexpected config: %+v", file.Play)
	}
}

// TestUpsertConfigKeyKeepsOtherTables: the config file is shared across
// platforms, so a Play setting must not disturb anyone else's section.
func TestUpsertConfigKeyKeepsOtherTables(t *testing.T) {
	path := writeConfig(t, "[other]\nkeep = \"me\"\n\n[play]\npackage_name = \"com.example.old\"\n")

	if err := upsertConfigKey(path, playConfigTable, "package_name", "com.example.new"); err != nil {
		t.Fatalf("upsertConfigKey: %v", err)
	}

	var parsed struct {
		Other struct {
			Keep string `toml:"keep"`
		} `toml:"other"`
		Play PlayConfig `toml:"play"`
	}
	if _, err := toml.DecodeFile(path, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Other.Keep != "me" {
		t.Error("another platform's settings were lost")
	}
	if parsed.Play.PackageName != "com.example.new" {
		t.Errorf("package name = %q", parsed.Play.PackageName)
	}
}

func TestSetPackageCommand(t *testing.T) {
	clearPlayEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	original := configPath
	configPath = path
	t.Cleanup(func() { configPath = original })

	var out bytes.Buffer
	playSetPackageCmd.SetOut(&out)
	t.Cleanup(func() { playSetPackageCmd.SetOut(nil) })

	if err := playSetPackageCmd.RunE(playSetPackageCmd, []string{"com.example.app"}); err != nil {
		t.Fatalf("set-package: %v", err)
	}
	cfg, err := loadPlayConfig(path)
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.PackageName != "com.example.app" {
		t.Errorf("package name = %q", cfg.PackageName)
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("command should say which file it wrote: %q", out.String())
	}
}

// TestSetPackageRejectsNonPackageNames: a display name typed here would be
// persisted and then fail with a 404 on every later command.
func TestSetPackageRejectsNonPackageNames(t *testing.T) {
	clearPlayEnv(t)
	original := configPath
	configPath = filepath.Join(t.TempDir(), "config.toml")
	t.Cleanup(func() { configPath = original })

	for _, bad := range []string{"My App", "example", "com..app", "com.9app", ""} {
		if err := playSetPackageCmd.RunE(playSetPackageCmd, []string{bad}); err == nil {
			t.Errorf("package name %q should have been rejected", bad)
		}
	}
	for _, good := range []string{"com.example.app", "com.example", "com.example.app_2", "com.Example.App3"} {
		if !validPackageName(good) {
			t.Errorf("package name %q should have been accepted", good)
		}
	}
}

func TestSetDeveloperIDRejectsNonDigits(t *testing.T) {
	clearPlayEnv(t)
	original := configPath
	configPath = filepath.Join(t.TempDir(), "config.toml")
	t.Cleanup(func() { configPath = original })

	if err := playSetDeveloperIDCmd.RunE(playSetDeveloperIDCmd, []string{"dev-1234"}); err == nil {
		t.Error("a non-numeric developer ID should have been rejected")
	}
	var out bytes.Buffer
	playSetDeveloperIDCmd.SetOut(&out)
	t.Cleanup(func() { playSetDeveloperIDCmd.SetOut(nil) })
	if err := playSetDeveloperIDCmd.RunE(playSetDeveloperIDCmd, []string{"1234567890"}); err != nil {
		t.Fatalf("set-developer-id: %v", err)
	}
}

// TestConfigShowRedactsSecrets is the reason `config show` exists as its own
// command: it must be safe to paste into a bug report.
func TestConfigShowRedactsSecrets(t *testing.T) {
	clearPlayEnv(t)
	t.Setenv("PLAY_SERVICE_ACCOUNT_JSON", testServiceAccountJSON)
	t.Setenv("PLAY_CLIENT_SECRET", "GOCSPX-super-secret-value")
	t.Setenv("PLAY_CLIENT_ID", "abc.apps.googleusercontent.com")
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })

	var out bytes.Buffer
	if err := playShowConfig(&out); err != nil {
		t.Fatalf("playShowConfig: %v", err)
	}
	rendered := out.String()
	for _, secret := range []string{"GOCSPX-super-secret-value", "not-a-real-key", "BEGIN PRIVATE KEY"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("config show leaked %q:\n%s", secret, rendered)
		}
	}
	// The service-account email is not a secret — it is the value the user has
	// to invite in the Play Console, so hiding it would make the output useless.
	if !strings.Contains(rendered, "bot@p.iam.gserviceaccount.com") {
		t.Errorf("config show should name the service account:\n%s", rendered)
	}
	// The client ID is public; the secret is masked but shown to exist.
	if !strings.Contains(rendered, "abc.apps.googleusercontent.com") {
		t.Errorf("config show should print the client id:\n%s", rendered)
	}
	if !strings.Contains(rendered, "set (…") {
		t.Errorf("config show should say the client secret is set:\n%s", rendered)
	}
}

// TestConfigShowExplainsWhichCredentialWins: someone who configured both modes
// is otherwise left guessing why their browser sign-in is ignored.
func TestConfigShowExplainsWhichCredentialWins(t *testing.T) {
	clearPlayEnv(t)
	t.Setenv("PLAY_SERVICE_ACCOUNT_JSON", testServiceAccountJSON)
	t.Setenv("PLAY_CLIENT_ID", "abc.apps.googleusercontent.com")
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })

	var out bytes.Buffer
	if err := playShowConfig(&out); err != nil {
		t.Fatalf("playShowConfig: %v", err)
	}
	if !strings.Contains(out.String(), "OAuth client is unused") {
		t.Errorf("config show should say which credential wins:\n%s", out.String())
	}
}

func TestRedactSecret(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "(not set)"},
		{"short", "set (redacted)"},
		{"GOCSPX-abcdefgh1234", "set (…1234)"},
	}
	for _, tc := range tests {
		if got := redactSecret(tc.in); got != tc.want {
			t.Errorf("redactSecret(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
