package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// monetizationAPI fakes the non-edit monetization resources: two product
// catalogues, the subscription tree, and the state-change actions. It records
// what a confirmed write actually sent, because the interesting assertion for
// these tools is not that a call happened but what was in it.
type monetizationAPI struct {
	// inappProducts is the legacy catalogue, keyed by SKU.
	inappProducts map[string]string
	// oneTimeProducts is the monetization catalogue, as raw JSON objects.
	oneTimeProducts []string
	// subscriptions is the subscription catalogue, keyed by product ID.
	subscriptions map[string]string
	// offers are returned by every offers listing, wildcard or not.
	offers []string

	// missing is a set of SKUs to answer 404 for, so a create can be exercised.
	missing map[string]bool

	// Recorded writes.
	patches map[string]string
	posts   map[string]string
	queries map[string]string
}

func (a *monetizationAPI) handler(t *testing.T) *fakePlayAPI {
	t.Helper()
	a.patches, a.posts, a.queries = map[string]string{}, map[string]string{}, map[string]string{}
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPatch:
			a.patches[path] = string(body)
			a.queries[path] = r.URL.RawQuery
			writeJSON(w, http.StatusOK, `{}`)
		case r.Method == http.MethodPost:
			a.posts[path] = string(body)
			a.queries[path] = r.URL.RawQuery
			writeJSON(w, http.StatusOK, `{}`)
		case strings.HasSuffix(path, "/offers"):
			a.serveList(w, "subscriptionOffers", a.offersUnder(path))
		case strings.Contains(path, "/offers/"):
			a.serveOffer(w, path)
		case strings.HasSuffix(path, "/oneTimeProducts"):
			a.serveList(w, "oneTimeProducts", a.oneTimeProducts)
		case strings.HasSuffix(path, "/inappproducts"):
			a.serveList(w, "inappproduct", mapValues(a.inappProducts))
		case strings.HasSuffix(path, "/subscriptions"):
			a.serveList(w, "subscriptions", mapValues(a.subscriptions))
		case strings.Contains(path, "/inappproducts/"):
			a.serveOne(w, a.inappProducts, path)
		case strings.Contains(path, "/subscriptions/"):
			a.serveOne(w, a.subscriptions, path)
		default:
			writeJSON(w, http.StatusOK, `{}`)
		}
	})
}

func (a *monetizationAPI) serveList(w http.ResponseWriter, field string, items []string) {
	writeJSON(w, http.StatusOK, `{"`+field+`":[`+strings.Join(items, ",")+`]}`)
}

// serveOne answers a single-resource GET, 404ing for the ids the test marked
// missing so the create path can be exercised.
func (a *monetizationAPI) serveOne(w http.ResponseWriter, store map[string]string, path string) {
	id := path[strings.LastIndex(path, "/")+1:]
	if a.missing[id] {
		writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"not found"}}`)
		return
	}
	if body, ok := store[id]; ok {
		writeJSON(w, http.StatusOK, body)
		return
	}
	writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"not found"}}`)
}

// offersUnder returns the offers an offers-collection GET should see. The path
// segments may be the wildcard `-`, in which case they match everything — which
// is exactly the behaviour the wildcard walk depends on, so the fake has to
// honour the scoping rather than always returning the lot.
func (a *monetizationAPI) offersUnder(path string) []string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	productID, basePlanID := "-", "-"
	for i, part := range parts {
		if part == "subscriptions" && i+1 < len(parts) {
			productID = parts[i+1]
		}
		if part == "basePlans" && i+1 < len(parts) {
			basePlanID = parts[i+1]
		}
	}
	var out []string
	for _, offer := range a.offers {
		if productID != "-" && !strings.Contains(offer, `"productId":"`+productID+`"`) {
			continue
		}
		if basePlanID != "-" && !strings.Contains(offer, `"basePlanId":"`+basePlanID+`"`) {
			continue
		}
		out = append(out, offer)
	}
	return out
}

