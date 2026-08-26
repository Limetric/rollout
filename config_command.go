package main

import (
	"fmt"

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

func init() {
	configCmd.AddCommand(configPathCmd)
	configCmd.AddCommand(configShowCmd)
}
