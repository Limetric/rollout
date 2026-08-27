package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Subscriptions, from the monetization API rather than the legacy
// `inappproducts` resource Google now tells you not to read them through.
//
// A subscription is three levels deep and only the first two arrive together:
// the subscription carries its base plans inline, but offers hang off a
// separate collection. Returning a subscription without its offers would be a
// half-answer — an offer is what a user is actually charged during a promotion,
// and "which offers are live" is the question these tools exist for. So offers
// are always inlined.
//
// That costs one extra request, not one per base plan: the offers collection
// accepts `-` for both the subscription and the base plan, so a single paged
// walk of `subscriptions/-/basePlans/-/offers` returns every offer in the app.

// wildcardID is the API's "any" for a path segment in the offers collection.
const wildcardID = "-"

// SubscriptionsArgs lists an app's subscriptions.
type SubscriptionsArgs struct {
	PackageName  string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	ShowArchived bool   `json:"show_archived,omitempty" jsonschema:"also return archived subscriptions"`
}

// SubscriptionsResult is the subscription catalogue with base plans and offers
// inlined.
type SubscriptionsResult struct {
	PackageName   string         `json:"package_name"`
	Subscriptions []Subscription `json:"subscriptions"`
	// Truncated says a walk stopped at the page cap rather than at the end.
	Truncated bool `json:"truncated,omitempty"`
}

func (r SubscriptionsResult) tableRows() ([]json.RawMessage, []string) {
	return subscriptionRows(r.Subscriptions), subscriptionFields
}

// SubscriptionArgs reads one subscription.
type SubscriptionArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	ProductID   string `json:"product_id" jsonschema:"the subscription's product ID, from play_subscriptions"`
}

// SubscriptionResult is one subscription.
type SubscriptionResult struct {
	PackageName  string       `json:"package_name"`
	Subscription Subscription `json:"subscription"`
}

func (r SubscriptionResult) tableRows() ([]json.RawMessage, []string) {
	return subscriptionRows([]Subscription{r.Subscription}), subscriptionFields
}

// subscriptionFields is the table shape both results share: one row per base
// plan, because a base plan is the thing that has a state you can change.
var subscriptionFields = []string{"product_id", "base_plan_id", "state", "billing_period", "offer_count"}

// subscriptionRows flattens subscriptions to one row per base plan. A
// subscription with no base plans still gets a row — it exists, and hiding it
// would make an empty draft look like a missing subscription.
func subscriptionRows(subscriptions []Subscription) []json.RawMessage {
	var rows []json.RawMessage
	for _, sub := range subscriptions {
		if len(sub.BasePlans) == 0 {
			rows = append(rows, jsonRow(map[string]any{"product_id": sub.ProductID}))
			continue
		}
		for _, plan := range sub.BasePlans {
			rows = append(rows, jsonRow(map[string]any{
				"product_id":     sub.ProductID,
				"base_plan_id":   plan.BasePlanID,
				"state":          plan.State,
				"billing_period": plan.BillingPeriod,
				"offer_count":    len(plan.Offers),
			}))
		}
	}
	return rows
}

