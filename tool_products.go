package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// In-app products: the things a user buys once.
//
// Play has two catalogues here and they are not the same API. The original
// `inappproducts` resource still holds every managed product, and the newer
// `monetization.oneTimeProducts` resource holds products created since Play
// split one-time purchases into products with purchase options. An app can have
// entries in either, so listing only one would quietly under-report the
// catalogue — this tool reads both and says which side each row came from.
//
// Neither resource is edit-scoped: a product write takes effect on its own,
// without a publishing transaction. That is why the writes here dispatch
// directly (see tool_products_write.go) rather than through an edit.

// Product sources, reported per row so a caller can tell which API a product
// lives in — it decides which write applies to it.
const (
	sourceInappProducts   = "inappproducts"
	sourceOneTimeProducts = "onetimeproducts"
)

// purchaseTypeSubscription is the legacy list's marker for a subscription.
// Those rows are excluded here: Google's own guidance is that `inappproducts`
// must no longer be used to read subscriptions, and play_subscriptions returns
// them with their base plans and offers, which this shape cannot represent.
const purchaseTypeSubscription = "subscription"

// ProductsArgs lists an app's in-app products.
type ProductsArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
}

// ProductsResult is the merged catalogue.
type ProductsResult struct {
	PackageName string    `json:"package_name"`
	Products    []Product `json:"products"`
	// Truncated says a walk stopped at the page cap rather than at the end of
	// the catalogue, so the list is a prefix and not the whole thing.
	Truncated bool `json:"truncated,omitempty"`
	// Note states what this list deliberately leaves out, so an agent does not
	// read a missing subscription as a missing product.
	Note string `json:"note,omitempty"`
}

// Product is one purchasable item, flattened from whichever catalogue it came
// from into the fields a human or an agent actually compares.
type Product struct {
	ProductID string `json:"product_id"`
	// Source names the API this product lives in, which decides what can write
	// to it: only `inappproducts` products are writable through
	// play_update_product.
	Source string `json:"source"`
	// Status is the product's own status for a legacy managed product. A
	// one-time product has no top-level status — its state lives per purchase
	// option — so this restates the distinct states of those options rather
	// than inventing a single one.
	Status string `json:"status,omitempty"`
	// PurchaseType is the legacy catalogue's own classification. One-time
	// products do not carry one.
	PurchaseType string `json:"purchase_type,omitempty"`
	Title        string `json:"title,omitempty"`
	// DefaultPrice is the developer-currency price a legacy product carries.
	// One-time products price per purchase option per region and have none.
	DefaultPrice string `json:"default_price,omitempty"`
	// RegionCount is how many regions carry an explicit price.
	RegionCount int `json:"region_count,omitempty"`
	// PurchaseOptions are a one-time product's buy/rent options with their
	// states. Empty for a legacy managed product.
	PurchaseOptions []PurchaseOption `json:"purchase_options,omitempty"`
	// Raw is the API's own object, so nothing this binary does not model is
	// lost on the way through.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// PurchaseOption is one way to buy a one-time product.
type PurchaseOption struct {
	PurchaseOptionID string `json:"purchase_option_id"`
	State            string `json:"state,omitempty"`
	// Kind is "buy" or "rent"; the API models them as separate sub-objects.
	Kind        string `json:"kind,omitempty"`
	RegionCount int    `json:"region_count,omitempty"`
}

func (r ProductsResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Products), []string{"product_id", "source", "status", "default_price", "title"}
}

// runProducts lists both product catalogues.
func runProducts(ctx context.Context, c *Client, args ProductsArgs) (ProductsResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ProductsResult{}, err
	}

	out := ProductsResult{PackageName: pkg}
	// Two catalogues, two failure modes: the credential can be allowed to read
	// one and not the other. Naming which walk failed is the difference between
	// a fixable permission error and a mystery.
	legacy, subscriptions, legacyTruncated, err := c.listInappProducts(ctx, pkg)
	if err != nil {
		return ProductsResult{}, toolError("products", fmt.Errorf("list managed products: %w", err))
	}
	oneTime, oneTimeTruncated, err := c.listOneTimeProducts(ctx, pkg)
	if err != nil {
		return ProductsResult{}, toolError("products", fmt.Errorf("list one-time products: %w", err))
	}

	out.Products = append(legacy, oneTime...)
	out.Truncated = legacyTruncated || oneTimeTruncated
	out.Note = productsNote(subscriptions)
	return out, nil
}

