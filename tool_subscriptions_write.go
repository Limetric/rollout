package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Turning a base plan or an offer on and off.
//
// These are the two subscription writes the API actually supports as a state
// change: everything else about a subscription is a patch of the whole
// resource. Neither is edit-scoped, so a confirmed call is live immediately.
//
// What deactivation does — and does not do — is the thing worth spelling out in
// the preview. It stops *new* subscribers. Everybody already on the plan keeps
// their subscription and keeps being charged. A reader who assumes otherwise
// will deactivate a base plan expecting to end a billing relationship, and it
// will not.

// The states these tools accept, and the API states they map onto. The API
// reports DRAFT as well, but a base plan cannot be moved *to* draft: it is
// where one starts.
const (
	stateActive   = "active"
	stateInactive = "inactive"

	apiStateActive   = "ACTIVE"
	apiStateInactive = "INACTIVE"
)

// SetBasePlanStateArgs activates or deactivates a base plan.
type SetBasePlanStateArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	ProductID   string `json:"product_id" jsonschema:"the subscription's product ID, from play_subscriptions"`
	BasePlanID  string `json:"base_plan_id" jsonschema:"the base plan to change, from play_subscriptions"`
	State       string `json:"state" jsonschema:"active to make the base plan available to new subscribers, inactive to stop new sign-ups"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runSetBasePlanState stages or applies a base plan state change.
func runSetBasePlanState(ctx context.Context, c *Client, args SetBasePlanStateArgs) (WriteResult, error) {
	const tool = "set_base_plan_state"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	productID, err := validateResourceID("product_id", args.ProductID, "pass --id (see `rollout play subscriptions`)")
	if err != nil {
		return WriteResult{}, err
	}
	basePlanID, err := validateResourceID("base_plan_id", args.BasePlanID, "pass --base-plan (see `rollout play subscriptions`)")
	if err != nil {
		return WriteResult{}, err
	}
	state, err := normalizeSubscriptionState(args.State)
	if err != nil {
		return WriteResult{}, err
	}

	sub, err := c.getSubscription(ctx, pkg, productID)
	if err != nil {
		return WriteResult{}, toolError(tool, err)
	}
	plan, err := findBasePlan(sub, basePlanID)
	if err != nil {
		return WriteResult{}, err
	}
	if err := checkStateChange("base plan "+basePlanID, plan.State, state); err != nil {
		return WriteResult{}, err
	}
	// The subscription resource does not carry its offers, and the offers are
	// the part of this write that reaches past the id the caller named: they
	// hang off the base plan and go wherever it goes. A preview that cannot
	// name them is asking for a confirmation of something it has not shown.
	if plan.Offers, err = c.listBasePlanOffers(ctx, pkg, productID, basePlanID); err != nil {
		return WriteResult{}, toolError(tool, err)
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "%s base plan %s of subscription %s: %s → %s",
		verbFor(state), basePlanID, productID, orNone(plan.State), apiStateFor(state))
	if plan.BillingPeriod != "" {
		fmt.Fprintf(&summary, "\n  billing period %s, configured in %d region(s), %d open to new subscribers",
			plan.BillingPeriod, plan.RegionCount, plan.AvailableRegions)
	}
	if n := len(plan.Offers); n > 0 {
		// An offer is an extension of its base plan, so this reaches further
		// than the one id the caller named.
		fmt.Fprintf(&summary, "\n  %d offer(s) hang off this base plan and follow it: %s", n, offerIDs(plan.Offers))
	}
	if state == stateInactive {
		summary.WriteString("\n  Existing subscribers keep their subscription and continue to be billed; only new sign-ups stop.")
	}

	body, err := json.Marshal(map[string]string{
		"packageName": pkg, "productId": productID, "basePlanId": basePlanID,
	})
	if err != nil {
		return WriteResult{}, err
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDirect, Summary: summary.String(),
		// Closing a base plan to new subscribers ends acquisition on a whole
		// price point, and every offer under it goes with it. That is worth a
		// second look; turning one back on is not.
		RequiresDouble: state == stateInactive,
		Payload: editPayload{Requests: []editRequest{{
			Method: http.MethodPost,
			Path: fmt.Sprintf("applications/%s/subscriptions/%s/basePlans/%s:%s",
				pkg, productID, basePlanID, verbPathFor(state)),
			Body:     body,
			Describe: "base plan " + basePlanID + " of " + productID,
		}}},
	})
}

// SetOfferStateArgs activates or deactivates a subscription offer.
type SetOfferStateArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	ProductID   string `json:"product_id" jsonschema:"the subscription's product ID, from play_subscriptions"`
	BasePlanID  string `json:"base_plan_id" jsonschema:"the base plan the offer extends, from play_subscriptions"`
	OfferID     string `json:"offer_id" jsonschema:"the offer to change, from play_subscriptions"`
	State       string `json:"state" jsonschema:"active to make the offer available, inactive to withdraw it"`

	Confirm string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runSetOfferState stages or applies an offer state change.
func runSetOfferState(ctx context.Context, c *Client, args SetOfferStateArgs) (WriteResult, error) {
	const tool = "set_offer_state"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	productID, err := validateResourceID("product_id", args.ProductID, "pass --id (see `rollout play subscriptions`)")
	if err != nil {
		return WriteResult{}, err
	}
	basePlanID, err := validateResourceID("base_plan_id", args.BasePlanID, "pass --base-plan (see `rollout play subscriptions`)")
	if err != nil {
		return WriteResult{}, err
	}
	offerID, err := validateResourceID("offer_id", args.OfferID, "pass --offer (see `rollout play subscriptions`)")
	if err != nil {
		return WriteResult{}, err
	}
	state, err := normalizeSubscriptionState(args.State)
	if err != nil {
		return WriteResult{}, err
	}

	offer, err := c.getSubscriptionOffer(ctx, pkg, productID, basePlanID, offerID)
	if err != nil {
		return WriteResult{}, toolError(tool, err)
	}
	if err := checkStateChange("offer "+offerID, offer.State, state); err != nil {
		return WriteResult{}, err
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "%s offer %s on base plan %s of subscription %s: %s → %s",
		verbFor(state), offerID, basePlanID, productID, orNone(offer.State), apiStateFor(state))
	fmt.Fprintf(&summary, "\n  %d phase(s), configured in %d region(s), %d open to new subscribers",
		offer.PhaseCount, offer.RegionCount, offer.AvailableRegions)
	if state == stateActive {
		// Activating an offer whose base plan is off changes nothing a user can
		// see, and that is a confusing way for a promotion to fail.
		summary.WriteString("\n  An offer is only live while its base plan is active too.")
	}

	body, err := json.Marshal(map[string]string{
		"packageName": pkg, "productId": productID, "basePlanId": basePlanID, "offerId": offerID,
	})
	if err != nil {
		return WriteResult{}, err
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchDirect, Summary: summary.String(),
		Payload: editPayload{Requests: []editRequest{{
			Method: http.MethodPost,
			Path: fmt.Sprintf("applications/%s/subscriptions/%s/basePlans/%s/offers/%s:%s",
				pkg, productID, basePlanID, offerID, verbPathFor(state)),
			Body:     body,
			Describe: "offer " + offerID + " on base plan " + basePlanID,
		}}},
	})
}

// --- shared helpers ---

// normalizeSubscriptionState validates --state.
func normalizeSubscriptionState(state string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case stateActive:
		return stateActive, nil
	case stateInactive:
		return stateInactive, nil
	case "":
		return "", fmt.Errorf("state is required — pass --state active or --state inactive")
	default:
		return "", fmt.Errorf("unknown state %q — use active or inactive", state)
	}
}

func apiStateFor(state string) string {
	if state == stateActive {
		return apiStateActive
	}
	return apiStateInactive
}

// verbPathFor is the API's action suffix for a state change.
func verbPathFor(state string) string {
	if state == stateActive {
		return "activate"
	}
	return "deactivate"
}

func verbFor(state string) string {
	if state == stateActive {
		return "Activate"
	}
	return "Deactivate"
}

// checkStateChange refuses a write that would not change anything. Staging a
// no-op costs a confirmation and an audit line for nothing, and it reads in the
// log as a change that happened.
func checkStateChange(what, current, wanted string) error {
	if strings.EqualFold(current, apiStateFor(wanted)) {
		return fmt.Errorf("%s is already %s — nothing to do", what, apiStateFor(wanted))
	}
	return nil
}

// findBasePlan locates a base plan within a subscription, listing what is there
// when the id is wrong — the API's 404 does not.
func findBasePlan(sub Subscription, basePlanID string) (BasePlan, error) {
	var available []string
	for _, plan := range sub.BasePlans {
		if plan.BasePlanID == basePlanID {
			return plan, nil
		}
		available = append(available, plan.BasePlanID)
	}
	if len(available) == 0 {
		return BasePlan{}, fmt.Errorf("subscription %s has no base plans", sub.ProductID)
	}
	return BasePlan{}, fmt.Errorf("subscription %s has no base plan %q — it has: %s",
		sub.ProductID, basePlanID, strings.Join(available, ", "))
}

// listBasePlanOffers reads the offers extending one base plan, in a stable
// order so a preview reads the same twice.
func (c *Client) listBasePlanOffers(ctx context.Context, pkg, productID, basePlanID string) ([]SubscriptionOffer, error) {
	offers, _, err := c.listSubscriptionOffers(ctx, pkg, productID, basePlanID)
	if err != nil {
		return nil, err
	}
	sort.Slice(offers, func(a, b int) bool { return offers[a].OfferID < offers[b].OfferID })
	return offers, nil
}

// getSubscriptionOffer reads one offer.
func (c *Client) getSubscriptionOffer(ctx context.Context, pkg, productID, basePlanID, offerID string) (SubscriptionOffer, error) {
	path := fmt.Sprintf("applications/%s/subscriptions/%s/basePlans/%s/offers/%s", pkg, productID, basePlanID, offerID)
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &raw); err != nil {
		return SubscriptionOffer{}, err
	}
	return normalizeSubscriptionOffer(raw), nil
}

// offerIDs lists the offers riding on a base plan.
func offerIDs(offers []SubscriptionOffer) string {
	ids := make([]string, 0, len(offers))
	for _, offer := range offers {
		ids = append(ids, offer.OfferID)
	}
	return strings.Join(ids, ", ")
}

// --- CLI front-end ---

var (
	setBasePlanStateArgs SetBasePlanStateArgs
	setOfferStateArgs    SetOfferStateArgs
)

// basePlanCmd and offerCmd group the writes under the subscription they belong
// to, so `rollout play subscription --help` shows the whole surface.
var basePlanCmd = &cobra.Command{
	Use:   "base-plan",
	Short: "Change a subscription's base plans",
}

var offerCmd = &cobra.Command{
	Use:   "offer",
	Short: "Change a subscription's offers",
}

// The two front-ends part company here. On the CLI activate and deactivate
// would read as two verbs, but they are one operation with two values over MCP
// — and a tool is registered once, under one name. So the state is the argument
// rather than the command.
var basePlanSetStateCmd = &cobra.Command{
	Use:         "set-state",
	Short:       "Activate or deactivate a base plan (deactivating takes two confirmations)",
	Annotations: mcpTool("set_base_plan_state"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, setBasePlanStateArgs, runSetBasePlanState)
	},
}

var offerSetStateCmd = &cobra.Command{
	Use:         "set-state",
	Short:       "Activate or deactivate a subscription offer (previews first; --confirm to apply)",
	Annotations: mcpTool("set_offer_state"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, setOfferStateArgs, runSetOfferState)
	},
}

func init() {
	addPackageFlag(basePlanSetStateCmd, &setBasePlanStateArgs.PackageName)
	basePlanSetStateCmd.Flags().StringVar(&setBasePlanStateArgs.ProductID, "id", "", "subscription product ID (required)")
	basePlanSetStateCmd.Flags().StringVar(&setBasePlanStateArgs.BasePlanID, "base-plan", "", "base plan ID (required)")
	basePlanSetStateCmd.Flags().StringVar(&setBasePlanStateArgs.State, "state", "", "active or inactive (required)")
	addConfirmFlag(basePlanSetStateCmd, &setBasePlanStateArgs.Confirm)
	basePlanCmd.AddCommand(basePlanSetStateCmd)

	addPackageFlag(offerSetStateCmd, &setOfferStateArgs.PackageName)
	offerSetStateCmd.Flags().StringVar(&setOfferStateArgs.ProductID, "id", "", "subscription product ID (required)")
	offerSetStateCmd.Flags().StringVar(&setOfferStateArgs.BasePlanID, "base-plan", "", "base plan ID the offer extends (required)")
	offerSetStateCmd.Flags().StringVar(&setOfferStateArgs.OfferID, "offer", "", "offer ID (required)")
	offerSetStateCmd.Flags().StringVar(&setOfferStateArgs.State, "state", "", "active or inactive (required)")
	addConfirmFlag(offerSetStateCmd, &setOfferStateArgs.Confirm)
	offerCmd.AddCommand(offerSetStateCmd)

	subscriptionCmd.AddCommand(basePlanCmd, offerCmd)
}
