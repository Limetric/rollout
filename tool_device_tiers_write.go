package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// CreateDeviceTierConfigArgs creates a device tier configuration from a file.
type CreateDeviceTierConfigArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	File        string `json:"file" jsonschema:"path to a JSON file holding the DeviceTierConfig to create"`
	// AllowUnknownDevices is the API's own escape hatch, and it is off by
	// default here for the same reason it is off there: a selector naming a
	// device Play has never heard of is nearly always a typo.
	AllowUnknownDevices bool `json:"allow_unknown_devices,omitempty" jsonschema:"accept device IDs that are not in Play's device catalogue"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runCreateDeviceTierConfig stages or applies a new device tier configuration.
//
// The config is taken from a file rather than from flags because the tier
// grammar is a nested selector language — RAM bands, system features, device
// groups, country sets — that no flag set can express without becoming a worse
// version of the JSON. The whole document is staged, so the confirm applies
// exactly what was previewed even from another process.
//
// Configurations are append-only: creating one never edits or replaces an
// existing one, and the app keeps using whichever its bundles reference.
func runCreateDeviceTierConfig(ctx context.Context, c *Client, args CreateDeviceTierConfigArgs) (WriteResult, error) {
	const tool = "create_device_tier_config"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.File == "" {
		return WriteResult{}, fmt.Errorf("file is required — pass --file device-tiers.json")
	}

	path := expandHome(args.File)
	data, err := os.ReadFile(path)
	if err != nil {
		return WriteResult{}, fmt.Errorf("read %q: %w", path, err)
	}
	config, err := parseDeviceTierConfigFile(path, data)
	if err != nil {
		return WriteResult{}, err
	}

	var query map[string]string
	if args.AllowUnknownDevices {
		query = map[string]string{"allowUnknownDevices": "true"}
	}
	summary := fmt.Sprintf("Create a device tier configuration for %s from %s (%s)",
		pkg, filepath.Base(path), describeDeviceTierConfig(config))
	if args.AllowUnknownDevices {
		summary += "\nUnknown devices are allowed: selectors naming devices absent from Play's catalogue will be accepted rather than rejected."
	}

	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDirect,
		Summary: summary,
		Payload: editPayload{Requests: []editRequest{{
			Method: http.MethodPost, Path: "applications/" + pkg + "/deviceTierConfigs",
			Query: query, Body: json.RawMessage(data),
			Describe: "device tier config from " + filepath.Base(path),
		}}},
	})
}

// deviceTierConfigFile is the part of a DeviceTierConfig the preview describes.
// Everything else is carried through verbatim — the selector grammar is the
// API's, and reshaping it here would only be a second place for it to drift.
type deviceTierConfigFile struct {
	DeviceGroups []struct {
		Name string `json:"name"`
	} `json:"deviceGroups"`
	DeviceTierSet struct {
		DeviceTiers []struct {
			Level int `json:"level"`
		} `json:"deviceTiers"`
	} `json:"deviceTierSet"`
	UserCountrySets []struct {
		Name string `json:"name"`
	} `json:"userCountrySets"`
}

// parseDeviceTierConfigFile checks the file is a DeviceTierConfig object before
// anything is staged. The API's own rejection quotes a field path against a
// document it does not echo back, which is unreadable next to the file the user
// actually wrote.
func parseDeviceTierConfigFile(path string, data []byte) (deviceTierConfigFile, error) {
	var config deviceTierConfigFile
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("%s is not a DeviceTierConfig object: %w — expected JSON with deviceGroups and deviceTierSet (see `rollout play device-tiers` for the shape of an existing one)", path, err)
	}
	if len(config.DeviceGroups) == 0 && len(config.DeviceTierSet.DeviceTiers) == 0 {
		return config, fmt.Errorf("%s has neither deviceGroups nor deviceTierSet — it would create an empty configuration", path)
	}
	return config, nil
}

// describeDeviceTierConfig renders what the file would create, naming the
// groups: a tier config is only ever read back by its numbers, and the group
// names are what a person recognizes.
func describeDeviceTierConfig(config deviceTierConfigFile) string {
	var parts []string

	names := make([]string, 0, len(config.DeviceGroups))
	for _, g := range config.DeviceGroups {
		if g.Name != "" {
			names = append(names, g.Name)
		}
	}
	sort.Strings(names)
	if len(config.DeviceGroups) > 0 {
		part := plural(len(config.DeviceGroups), "device group")
		if len(names) > 0 {
			part += ": " + strings.Join(names, ", ")
		}
		parts = append(parts, part)
	}

	if tiers := config.DeviceTierSet.DeviceTiers; len(tiers) > 0 {
		levels := make([]string, 0, len(tiers))
		for _, t := range tiers {
			levels = append(levels, fmt.Sprint(t.Level))
		}
		parts = append(parts, fmt.Sprintf("%s, level %s", plural(len(tiers), "tier"), strings.Join(levels, "/")))
	}
	if n := len(config.UserCountrySets); n > 0 {
		parts = append(parts, plural(n, "country set"))
	}
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, "; ")
}

// --- CLI front-end ---

var createDeviceTierConfigArgs CreateDeviceTierConfigArgs

var deviceTiersCreateCmd = &cobra.Command{
	Use:         "create",
	Short:       "Create a device tier configuration from a JSON file (previews first; --confirm to apply)",
	Annotations: mcpTool("create_device_tier_config"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, createDeviceTierConfigArgs, runCreateDeviceTierConfig)
	},
}

func init() {
	addPackageFlag(deviceTiersCreateCmd, &createDeviceTierConfigArgs.PackageName)
	deviceTiersCreateCmd.Flags().StringVar(&createDeviceTierConfigArgs.File, "file", "", "path to the DeviceTierConfig JSON (required)")
	deviceTiersCreateCmd.Flags().BoolVar(&createDeviceTierConfigArgs.AllowUnknownDevices, "allow-unknown-devices", false, "accept device IDs absent from Play's catalogue")
	addConfirmFlag(deviceTiersCreateCmd, &createDeviceTierConfigArgs.Confirm)
	deviceTiersCmd.AddCommand(deviceTiersCreateCmd)
}
