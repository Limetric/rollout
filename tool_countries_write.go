package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// SetCountriesArgs scopes a track's release to a set of countries.
type SetCountriesArgs struct {
	PackageName  string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track        string   `json:"track" jsonschema:"the track holding the release"`
	VersionCodes []string `json:"version_codes,omitempty" jsonschema:"the version codes identifying the release; omit when the track holds exactly one release"`
	Countries    []string `json:"countries" jsonschema:"ISO 3166-1 alpha-2 country codes such as NL, DE; this replaces the current list"`
	RestOfWorld  bool     `json:"rest_of_world,omitempty" jsonschema:"also ship to every country not named above"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runSetCountries stages or applies a country-targeting change.
//
// Play has no track-level country update: availability is a property of the
// *release* (`Track.Release.countryTargeting`), which is why this is a release
// patch rather than a write to the countryAvailability resource that
// `play_countries` reads. That resource is read-only in the Publisher API.
func runSetCountries(ctx context.Context, c *Client, args SetCountriesArgs) (WriteResult, error) {
	const tool = "set_countries"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.Track == "" {
		return WriteResult{}, fmt.Errorf("track is required — pass --track production")
	}
	countries, err := normalizeCountryCodes(args.Countries)
	if err != nil {
		return WriteResult{}, err
	}

	var codes []string
	if len(args.VersionCodes) > 0 {
		if codes, err = parseVersionCodes(args.VersionCodes); err != nil {
			return WriteResult{}, err
		}
	}

	patch, err := json.Marshal(map[string]any{
		"countryTargeting": countryTargeting{Countries: countries, IncludeRestOfWorld: args.RestOfWorld},
	})
	if err != nil {
		return WriteResult{}, err
	}

	scope := strings.Join(countries, ", ")
	if args.RestOfWorld {
		scope += " plus the rest of the world"
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchTrack, Track: args.Track,
		Summary: fmt.Sprintf("Make the %s release (%s) available in %s",
			args.Track, describeCurrentRelease(ctx, c, pkg, args.Track, codes), scope),
		Payload: trackPayload{
			Track: args.Track, MatchVersionCodes: codes, Patch: patch,
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// normalizeCountryCodes upper-cases and validates ISO 3166-1 alpha-2 codes.
// The API rejects a bad code with a message that does not name it.
func normalizeCountryCodes(codes []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range codes {
		for _, part := range strings.Split(raw, ",") {
			part = strings.ToUpper(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			if len(part) != 2 || !isAlpha(part) {
				return nil, fmt.Errorf("%q is not a two-letter country code — use ISO 3166-1 alpha-2, for example NL or DE", part)
			}
			if seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no countries given — pass --countries NL,DE")
	}
	sort.Strings(out)
	return out, nil
}

func isAlpha(s string) bool {
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// --- CLI front-end ---

var setCountriesArgs SetCountriesArgs

var setCountriesCmd = &cobra.Command{
	Use:         "set",
	Short:       "Set the countries a release ships to (previews first; --confirm to apply)",
	Annotations: mcpTool("set_countries"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, setCountriesArgs, runSetCountries)
	},
}

func init() {
	addPackageFlag(setCountriesCmd, &setCountriesArgs.PackageName)
	setCountriesCmd.Flags().StringVar(&setCountriesArgs.Track, "track", "", "track holding the release (required)")
	setCountriesCmd.Flags().StringArrayVar(&setCountriesArgs.VersionCodes, "version-codes", nil, "version codes identifying the release")
	setCountriesCmd.Flags().StringArrayVar(&setCountriesArgs.Countries, "countries", nil, "ISO 3166-1 alpha-2 country codes (repeatable or comma-separated)")
	setCountriesCmd.Flags().BoolVar(&setCountriesArgs.RestOfWorld, "rest-of-world", false, "also ship to every country not named")
	setCountriesCmd.Flags().BoolVar(&setCountriesArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(setCountriesCmd, &setCountriesArgs.Confirm)
	countriesCmd.AddCommand(setCountriesCmd)
}