// productsNote spells out what the list omits. A caller that cannot see the
// exclusion would read a subscription's absence as the product not existing.
func productsNote(excludedSubscriptions int) string {
	note := "Managed products come from the legacy inappproducts API and one-time products from the monetization API; each row names its source. Subscriptions are not products — read them with play_subscriptions."
	if excludedSubscriptions > 0 {
		note += fmt.Sprintf(" %d legacy subscription SKU(s) were excluded from this list.", excludedSubscriptions)
	}
	return note
}

// --- legacy managed products ---

// apiInAppProduct is the wire shape of the legacy catalogue's entry.
type apiInAppProduct struct {
	SKU             string                        `json:"sku"`
	Status          string                        `json:"status"`
	PurchaseType    string                        `json:"purchaseType"`
	DefaultLanguage string                        `json:"defaultLanguage"`
	DefaultPrice    apiPrice                      `json:"defaultPrice"`
	Prices          map[string]apiPrice           `json:"prices"`
	Listings        map[string]apiInAppProductTxt `json:"listings"`
}

// apiInAppProductTxt is one locale's product listing.
type apiInAppProductTxt struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Benefits    []string `json:"benefits,omitempty"`
}

// apiPrice is the legacy catalogue's price: micros as a string, plus currency.
type apiPrice struct {
	PriceMicros string `json:"priceMicros"`
	Currency    string `json:"currency"`
}

// listInappProducts walks the legacy catalogue, returning the managed products
// and how many subscription rows it skipped.
func (c *Client) listInappProducts(ctx context.Context, pkg string) (products []Product, skippedSubscriptions int, truncated bool, err error) {
	truncated, err = eachPage(func(token string) (string, bool, error) {
		query := url.Values{}
		if token != "" {
			query.Set("token", token)
		}
		var page struct {
			InAppProduct []json.RawMessage `json:"inappproduct"`
			pagedResponse
		}
		if err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/inappproducts", query, nil, &page); err != nil {
			return "", false, err
		}
		for _, raw := range page.InAppProduct {
			var p apiInAppProduct
			if err := json.Unmarshal(raw, &p); err != nil {
				// An entry this binary cannot parse is still an entry: report
				// it with whatever the API sent rather than dropping it.
				products = append(products, Product{Source: sourceInappProducts, Raw: raw})
				continue
			}
			if strings.EqualFold(p.PurchaseType, purchaseTypeSubscription) {
				skippedSubscriptions++
				continue
			}
			products = append(products, normalizeInappProduct(p, raw))
		}
		return page.next(), true, nil
	})
	return products, skippedSubscriptions, truncated, err
}

// normalizeInappProduct flattens a legacy product.
func normalizeInappProduct(p apiInAppProduct, raw json.RawMessage) Product {
	return Product{
		ProductID:    p.SKU,
		Source:       sourceInappProducts,
		Status:       p.Status,
		PurchaseType: p.PurchaseType,
		Title:        inappProductTitle(p),
		DefaultPrice: formatPrice(p.DefaultPrice),
		RegionCount:  len(p.Prices),
		Raw:          raw,
	}
}

// inappProductTitle picks the title to show: the default language's, falling
// back to any locale so a product with listings still gets a name.
func inappProductTitle(p apiInAppProduct) string {
	if listing, ok := p.Listings[p.DefaultLanguage]; ok && listing.Title != "" {
		return listing.Title
	}
	locales := make([]string, 0, len(p.Listings))
	for locale := range p.Listings {
		locales = append(locales, locale)
	}
	sort.Strings(locales)
	for _, locale := range locales {
		if title := p.Listings[locale].Title; title != "" {
			return title
		}
	}
	return ""
}

// --- one-time products ---

// apiOneTimeProduct is the wire shape of a monetization one-time product.
type apiOneTimeProduct struct {
	ProductID       string                         `json:"productId"`
	Listings        []apiOneTimeProductListing     `json:"listings"`
	PurchaseOptions []apiOneTimeProductPurchaseOpt `json:"purchaseOptions"`
}

type apiOneTimeProductListing struct {
	LanguageCode string `json:"languageCode"`
	Title        string `json:"title"`
	Description  string `json:"description"`
}

type apiOneTimeProductPurchaseOpt struct {
	PurchaseOptionID string            `json:"purchaseOptionId"`
	State            string            `json:"state"`
	BuyOption        *json.RawMessage  `json:"buyOption"`
	RentOption       *json.RawMessage  `json:"rentOption"`
	RegionalConfigs  []json.RawMessage `json:"regionalPricingAndAvailabilityConfigs"`
}

