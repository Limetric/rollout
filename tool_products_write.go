package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Writing an in-app product.
//
// This is the legacy `inappproducts` resource, which is not edit-scoped: there
// is no draft, no validate, no commit. A confirmed write is live immediately,
// which is the whole reason it goes through the preview/confirm flow — and why
// the preview spends its effort on the one field that costs money to get wrong.
//
// Prices are a map from region to amount, and the API's PATCH replaces that map
// wholesale rather than merging it. A file listing eleven regions applied to a
// product priced in twelve therefore *removes* a price. That is invisible in
// the file, so the preview diffs region by region and names every add, change,
// and removal before anything is sent.

// productStatusActive and productStatusInactive are the API's own status
// values, spelled here because the CLI accepts them as `--status`.
const (
	productStatusActive   = "active"
	productStatusInactive = "inactive"
)

// UpdateProductArgs changes or creates one managed in-app product.
type UpdateProductArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	SKU         string `json:"sku" jsonschema:"the product's SKU, unique within the app"`
	// FromFile is the main way in. An InAppProduct has nested prices and
	// per-locale listings, and a flag surface wide enough to express them would
	// be worse than the JSON it is trying to avoid.
	FromFile string `json:"from_file,omitempty" jsonschema:"a JSON file holding the InAppProduct fields to write, in the Play API's own shape (prices, listings, defaultPrice, status); only the fields present are changed"`
	Status   string `json:"status,omitempty" jsonschema:"set the product's status: active or inactive"`
	// AutoConvertMissingPrices is the API's own convenience: it fills in every
	// region the request does not price, converted from the default price.
	AutoConvertMissingPrices bool `json:"auto_convert_missing_prices,omitempty" jsonschema:"let Play price the regions this write does not name, converted from the default price"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runUpdateProduct stages or applies an in-app product write.
func runUpdateProduct(ctx context.Context, c *Client, args UpdateProductArgs) (WriteResult, error) {
	const tool = "update_product"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	sku, err := validateResourceID("sku", args.SKU, "pass --sku (see `rollout play products`)")
	if err != nil {
		return WriteResult{}, err
	}
	status, err := normalizeProductStatus(args.Status)
	if err != nil {
		return WriteResult{}, err
	}

	// The fields this call is setting, keyed exactly as the API names them.
	// Reading the file as raw fields rather than a typed struct is what lets a
	// field the file omits stay omitted: a PATCH only touches what it carries,
	// and decoding through a struct would invent zero values for the rest.
	fields := map[string]json.RawMessage{}
	if args.FromFile != "" {
		if fields, err = readProductFile(args.FromFile); err != nil {
			return WriteResult{}, err
		}
	}
	if status != "" {
		fields["status"], _ = json.Marshal(status)
	}
	if len(fields) == 0 {
		return WriteResult{}, fmt.Errorf("nothing to change — pass --from-file product.json or --status active|inactive")
	}
	// The API derives both from the path; sending a conflicting value in the
	// body is a confusing 400, so this owns them rather than the file.
	delete(fields, "sku")
	delete(fields, "packageName")

	current, found, err := c.getInappProduct(ctx, pkg, sku)
	if err != nil {
		return WriteResult{}, toolError(tool, err)
	}
	if found && strings.EqualFold(current.PurchaseType, purchaseTypeSubscription) {
		return WriteResult{}, fmt.Errorf("%q is a subscription, not a managed product — Play no longer supports writing subscriptions through this API; use `rollout play subscription base-plan set-state` and `rollout play subscription offer set-state` instead", sku)
	}
	if !found && args.FromFile == "" {
		return WriteResult{}, fmt.Errorf("no product %q exists in %s, and creating one needs its full definition — pass --from-file product.json with at least defaultPrice, purchaseType, defaultLanguage, and listings", sku, pkg)
	}

	fields["sku"], _ = json.Marshal(sku)
	fields["packageName"], _ = json.Marshal(pkg)
	body, err := json.Marshal(fields)
	if err != nil {
		return WriteResult{}, fmt.Errorf("encode the product write: %w", err)
	}

	summary, err := describeProductWrite(pkg, sku, current, found, fields, args.AutoConvertMissingPrices)
	if err != nil {
		return WriteResult{}, err
	}

	request := editRequest{
		Method: http.MethodPatch, Path: "applications/" + pkg + "/inappproducts/" + sku,
		Body: body, Describe: "product " + sku,
	}
	if !found {
		request.Method, request.Path = http.MethodPost, "applications/"+pkg+"/inappproducts"
	}
	if args.AutoConvertMissingPrices {
		request.Query = map[string]string{"autoConvertMissingPrices": "true"}
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDirect, Summary: summary,
		Payload: editPayload{Requests: []editRequest{request}},
	})
}

// normalizeProductStatus validates --status. The API's own rejection of a bad
// value does not say which values are allowed.
func normalizeProductStatus(status string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "":
		return "", nil
	case productStatusActive:
		return productStatusActive, nil
	case productStatusInactive:
		return productStatusInactive, nil
	default:
		return "", fmt.Errorf("unknown status %q — use active or inactive", status)
	}
}

// readProductFile loads the InAppProduct fields to write.
func readProductFile(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, fmt.Errorf("read product file %q: %w", path, err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("parse product file %q: %w — it must be a JSON object of InAppProduct fields", path, err)
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("product file %q is empty — it must set at least one field", path)
	}
	return fields, nil
}

// getInappProduct reads one legacy product, reporting absence as an answer
// rather than an error: a SKU that does not exist yet is a create.
func (c *Client) getInappProduct(ctx context.Context, pkg, sku string) (apiInAppProduct, bool, error) {
	var product apiInAppProduct
	err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/inappproducts/"+sku, nil, nil, &product)
	if isNotFound(err) {
		return apiInAppProduct{}, false, nil
	}
	if err != nil {
		return apiInAppProduct{}, false, err
	}
	return product, true, nil
}

// describeProductWrite renders the preview: what changes, region by region.
func describeProductWrite(pkg, sku string, current apiInAppProduct, found bool, fields map[string]json.RawMessage, autoConvert bool) (string, error) {
	desired, err := decodeProductFields(fields)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if !found {
		fmt.Fprintf(&b, "Create managed product %s on %s", sku, pkg)
		if price := formatPrice(desired.DefaultPrice); price != "" {
			fmt.Fprintf(&b, " at %s", price)
		}
		if n := len(desired.Prices); n > 0 {
			fmt.Fprintf(&b, ", priced in %d region(s)", n)
		}
		if desired.Status != "" {
			fmt.Fprintf(&b, ", status %s", desired.Status)
		}
	} else {
		fmt.Fprintf(&b, "Update managed product %s on %s:", sku, pkg)
		changes := diffProduct(current, desired, fields)
		if len(changes) == 0 {
			return "", fmt.Errorf("product %s already matches — nothing to do", sku)
		}
		for _, change := range changes {
			fmt.Fprintf(&b, "\n  %s", change)
		}
	}
	if autoConvert {
		b.WriteString("\nPlay will price every region this write does not name, converted from the default price.")
	}
	return b.String(), nil
}

// decodeProductFields re-reads the assembled fields as a product, so the diff
// works on the same bytes that will be sent.
func decodeProductFields(fields map[string]json.RawMessage) (apiInAppProduct, error) {
	body, err := json.Marshal(fields)
	if err != nil {
		return apiInAppProduct{}, err
	}
	var desired apiInAppProduct
	if err := json.Unmarshal(body, &desired); err != nil {
		return apiInAppProduct{}, fmt.Errorf("the product fields are not a valid InAppProduct: %w", err)
	}
	return desired, nil
}

// diffProduct reports what an update actually changes.
//
// It considers only the fields the write carries. A field the caller did not
// mention is not compared, because a PATCH does not touch it — reporting it as
// "unchanged" would be noise, and reporting it as a change would be a lie.
func diffProduct(current, desired apiInAppProduct, fields map[string]json.RawMessage) []string {
	var changes []string
	if _, ok := fields["status"]; ok && !strings.EqualFold(current.Status, desired.Status) {
		changes = append(changes, fmt.Sprintf("status: %s → %s", orNone(current.Status), orNone(desired.Status)))
	}
	if _, ok := fields["defaultPrice"]; ok {
		before, after := formatPrice(current.DefaultPrice), formatPrice(desired.DefaultPrice)
		if before != after {
			changes = append(changes, fmt.Sprintf("default price: %s → %s", orNone(before), orNone(after)))
		}
	}
	if _, ok := fields["defaultLanguage"]; ok && current.DefaultLanguage != desired.DefaultLanguage {
		changes = append(changes, fmt.Sprintf("default language: %s → %s", orNone(current.DefaultLanguage), orNone(desired.DefaultLanguage)))
	}
	if _, ok := fields["prices"]; ok {
		changes = append(changes, diffProductPrices(current.Prices, desired.Prices)...)
	}
	if _, ok := fields["listings"]; ok {
		changes = append(changes, diffProductListings(current.Listings, desired.Listings)...)
	}
	// Anything else the file carries is still going to be written; naming it
	// without diffing it is better than letting it apply unmentioned.
	for _, field := range otherProductFields(fields) {
		changes = append(changes, "sets "+field)
	}
	return changes
}

// diffProductPrices names every regional price the write adds, changes, or
// removes. The removals are the point: the API replaces the whole map, so a
// region missing from the file loses its price without the file saying so.
func diffProductPrices(before, after map[string]apiPrice) []string {
	var changes []string
	for _, region := range sortedRegions(before, after) {
		old, hadOld := before[region]
		next, hasNext := after[region]
		oldText, nextText := formatPrice(old), formatPrice(next)
		switch {
		case !hadOld && hasNext:
			changes = append(changes, fmt.Sprintf("price in %s: added at %s", region, nextText))
		case hadOld && !hasNext:
			changes = append(changes, fmt.Sprintf("price in %s: REMOVED (was %s)", region, oldText))
		case oldText != nextText:
			changes = append(changes, fmt.Sprintf("price in %s: %s → %s", region, oldText, nextText))
		}
	}
	return changes
}

// diffProductListings names the locales a write adds, changes, or removes. As
// with prices, the API replaces the whole map.
func diffProductListings(before, after map[string]apiInAppProductTxt) []string {
	locales := map[string]bool{}
	for locale := range before {
		locales[locale] = true
	}
	for locale := range after {
		locales[locale] = true
	}
	ordered := make([]string, 0, len(locales))
	for locale := range locales {
		ordered = append(ordered, locale)
	}
	sort.Strings(ordered)

	var changes []string
	for _, locale := range ordered {
		old, hadOld := before[locale]
		next, hasNext := after[locale]
		switch {
		case !hadOld && hasNext:
			changes = append(changes, fmt.Sprintf("listing %s: added (%q)", locale, next.Title))
		case hadOld && !hasNext:
			changes = append(changes, fmt.Sprintf("listing %s: REMOVED (was %q)", locale, old.Title))
		case old.Title != next.Title:
			changes = append(changes, fmt.Sprintf("listing %s: title %q → %q", locale, old.Title, next.Title))
		case old.Description != next.Description:
			changes = append(changes, fmt.Sprintf("listing %s: description changed", locale))
		}
	}
	return changes
}

// sortedRegions is the union of two price maps, in a stable order.
func sortedRegions(before, after map[string]apiPrice) []string {
	seen := map[string]bool{}
	for region := range before {
		seen[region] = true
	}
	for region := range after {
		seen[region] = true
	}
	regions := make([]string, 0, len(seen))
	for region := range seen {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	return regions
}

// diffedProductFields are the fields diffProduct compares field by field.
// Anything outside this set is reported by name instead.
var diffedProductFields = map[string]bool{
	"status": true, "defaultPrice": true, "defaultLanguage": true,
	"prices": true, "listings": true, "sku": true, "packageName": true,
}

func otherProductFields(fields map[string]json.RawMessage) []string {
	var out []string
	for field := range fields {
		if !diffedProductFields[field] {
			out = append(out, field)
		}
	}
	sort.Strings(out)
	return out
}

// --- CLI front-end ---

var updateProductArgs UpdateProductArgs

var productSetCmd = &cobra.Command{
	Use:         "set",
	Short:       "Create or update a managed in-app product (previews price deltas; --confirm to apply)",
	Annotations: mcpTool("update_product"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, updateProductArgs, runUpdateProduct)
	},
}

func init() {
	addPackageFlag(productSetCmd, &updateProductArgs.PackageName)
	productSetCmd.Flags().StringVar(&updateProductArgs.SKU, "sku", "", "product SKU (required)")
	productSetCmd.Flags().StringVar(&updateProductArgs.FromFile, "from-file", "", "JSON file of InAppProduct fields to write")
	productSetCmd.Flags().StringVar(&updateProductArgs.Status, "status", "", "set the product status: active or inactive")
	productSetCmd.Flags().BoolVar(&updateProductArgs.AutoConvertMissingPrices, "auto-convert-missing-prices", false, "let Play price the regions this write does not name")
	addConfirmFlag(productSetCmd, &updateProductArgs.Confirm)
	productCmd.AddCommand(productSetCmd)
}
