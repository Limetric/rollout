package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect and update configuration",
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the config path selected by rollout",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		resolved, err := resolveConfigPath(configPath)
		if err != nil {
			return fmt.Errorf("resolve config path: %w", err)
		}
		if resolved == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "environment only (no config file)")
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), resolved)
		return nil
	},
}

// configShowCmd prints the fully resolved configuration (file + env overlay)
// with credentials redacted, so users can see which values are in effect
// without exposing secrets in scrollback or logs. The config file is shared;
// each platform renders its own section.
var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the resolved configuration (secrets redacted)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		resolved, err := resolveConfigPath(configPath)
		if err != nil {
			return fmt.Errorf("resolve config path: %w", err)
		}
		source := resolved
		if source == "" {
			source = "(none — environment only)"
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "config file:          %s\n", source)
		for _, p := range platforms() {
			if p.ShowConfig == nil {
				continue
			}
			fmt.Fprintf(out, "\n[%s] %s\n", p.Name, p.Title)
			if err := p.ShowConfig(out); err != nil {
				return err
			}
		}
		return nil
	},
}

// writableConfigPath returns the config file to write settings to: the
// explicit --config path when given, otherwise the default location (whose
// directory is created on demand — unlike resolveConfigPath, a missing default
// file is fine because we are about to create it).
func writableConfigPath(explicit string) (string, error) {
	if explicit != "" {
		// Create missing parents so a fresh --config path works, matching the
		// default-path branch below.
		if dir := filepath.Dir(explicit); dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", fmt.Errorf("create config directory %q: %w", dir, err)
			}
		}
		return explicit, nil
	}
	dir, err := userConfigDir()
	if err != nil {
		return "", fmt.Errorf("no usable config directory (%v) — set HOME/XDG_CONFIG_HOME or pass --config", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create config directory %q: %w", dir, err)
	}
	return filepath.Join(dir, defaultConfigFile), nil
}

// upsertConfigKey sets one key inside a TOML table (e.g. table "play", key
// "package_name"), preserving every other setting. The file is created if
// missing and rewritten 0600 (it holds secrets). Comments do not survive — the
// file is re-encoded from its parsed form, which the settings commands
// document, and which is acceptable because the user asked for this write
// explicitly.
//
// An empty table writes a top-level key.
func upsertConfigKey(path, table, key, value string) error {
	settings := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse existing config %q: %w", path, err)
		}
	case os.IsNotExist(err):
		// Fall through with an empty map: the file is about to be created.
	default:
		return fmt.Errorf("read config %q: %w", path, err)
	}
	dst := settings
	if table != "" {
		existing, ok := settings[table].(map[string]any)
		if !ok {
			if _, taken := settings[table]; taken {
				return fmt.Errorf("config %q has a non-table [%s] value; fix it by hand", path, table)
			}
			existing = map[string]any{}
			settings[table] = existing
		}
		dst = existing
	}
	dst[key] = value
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(settings); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	// Write-then-rename so an interrupted write can never truncate a config
	// file that holds credentials.
	if err := writeFileAtomic(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write config %q: %w", path, err)
	}
	return nil
}

// redactSecret renders a credential for display without exposing it; the last
// four characters are kept so two credentials can be told apart.
func redactSecret(s string) string {
	if s == "" {
		return "(not set)"
	}
	if len(s) <= 8 {
		return "set (redacted)"
	}
	return "set (…" + s[len(s)-4:] + ")"
}

// orNone renders an optional setting, naming the empty case rather than
// printing a blank line the reader has to interpret.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func init() {
	configCmd.AddCommand(configPathCmd)
	configCmd.AddCommand(configShowCmd)
}