// listOneTimeProducts walks the monetization catalogue.
func (c *Client) listOneTimeProducts(ctx context.Context, pkg string) (products []Product, truncated bool, err error) {
	truncated, err = eachPage(func(token string) (string, bool, error) {
		query := url.Values{}
		if token != "" {
			query.Set("pageToken", token)
		}
		var page struct {
			OneTimeProducts []json.RawMessage `json:"oneTimeProducts"`
			pagedResponse
		}
		if err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/oneTimeProducts", query, nil, &page); err != nil {
			return "", false, err
		}
		for _, raw := range page.OneTimeProducts {
			var p apiOneTimeProduct
			if err := json.Unmarshal(raw, &p); err != nil {
				products = append(products, Product{Source: sourceOneTimeProducts, Raw: raw})
				continue
			}
			products = append(products, normalizeOneTimeProduct(p, raw))
		}
		return page.next(), true, nil
	})
	return products, truncated, err
}

// normalizeOneTimeProduct flattens a one-time product.
func normalizeOneTimeProduct(p apiOneTimeProduct, raw json.RawMessage) Product {
	out := Product{
		ProductID: p.ProductID,
		Source:    sourceOneTimeProducts,
		Raw:       raw,
	}
	for _, listing := range p.Listings {
		if listing.Title != "" {
			out.Title = listing.Title
			break
		}
	}
	for _, opt := range p.PurchaseOptions {
		out.PurchaseOptions = append(out.PurchaseOptions, PurchaseOption{
			PurchaseOptionID: opt.PurchaseOptionID,
			State:            opt.State,
			Kind:             purchaseOptionKind(opt),
			RegionCount:      len(opt.RegionalConfigs),
		})
	}
	out.Status = summarizeOptionStates(out.PurchaseOptions)
	return out
}

// purchaseOptionKind reports whether an option is a purchase or a rental. The
// API models the two as mutually exclusive sub-objects rather than an enum.
func purchaseOptionKind(opt apiOneTimeProductPurchaseOpt) string {
	switch {
	case opt.BuyOption != nil:
		return "buy"
	case opt.RentOption != nil:
		return "rent"
	default:
		return ""
	}
}

// summarizeOptionStates restates a one-time product's purchase-option states as
// the row's status.
//
// It joins the distinct states rather than reducing them to one: a product with
// an active option and a draft option is genuinely both, and picking a winner
// would hide the draft.
func summarizeOptionStates(options []PurchaseOption) string {
	var states []string
	seen := map[string]bool{}
	for _, opt := range options {
		if opt.State == "" || seen[opt.State] {
			continue
		}
		seen[opt.State] = true
		states = append(states, opt.State)
	}
	sort.Strings(states)
	return strings.Join(states, ", ")
}

// --- price formatting ---

// formatPrice renders a legacy Price as "EUR 4.99".
func formatPrice(p apiPrice) string {
	if p.Currency == "" && p.PriceMicros == "" {
		return ""
	}
	micros, err := strconv.ParseInt(p.PriceMicros, 10, 64)
	if err != nil {
		// An unparseable amount is still worth showing verbatim — the caller
		// can see what the API said instead of an empty cell.
		return strings.TrimSpace(p.Currency + " " + p.PriceMicros)
	}
	return strings.TrimSpace(p.Currency + " " + formatMicros(micros))
}

// formatMicros renders 1/1,000,000ths of a currency unit as a decimal amount.
func formatMicros(micros int64) string {
	negative := micros < 0
	if negative {
		micros = -micros
	}
	out := strconv.FormatInt(micros/1_000_000, 10)
	if frac := micros % 1_000_000; frac != 0 {
		out += "." + strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	} else {
		out += ".00"
	}
	if negative {
		out = "-" + out
	}
	return out
}

// --- CLI front-end ---

var (
	productsArgs   ProductsArgs
	productsFormat string
)

// productsCmd lists the catalogue.
var productsCmd = &cobra.Command{
	Use:         "products",
	Short:       "List in-app products (managed products and one-time products)",
	Annotations: mcpTool("products"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, productsArgs, productsFormat, runProducts)
	},
}

// productCmd parents the single-product write. Reads are plural
// (`rollout play products`), writes act on one thing.
var productCmd = &cobra.Command{
	Use:   "product",
	Short: "Change a single in-app product",
}

func init() {
	addPackageFlag(productsCmd, &productsArgs.PackageName)
	addFormatFlag(productsCmd, &productsFormat)
}
