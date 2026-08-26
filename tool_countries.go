package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

// CountriesArgs reads a track's country availability.
type CountriesArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track       string `json:"track" jsonschema:"the track whose country availability to read"`
}

// CountriesResult is where a track is available.
type CountriesResult struct {
	PackageName string   `json:"package_name"`
	Track       string   `json:"track"`
	Countries   []string `json:"countries"`
	// SyncWithProduction is the API's own flag: when set, the track's
	// availability follows production rather than the list above, so an empty
	// Countries list is not the same as "available nowhere".
	SyncWithProduction bool `json:"sync_with_production"`
	RestOfWorld        bool `json:"rest_of_world"`
}

func (r CountriesResult) tableRows() ([]json.RawMessage, []string) {
	rows := make([]json.RawMessage, 0, len(r.Countries))
	for _, code := range r.Countries {
		rows = append(rows, jsonRow(map[string]string{"track": r.Track, "country": code}))
	}
	return rows, []string{"track", "country"}
}

// apiCountryAvailability is the wire shape.
type apiCountryAvailability struct {
	SyncWithProduction bool `json:"syncWithProduction"`
	RestOfWorld        bool `json:"restOfWorld"`
	Countries          []struct {
		CountryCode string `json:"countryCode"`
	} `json:"countries"`
}

// runCountries reads a track's country availability inside a read-only edit.
func runCountries(ctx context.Context, c *Client, args CountriesArgs) (CountriesResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return CountriesResult{}, err
	}
	if args.Track == "" {
		return CountriesResult{}, fmt.Errorf("track is required — pass --track production (availability is per track)")
	}
	var availability apiCountryAvailability
	_, err = c.withEdit(ctx, pkg, false, commitOptions{}, func(e *editSession) error {
		return c.do(ctx, http.MethodGet, e.path("countryAvailability/"+args.Track), nil, nil, &availability)
	})
	if err != nil {
		return CountriesResult{}, toolError("countries", err)
	}

	out := CountriesResult{
		PackageName:        pkg,
		Track:              args.Track,
		SyncWithProduction: availability.SyncWithProduction,
		RestOfWorld:        availability.RestOfWorld,
	}
	for _, country := range availability.Countries {
		out.Countries = append(out.Countries, country.CountryCode)
	}
	return out, nil
}

// --- CLI front-end ---

var (
	countriesArgs   CountriesArgs
	countriesFormat string
)

// countriesCmd reads availability and parents `countries set`.
var countriesCmd = &cobra.Command{
	Use:         "countries",
	Short:       "Show where a track is available",
	Annotations: mcpTool("countries"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, countriesArgs, countriesFormat, runCountries)
	},
}

func init() {
	addPackageFlag(countriesCmd, &countriesArgs.PackageName)
	countriesCmd.Flags().StringVar(&countriesArgs.Track, "track", "", "track (required)")
	addFormatFlag(countriesCmd, &countriesFormat)
}