// serveOffer answers a single-offer GET out of the offers fixture.
func (a *monetizationAPI) serveOffer(w http.ResponseWriter, path string) {
	id := path[strings.LastIndex(path, "/")+1:]
	for _, offer := range a.offers {
		if strings.Contains(offer, `"offerId":"`+id+`"`) {
			writeJSON(w, http.StatusOK, offer)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, `{"error":{"code":404,"message":"not found"}}`)
}

// mapValues returns a map's values ordered by key, so a fake list response does
// not reorder between runs.
func mapValues(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// --- reads ---

// TestProductsMergesBothCatalogues: Play keeps managed products in the legacy
// resource and one-time products in the monetization one. Reading only one
// would under-report the catalogue without saying so.
func TestProductsMergesBothCatalogues(t *testing.T) {
	fake := &monetizationAPI{
		inappProducts: map[string]string{
			"coins_100": `{"sku":"coins_100","status":"active","purchaseType":"managedUser",
				"defaultLanguage":"en-US","defaultPrice":{"priceMicros":"4990000","currency":"EUR"},
				"prices":{"DE":{"priceMicros":"4990000","currency":"EUR"},"US":{"priceMicros":"5990000","currency":"USD"}},
				"listings":{"en-US":{"title":"100 coins","description":"Shiny"}}}`,
			"legacy_sub": `{"sku":"legacy_sub","status":"active","purchaseType":"subscription"}`,
		},
		oneTimeProducts: []string{
			`{"productId":"season_pass","listings":[{"languageCode":"en-US","title":"Season pass"}],
			  "purchaseOptions":[{"purchaseOptionId":"buy","state":"ACTIVE","buyOption":{},
			    "regionalPricingAndAvailabilityConfigs":[{"regionCode":"DE"},{"regionCode":"US"}]}]}`,
		},
	}
	client := newTestClient(t, fake.handler(t))

	res, err := runProducts(context.Background(), client, ProductsArgs{})
	if err != nil {
		t.Fatalf("runProducts: %v", err)
	}
	if len(res.Products) != 2 {
		t.Fatalf("got %d products, want the managed product and the one-time product: %+v", len(res.Products), res.Products)
	}

	managed, oneTime := res.Products[0], res.Products[1]
	if managed.ProductID != "coins_100" || managed.Source != sourceInappProducts {
		t.Errorf("first product = %+v, want the managed product tagged with its source", managed)
	}
	if managed.DefaultPrice != "EUR 4.99" {
		t.Errorf("default price = %q, want the micros rendered as a real amount", managed.DefaultPrice)
	}
	if managed.Title != "100 coins" || managed.RegionCount != 2 {
		t.Errorf("managed product = %+v, want its default-language title and region count", managed)
	}
	if oneTime.ProductID != "season_pass" || oneTime.Source != sourceOneTimeProducts {
		t.Errorf("second product = %+v, want the one-time product tagged with its source", oneTime)
	}
	if oneTime.Status != "ACTIVE" {
		t.Errorf("one-time status = %q, want it derived from the purchase options", oneTime.Status)
	}
	if len(oneTime.PurchaseOptions) != 1 || oneTime.PurchaseOptions[0].Kind != "buy" {
		t.Errorf("purchase options = %+v, want the buy option with its kind", oneTime.PurchaseOptions)
	}
}

// TestProductsExcludesLegacySubscriptionsOutLoud: the legacy list returns
// subscriptions too, and Google says not to read them there. Dropping them
// silently would read as the subscription not existing.
func TestProductsExcludesLegacySubscriptionsOutLoud(t *testing.T) {
	fake := &monetizationAPI{inappProducts: map[string]string{
		"legacy_sub": `{"sku":"legacy_sub","purchaseType":"subscription"}`,
	}}
	client := newTestClient(t, fake.handler(t))

	res, err := runProducts(context.Background(), client, ProductsArgs{})
	if err != nil {
		t.Fatalf("runProducts: %v", err)
	}
	if len(res.Products) != 0 {
		t.Errorf("subscriptions must not appear as products: %+v", res.Products)
	}
	for _, want := range []string{"1 legacy subscription SKU", "play_subscriptions"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("note %q should mention %q", res.Note, want)
		}
	}
}

func TestFormatMicros(t *testing.T) {
	for _, tc := range []struct {
		micros int64
		want   string
	}{
		{4990000, "4.99"},
		{5000000, "5.00"},
		{4500000, "4.5"},
		{0, "0.00"},
		{999, "0.000999"},
		{-1500000, "-1.5"},
	} {
		if got := formatMicros(tc.micros); got != tc.want {
			t.Errorf("formatMicros(%d) = %q, want %q", tc.micros, got, tc.want)
		}
	}
	// An amount the API sent in a shape we cannot parse is still shown.
	if got := formatPrice(apiPrice{PriceMicros: "not-a-number", Currency: "EUR"}); got != "EUR not-a-number" {
		t.Errorf("formatPrice = %q, want the raw value rather than an empty cell", got)
	}
}

// --- writes ---

// TestUpdateProductPreviewsPerRegionPriceDeltas is the point of this write's
// preview. The API replaces the whole price map, so a region the file leaves
// out loses its price — which the file itself does not say anywhere.
func TestUpdateProductPreviewsPerRegionPriceDeltas(t *testing.T) {
	isolateState(t)
	fake := &monetizationAPI{inappProducts: map[string]string{
		"coins_100": `{"sku":"coins_100","status":"active","purchaseType":"managedUser",
			"prices":{"DE":{"priceMicros":"4990000","currency":"EUR"},
			          "US":{"priceMicros":"5990000","currency":"USD"},
			          "GB":{"priceMicros":"3990000","currency":"GBP"}}}`,
	}}
	client := newTestClient(t, fake.handler(t))
	ctx := context.Background()

	// DE goes up, FR is new, GB is absent from the file and therefore dropped.
	file := writeTempJSON(t, `{"prices":{
		"DE":{"priceMicros":"5990000","currency":"EUR"},
		"US":{"priceMicros":"5990000","currency":"USD"},
		"FR":{"priceMicros":"5990000","currency":"EUR"}}}`)

	preview, err := runUpdateProduct(ctx, client, UpdateProductArgs{SKU: "coins_100", FromFile: file})
	if err != nil {
		t.Fatalf("runUpdateProduct: %v", err)
	}
	for _, want := range []string{
		"price in DE: EUR 4.99 → EUR 5.99",
		"price in FR: added at EUR 5.99",
		"price in GB: REMOVED (was GBP 3.99)",
	} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}
	// US is unchanged and must not be reported as a change.
	if strings.Contains(preview.Preview, "price in US") {
		t.Errorf("an unchanged region should not appear as a change:\n%s", preview.Preview)
	}

	if _, err := applyConfirmed(ctx, client, "update_product", preview.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	body, ok := fake.patches["/androidpublisher/v3/applications/com.example.app/inappproducts/coins_100"]
	if !ok {
		t.Fatalf("no PATCH was sent; patches = %v", fake.patches)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decode PATCH: %v", err)
	}
	// A PATCH must carry only what the caller set, plus the identity the path
	// already names — never a status or listing nobody mentioned.
	for _, unexpected := range []string{"status", "listings", "defaultPrice"} {
		if _, present := sent[unexpected]; present {
			t.Errorf("PATCH carries %q, which the caller never set: %s", unexpected, body)
		}
	}
	if _, present := sent["prices"]; !present {
		t.Errorf("PATCH does not carry the prices it previewed: %s", body)
	}
}

// TestUpdateProductCreatesWhenTheSKUIsNew: a SKU that does not exist is an
// insert, not a failed patch — but creating one needs a definition.
func TestUpdateProductCreatesWhenTheSKUIsNew(t *testing.T) {
	isolateState(t)
	fake := &monetizationAPI{
		inappProducts: map[string]string{},
		missing:       map[string]bool{"coins_500": true},
	}
	client := newTestClient(t, fake.handler(t))
	ctx := context.Background()

	if _, err := runUpdateProduct(ctx, client, UpdateProductArgs{SKU: "coins_500", Status: "active"}); err == nil ||
		!strings.Contains(err.Error(), "--from-file") {
		t.Errorf("err = %v, want a create without a definition to be refused with the fix", err)
	}

	file := writeTempJSON(t, `{"purchaseType":"managedUser","defaultLanguage":"en-US","status":"active",
		"defaultPrice":{"priceMicros":"9990000","currency":"EUR"},
		"listings":{"en-US":{"title":"500 coins","description":"More"}}}`)
	preview, err := runUpdateProduct(ctx, client, UpdateProductArgs{
		SKU: "coins_500", FromFile: file, AutoConvertMissingPrices: true,
	})
	if err != nil {
		t.Fatalf("runUpdateProduct: %v", err)
	}
	for _, want := range []string{"Create managed product coins_500", "EUR 9.99", "Play will price every region"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}

	if _, err := applyConfirmed(ctx, client, "update_product", preview.ConfirmToken); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	const insertPath = "/androidpublisher/v3/applications/com.example.app/inappproducts"
	body, ok := fake.posts[insertPath]
	if !ok {
		t.Fatalf("a new SKU must be inserted, not patched; posts = %v patches = %v", fake.posts, fake.patches)
	}
	if !strings.Contains(body, `"coins_500"`) {
		t.Errorf("the insert does not carry the SKU: %s", body)
	}
	// The query parameter decides whether the regions nobody priced get one at
	// all, so it has to survive being staged and confirmed.
	if q := fake.queries[insertPath]; !strings.Contains(q, "autoConvertMissingPrices=true") {
		t.Errorf("query = %q, want the staged autoConvertMissingPrices to reach the API", q)
	}
}

// TestUpdateProductRefusesSubscriptionsAndNoOps.
func TestUpdateProductRefusesSubscriptionsAndNoOps(t *testing.T) {
	isolateState(t)
	fake := &monetizationAPI{inappProducts: map[string]string{
		"legacy_sub": `{"sku":"legacy_sub","status":"active","purchaseType":"subscription"}`,
		"coins_100":  `{"sku":"coins_100","status":"active","purchaseType":"managedUser"}`,
	}}
	client := newTestClient(t, fake.handler(t))
	ctx := context.Background()

	_, err := runUpdateProduct(ctx, client, UpdateProductArgs{SKU: "legacy_sub", Status: "inactive"})
	if err == nil || !strings.Contains(err.Error(), "subscription") {
		t.Errorf("err = %v, want writing a subscription through this API to be refused", err)
	}
	if err != nil && !strings.Contains(err.Error(), "set-state") {
		t.Errorf("err = %v, should point at the subscription tools", err)
	}

	_, err = runUpdateProduct(ctx, client, UpdateProductArgs{SKU: "coins_100", Status: "active"})
	if err == nil || !strings.Contains(err.Error(), "already matches") {
		t.Errorf("err = %v, want a no-op reported rather than staged", err)
	}

	if _, err := runUpdateProduct(ctx, client, UpdateProductArgs{SKU: "coins_100"}); err == nil {
		t.Error("a call that changes nothing should be rejected")
	}
	if _, err := runUpdateProduct(ctx, client, UpdateProductArgs{Status: "active"}); err == nil {
		t.Error("sku should be required")
	}
	if _, err := runUpdateProduct(ctx, client, UpdateProductArgs{SKU: "coins_100", Status: "enabled"}); err == nil {
		t.Error("an unknown status should be rejected before it reaches the API")
	}
	if len(fake.patches)+len(fake.posts) != 0 {
		t.Errorf("nothing may be written while previewing: %v %v", fake.patches, fake.posts)
	}
}

// TestValidateResourceIDRejectsPathTricks: these ids are spliced into a URL, so
// one that can change which resource a path addresses is refused up front.
func TestValidateResourceIDRejectsPathTricks(t *testing.T) {
	if _, err := validateResourceID("sku", "", "pass --sku"); err == nil {
		t.Error("an empty id should be refused")
	}
	for _, bad := range []string{"a/b", "a?b", "a b", "a#b"} {
		if _, err := validateResourceID("sku", bad, "pass --sku"); err == nil {
			t.Errorf("id %q should be refused", bad)
		}
	}
	got, err := validateResourceID("sku", "  coins_100  ", "pass --sku")
	if err != nil || got != "coins_100" {
		t.Errorf("validateResourceID = %q, %v; want the trimmed id", got, err)
	}
}

// writeTempJSON drops a JSON file in a temp dir and returns its path.
func writeTempJSON(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "product.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