// Subscription is one subscription with its base plans and their offers.
type Subscription struct {
	ProductID   string `json:"product_id"`
	PackageName string `json:"package_name,omitempty"`
	// Archived is the API's own flag. Play has withdrawn subscription
	// archiving, so this is read-only history rather than something to set.
	Archived  bool                  `json:"archived,omitempty"`
	Title     string                `json:"title,omitempty"`
	Listings  []SubscriptionListing `json:"listings,omitempty"`
	BasePlans []BasePlan            `json:"base_plans,omitempty"`
	// Raw is the API's own object, so nothing this binary does not model is
	// lost. Offers are not part of it — they are fetched separately.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// SubscriptionListing is one locale's consumer-visible subscription text.
type SubscriptionListing struct {
	LanguageCode string   `json:"language_code"`
	Title        string   `json:"title,omitempty"`
	Description  string   `json:"description,omitempty"`
	Benefits     []string `json:"benefits,omitempty"`
}

// BasePlan is a subscription's price and duration when no offer applies.
type BasePlan struct {
	BasePlanID string `json:"base_plan_id"`
	// State is DRAFT, ACTIVE, or INACTIVE. It is what play_set_base_plan_state
	// changes.
	State string `json:"state,omitempty"`
	// Kind is autoRenewing, prepaid, or installments — the API models each as
	// its own sub-object rather than as an enum.
	Kind          string `json:"kind,omitempty"`
	BillingPeriod string `json:"billing_period,omitempty"`
	// RegionCount is how many regions this base plan is configured for, and
	// AvailableRegions how many of those accept new subscribers. The two differ
	// exactly when a region has been closed to new sign-ups without being
	// removed, which is invisible in a plain region count.
	RegionCount      int                 `json:"region_count,omitempty"`
	AvailableRegions int                 `json:"available_regions,omitempty"`
	Offers           []SubscriptionOffer `json:"offers,omitempty"`
}

// SubscriptionOffer is a temporary offer extending a base plan.
type SubscriptionOffer struct {
	OfferID    string `json:"offer_id"`
	BasePlanID string `json:"base_plan_id,omitempty"`
	ProductID  string `json:"product_id,omitempty"`
	// State is DRAFT, ACTIVE, or INACTIVE. It is what play_set_offer_state
	// changes. An offer is only live if its base plan is active too.
	State            string          `json:"state,omitempty"`
	RegionCount      int             `json:"region_count,omitempty"`
	AvailableRegions int             `json:"available_regions,omitempty"`
	PhaseCount       int             `json:"phase_count,omitempty"`
	Raw              json.RawMessage `json:"raw,omitempty"`
}

// runSubscriptions lists every subscription with its base plans and offers.
func runSubscriptions(ctx context.Context, c *Client, args SubscriptionsArgs) (SubscriptionsResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return SubscriptionsResult{}, err
	}

	subscriptions, listTruncated, err := c.listSubscriptions(ctx, pkg, args.ShowArchived)
	if err != nil {
		return SubscriptionsResult{}, toolError("subscriptions", err)
	}
	// One walk of the whole app's offers, rather than one per base plan.
	offers, offersTruncated, err := c.listSubscriptionOffers(ctx, pkg, wildcardID, wildcardID)
	if err != nil {
		return SubscriptionsResult{}, toolError("subscriptions", err)
	}
	attachOffers(subscriptions, offers)

	return SubscriptionsResult{
		PackageName:   pkg,
		Subscriptions: subscriptions,
		Truncated:     listTruncated || offersTruncated,
	}, nil
}

// runSubscription reads one subscription with its base plans and offers.
func runSubscription(ctx context.Context, c *Client, args SubscriptionArgs) (SubscriptionResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return SubscriptionResult{}, err
	}
	productID, err := validateResourceID("product_id", args.ProductID, "pass --id (see `rollout play subscriptions`)")
	if err != nil {
		return SubscriptionResult{}, err
	}

	sub, err := c.getSubscription(ctx, pkg, productID)
	if err != nil {
		return SubscriptionResult{}, toolError("subscription", err)
	}
	offers, _, err := c.listSubscriptionOffers(ctx, pkg, productID, wildcardID)
	if err != nil {
		return SubscriptionResult{}, toolError("subscription", err)
	}
	subscriptions := []Subscription{sub}
	attachOffers(subscriptions, offers)

	return SubscriptionResult{PackageName: pkg, Subscription: subscriptions[0]}, nil
}

// attachOffers files each offer under the base plan it extends.
//
// An offer whose base plan is not in the list is dropped rather than invented:
// it means the base plan was filtered out (archived, say), and a floating offer
// under no plan would read as a live discount nobody can find.
func attachOffers(subscriptions []Subscription, offers []SubscriptionOffer) {
	index := map[string]map[string]*BasePlan{}
	for i := range subscriptions {
		plans := map[string]*BasePlan{}
		for j := range subscriptions[i].BasePlans {
			plan := &subscriptions[i].BasePlans[j]
			plans[plan.BasePlanID] = plan
		}
		index[subscriptions[i].ProductID] = plans
	}
	for _, offer := range offers {
		plans, ok := index[offer.ProductID]
		if !ok {
			continue
		}
		if plan, ok := plans[offer.BasePlanID]; ok {
			plan.Offers = append(plan.Offers, offer)
		}
	}
	for i := range subscriptions {
		for j := range subscriptions[i].BasePlans {
			offers := subscriptions[i].BasePlans[j].Offers
			sort.Slice(offers, func(a, b int) bool { return offers[a].OfferID < offers[b].OfferID })
		}
	}
}

