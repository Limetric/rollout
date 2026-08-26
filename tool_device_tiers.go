package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/spf13/cobra"
)

// DeviceTiersArgs lists the device tier configurations of an app.
type DeviceTiersArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
}

// DeviceTierConfig is one device tier configuration. The tier definitions
// themselves are carried raw: they are a nested selector grammar (RAM, system
// features, device groups) that nothing here needs to interpret, and reshaping
// them would only lose detail.
type DeviceTierConfig struct {
	ID  string          `json:"device_tier_config_id"`
	Raw json.RawMessage `json:"raw,omitempty"`
}

// DeviceTiersResult lists the configurations.
type DeviceTiersResult struct {
	PackageName string             `json:"package_name"`
	Configs     []DeviceTierConfig `json:"device_tier_configs"`
}

func (r DeviceTiersResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Configs), []string{"device_tier_config_id"}
}

// runDeviceTiers lists device tier configs. This one needs no edit: the
// configs hang off the application, not off a publishing transaction.
func runDeviceTiers(ctx context.Context, c *Client, args DeviceTiersArgs) (DeviceTiersResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return DeviceTiersResult{}, err
	}
	var list struct {
		DeviceTierConfigs []json.RawMessage `json:"deviceTierConfigs"`
	}
	if err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/deviceTierConfigs", nil, nil, &list); err != nil {
		return DeviceTiersResult{}, toolError("device_tiers", err)
	}

	out := DeviceTiersResult{PackageName: pkg}
	for _, raw := range list.DeviceTierConfigs {
		var id struct {
			DeviceTierConfigID string `json:"deviceTierConfigId"`
		}
		_ = json.Unmarshal(raw, &id)
		out.Configs = append(out.Configs, DeviceTierConfig{ID: id.DeviceTierConfigID, Raw: raw})
	}
	return out, nil
}

// --- CLI front-end ---

var (
	deviceTiersArgs   DeviceTiersArgs
	deviceTiersFormat string
)

var deviceTiersCmd = &cobra.Command{
	Use:         "device-tiers",
	Short:       "List device tier configurations",
	Annotations: mcpTool("device_tiers"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, deviceTiersArgs, deviceTiersFormat, runDeviceTiers)
	},
}

func init() {
	addPackageFlag(deviceTiersCmd, &deviceTiersArgs.PackageName)
	addFormatFlag(deviceTiersCmd, &deviceTiersFormat)
}
