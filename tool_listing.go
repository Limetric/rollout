package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/spf13/cobra"
)

// ListingArgs reads the store listing text of an app.
type ListingArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Locale      string `json:"locale,omitempty" jsonschema:"a BCP-47 locale such as en-US or nl-NL; omit for every locale that has a listing"`
}

// Listing is one locale's store listing. The field names mirror the API's, and
// the character limits are the ones Play enforces: title 30, short description
// 80, full description 4000.
type Listing struct {
	Language         string `json:"language"`
	Title            string `json:"title,omitempty"`
	ShortDescription string `json:"short_description,omitempty"`
	FullDescription  string `json:"full_description,omitempty"`
	Video            string `json:"video,omitempty"`
}

// ListingResult is the listing text per locale.
type ListingResult struct {
	PackageName string    `json:"package_name"`
	Listings    []Listing `json:"listings"`
}

func (r ListingResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Listings), []string{"language", "title", "short_description"}
}

// apiListing is the API's Listing resource.
type apiListing struct {
	Language         string `json:"language"`
	Title            string `json:"title"`
	ShortDescription string `json:"shortDescription"`
	FullDescription  string `json:"fullDescription"`
	Video            string `json:"video"`
}

// normalize converts the wire shape to the output shape. The two carry the
// same fields under different JSON names — camelCase in, snake_case out — so
// this is a conversion, and adding a field to one without the other stops
// compiling rather than silently dropping it.
func (l apiListing) normalize() Listing { return Listing(l) }

// runListing reads store listing text inside a read-only edit.
func runListing(ctx context.Context, c *Client, args ListingArgs) (ListingResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ListingResult{}, err
	}
	listings, err := c.readListings(ctx, pkg, args.Locale)
	if err != nil {
		return ListingResult{}, toolError("listing", err)
	}
	return ListingResult{PackageName: pkg, Listings: listings}, nil
}

// readListings fetches one locale or all of them in a single read-only edit.
func (c *Client) readListings(ctx context.Context, pkg, locale string) ([]Listing, error) {
	var out []Listing
	_, err := c.withEdit(ctx, pkg, false, commitOptions{}, func(e *editSession) error {
		fetched, err := e.listings(ctx, locale)
		if err != nil {
			return err
		}
		out = fetched
		return nil
	})
	return out, err
}

// listings reads listing text inside an already-open edit, so a sync can reuse
// the edit it is about to write in rather than opening a second one.
func (e *editSession) listings(ctx context.Context, locale string) ([]Listing, error) {
	if locale != "" {
		var l apiListing
		if err := e.c.do(ctx, http.MethodGet, e.path("listings/"+locale), nil, nil, &l); err != nil {
			// A locale with no listing yet is an answer, not a failure: it is
			// exactly what a caller asks before creating one.
			if isNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		if l.Language == "" {
			l.Language = locale
		}
		return []Listing{l.normalize()}, nil
	}

	var list struct {
		Listings []apiListing `json:"listings"`
	}
	if err := e.c.do(ctx, http.MethodGet, e.path("listings"), nil, nil, &list); err != nil {
		return nil, err
	}
	out := make([]Listing, 0, len(list.Listings))
	for _, l := range list.Listings {
		out = append(out, l.normalize())
	}
	// Stable order so a diff between two runs is a diff in the data, not in
	// whatever order the API happened to return.
	sort.Slice(out, func(i, j int) bool { return out[i].Language < out[j].Language })
	return out, nil
}

// --- CLI front-end ---

var (
	listingArgs   ListingArgs
	listingFormat string
)

// listingCmd is both a readable command and the parent of the listing writes,
// so `rollout play listing` shows the listing and `rollout play listing set`
// changes it.
var listingCmd = &cobra.Command{
	Use:         "listing",
	Short:       "Show store listing text per locale",
	Annotations: mcpTool("listing"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, listingArgs, listingFormat, runListing)
	},
}

func init() {
	addPackageFlag(listingCmd, &listingArgs.PackageName)
	listingCmd.Flags().StringVar(&listingArgs.Locale, "locale", "", "a BCP-47 locale such as en-US (default: every locale)")
	addFormatFlag(listingCmd, &listingFormat)
}