// --- wire shapes ---

type apiSubscription struct {
	ProductID   string                   `json:"productId"`
	PackageName string                   `json:"packageName"`
	Archived    bool                     `json:"archived"`
	Listings    []apiSubscriptionListing `json:"listings"`
	BasePlans   []apiBasePlan            `json:"basePlans"`
}

type apiSubscriptionListing struct {
	LanguageCode string   `json:"languageCode"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Benefits     []string `json:"benefits"`
}

type apiBasePlan struct {
	BasePlanID      string                    `json:"basePlanId"`
	State           string                    `json:"state"`
	RegionalConfigs []apiRegionalAvailability `json:"regionalConfigs"`
	AutoRenewing    *apiBasePlanPeriod        `json:"autoRenewingBasePlanType"`
	Prepaid         *apiBasePlanPeriod        `json:"prepaidBasePlanType"`
	Installments    *apiBasePlanPeriod        `json:"installmentsBasePlanType"`
}

// apiBasePlanPeriod is the part every base plan type shares.
type apiBasePlanPeriod struct {
	BillingPeriodDuration string `json:"billingPeriodDuration"`
}

// apiRegionalAvailability is the shape base plans and offers share: a region
// and whether it is open to new subscribers.
type apiRegionalAvailability struct {
	RegionCode                string `json:"regionCode"`
	NewSubscriberAvailability bool   `json:"newSubscriberAvailability"`
}

type apiSubscriptionOffer struct {
	OfferID         string                    `json:"offerId"`
	BasePlanID      string                    `json:"basePlanId"`
	ProductID       string                    `json:"productId"`
	State           string                    `json:"state"`
	RegionalConfigs []apiRegionalAvailability `json:"regionalConfigs"`
	Phases          []json.RawMessage         `json:"phases"`
}

// --- API calls ---

// listSubscriptions walks the subscription catalogue.
func (c *Client) listSubscriptions(ctx context.Context, pkg string, showArchived bool) (subscriptions []Subscription, truncated bool, err error) {
	truncated, err = eachPage(func(token string) (string, bool, error) {
		query := url.Values{}
		if showArchived {
			query.Set("showArchived", "true")
		}
		if token != "" {
			query.Set("pageToken", token)
		}
		var page struct {
			Subscriptions []json.RawMessage `json:"subscriptions"`
			pagedResponse
		}
		if err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/subscriptions", query, nil, &page); err != nil {
			return "", false, err
		}
		for _, raw := range page.Subscriptions {
			subscriptions = append(subscriptions, normalizeSubscription(raw))
		}
		return page.next(), true, nil
	})
	return subscriptions, truncated, err
}

// getSubscription reads one subscription.
func (c *Client) getSubscription(ctx context.Context, pkg, productID string) (Subscription, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "applications/"+pkg+"/subscriptions/"+productID, nil, nil, &raw); err != nil {
		return Subscription{}, err
	}
	return normalizeSubscription(raw), nil
}

// listSubscriptionOffers walks an offers collection. Either id may be the
// wildcard `-`, which is what lets one call cover a whole app.
func (c *Client) listSubscriptionOffers(ctx context.Context, pkg, productID, basePlanID string) (offers []SubscriptionOffer, truncated bool, err error) {
	path := fmt.Sprintf("applications/%s/subscriptions/%s/basePlans/%s/offers", pkg, productID, basePlanID)
	truncated, err = eachPage(func(token string) (string, bool, error) {
		query := url.Values{}
		if token != "" {
			query.Set("pageToken", token)
		}
		var page struct {
			SubscriptionOffers []json.RawMessage `json:"subscriptionOffers"`
			pagedResponse
		}
		if err := c.do(ctx, http.MethodGet, path, query, nil, &page); err != nil {
			return "", false, err
		}
		for _, raw := range page.SubscriptionOffers {
			offers = append(offers, normalizeSubscriptionOffer(raw))
		}
		return page.next(), true, nil
	})
	return offers, truncated, err
}

// --- normalization ---

func normalizeSubscription(raw json.RawMessage) Subscription {
	out := Subscription{Raw: raw}
	var s apiSubscription
	if err := json.Unmarshal(raw, &s); err != nil {
		return out
	}
	out.ProductID, out.PackageName, out.Archived = s.ProductID, s.PackageName, s.Archived
	for _, listing := range s.Listings {
		out.Listings = append(out.Listings, SubscriptionListing(listing))
		if out.Title == "" {
			out.Title = listing.Title
		}
	}
	for _, plan := range s.BasePlans {
		out.BasePlans = append(out.BasePlans, normalizeBasePlan(plan))
	}
	return out
}

func normalizeBasePlan(plan apiBasePlan) BasePlan {
	out := BasePlan{
		BasePlanID:  plan.BasePlanID,
		State:       plan.State,
		RegionCount: len(plan.RegionalConfigs),
	}
	out.AvailableRegions = countAvailableRegions(plan.RegionalConfigs)
	switch {
	case plan.AutoRenewing != nil:
		out.Kind, out.BillingPeriod = "autoRenewing", plan.AutoRenewing.BillingPeriodDuration
	case plan.Prepaid != nil:
		out.Kind, out.BillingPeriod = "prepaid", plan.Prepaid.BillingPeriodDuration
	case plan.Installments != nil:
		out.Kind, out.BillingPeriod = "installments", plan.Installments.BillingPeriodDuration
	}
	return out
}

func normalizeSubscriptionOffer(raw json.RawMessage) SubscriptionOffer {
	out := SubscriptionOffer{Raw: raw}
	var o apiSubscriptionOffer
	if err := json.Unmarshal(raw, &o); err != nil {
		return out
	}
	out.OfferID, out.BasePlanID, out.ProductID = o.OfferID, o.BasePlanID, o.ProductID
	out.State = o.State
	out.RegionCount = len(o.RegionalConfigs)
	out.AvailableRegions = countAvailableRegions(o.RegionalConfigs)
	out.PhaseCount = len(o.Phases)
	return out
}

func countAvailableRegions(configs []apiRegionalAvailability) int {
	n := 0
	for _, config := range configs {
		if config.NewSubscriberAvailability {
			n++
		}
	}
	return n
}

// --- shared id validation ---

// validateResourceID checks an identifier that will be spliced into a URL path.
//
// Play's own rules are narrower than this (product IDs are lowercase letters,
// digits, underscores and periods), but the point here is not to re-implement
// the API's validation: it is that a caller-supplied id must never be able to
// change which resource a path addresses. A slash or a `?` would do exactly
// that, and the API's error for it names neither the field nor the reason.
func validateResourceID(field, value, hint string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required — %s", field, hint)
	}
	if strings.ContainsAny(value, "/?#&= \t\n") {
		return "", fmt.Errorf("%s %q contains a character that cannot appear in an ID — use the id exactly as the API reports it", field, value)
	}
	return value, nil
}

// --- CLI front-end ---

var (
	subscriptionsArgs   SubscriptionsArgs
	subscriptionsFormat string
	subscriptionArgs    SubscriptionArgs
	subscriptionFormat  string
)

var subscriptionsCmd = &cobra.Command{
	Use:         "subscriptions",
	Short:       "List subscriptions with their base plans and offers",
	Annotations: mcpTool("subscriptions"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, subscriptionsArgs, subscriptionsFormat, runSubscriptions)
	},
}

// subscriptionCmd parents everything that acts on one subscription.
var subscriptionCmd = &cobra.Command{
	Use:   "subscription",
	Short: "Read and change a single subscription",
}

var subscriptionGetCmd = &cobra.Command{
	Use:         "get",
	Short:       "Read one subscription with its base plans and offers",
	Annotations: mcpTool("subscription"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, subscriptionArgs, subscriptionFormat, runSubscription)
	},
}

func init() {
	addPackageFlag(subscriptionsCmd, &subscriptionsArgs.PackageName)
	subscriptionsCmd.Flags().BoolVar(&subscriptionsArgs.ShowArchived, "show-archived", false, "also return archived subscriptions")
	addFormatFlag(subscriptionsCmd, &subscriptionsFormat)

	addPackageFlag(subscriptionGetCmd, &subscriptionArgs.PackageName)
	subscriptionGetCmd.Flags().StringVar(&subscriptionArgs.ProductID, "id", "", "subscription product ID (required)")
	addFormatFlag(subscriptionGetCmd, &subscriptionFormat)
	subscriptionCmd.AddCommand(subscriptionGetCmd)
}
