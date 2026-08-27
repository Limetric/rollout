package main

import (
	"context"
	"strings"
	"testing"
)

// subscriptionFixture is a subscription with two base plans, one active and one
// inactive, used by most of the tests below.
const subscriptionFixture = `{
	"packageName":"com.example.app","productId":"premium",
	"listings":[{"languageCode":"en-US","title":"Premium","description":"All of it"}],
	"basePlans":[
		{"basePlanId":"monthly","state":"ACTIVE",
		 "autoRenewingBasePlanType":{"billingPeriodDuration":"P1M"},
		 "regionalConfigs":[{"regionCode":"DE","newSubscriberAvailability":true},
		                    {"regionCode":"US","newSubscriberAvailability":false}]},
		{"basePlanId":"yearly","state":"INACTIVE",
		 "autoRenewingBasePlanType":{"billingPeriodDuration":"P1Y"},
		 "regionalConfigs":[{"regionCode":"DE","newSubscriberAvailability":true}]}
	]}`

// offerFixtures include one offer whose base plan is not in the fixture, to
// prove a floating offer is dropped rather than shown under nothing.
var offerFixtures = []string{
	`{"offerId":"intro","productId":"premium","basePlanId":"monthly","state":"ACTIVE",
	  "regionalConfigs":[{"regionCode":"DE","newSubscriberAvailability":true}],
	  "phases":[{"duration":"P1M"}]}`,
	`{"offerId":"winter","productId":"premium","basePlanId":"monthly","state":"DRAFT",
	  "regionalConfigs":[{"regionCode":"DE","newSubscriberAvailability":false}],"phases":[{}]}`,
	`{"offerId":"orphan","productId":"premium","basePlanId":"gone","state":"ACTIVE"}`,
}

func subscriptionAPI() *monetizationAPI {
	return &monetizationAPI{
		subscriptions: map[string]string{"premium": subscriptionFixture},
		offers:        offerFixtures,
	}
}

// --- reads ---

// TestSubscriptionsInlineBasePlansAndOffers: a subscription without its offers
// is a half-answer, and fetching them per base plan would be a request each.
// The wildcard collection makes it one call for the whole app.
func TestSubscriptionsInlineBasePlansAndOffers(t *testing.T) {
	fake := subscriptionAPI()
	api := fake.handler(t)
	client := newTestClient(t, api)

	res, err := runSubscriptions(context.Background(), client, SubscriptionsArgs{})
	if err != nil {
		t.Fatalf("runSubscriptions: %v", err)
	}
	if len(res.Subscriptions) != 1 {
		t.Fatalf("got %d subscriptions, want 1: %+v", len(res.Subscriptions), res.Subscriptions)
	}
	sub := res.Subscriptions[0]
	if sub.ProductID != "premium" || sub.Title != "Premium" {
		t.Errorf("subscription = %+v, want its id and default title", sub)
	}
	if len(sub.BasePlans) != 2 {
		t.Fatalf("got %d base plans, want 2: %+v", len(sub.BasePlans), sub.BasePlans)
	}

	monthly := sub.BasePlans[0]
	if monthly.BasePlanID != "monthly" || monthly.State != "ACTIVE" {
		t.Errorf("base plan = %+v, want the active monthly plan", monthly)
	}
	if monthly.Kind != "autoRenewing" || monthly.BillingPeriod != "P1M" {
		t.Errorf("base plan = %+v, want its kind and billing period flattened", monthly)
	}
	// Two regions configured, one of them closed to new subscribers — a plain
	// region count would hide the closure.
	if monthly.RegionCount != 2 || monthly.AvailableRegions != 1 {
		t.Errorf("base plan = %+v, want 2 regions with 1 open to new subscribers", monthly)
	}
	if len(monthly.Offers) != 2 {
		t.Fatalf("got %d offers on monthly, want the two that name it: %+v", len(monthly.Offers), monthly.Offers)
	}
	if monthly.Offers[0].OfferID != "intro" || monthly.Offers[1].OfferID != "winter" {
		t.Errorf("offers = %+v, want them ordered by id", monthly.Offers)
	}
	if monthly.Offers[0].PhaseCount != 1 {
		t.Errorf("offer = %+v, want its phase count", monthly.Offers[0])
	}
	if len(sub.BasePlans[1].Offers) != 0 {
		t.Errorf("yearly has no offers, got %+v", sub.BasePlans[1].Offers)
	}

	// One wildcard call, not one per base plan.
	const wildcard = "/androidpublisher/v3/applications/com.example.app/subscriptions/-/basePlans/-/offers"
	if !api.sawCall("GET", wildcard) {
		t.Errorf("offers should be read in one wildcard call; calls = %v", api.calls())
	}
	offerCalls := 0
	for _, call := range api.calls() {
		if strings.HasSuffix(call, "/offers") {
			offerCalls++
		}
	}
	if offerCalls != 1 {
		t.Errorf("made %d offer calls, want exactly 1: %v", offerCalls, api.calls())
	}
}

// TestSubscriptionsDropOffersWithNoBasePlan: an offer whose base plan is not in
// the result would otherwise read as a live discount nobody can find.
func TestSubscriptionsDropOffersWithNoBasePlan(t *testing.T) {
	fake := subscriptionAPI()
	client := newTestClient(t, fake.handler(t))

	res, err := runSubscriptions(context.Background(), client, SubscriptionsArgs{})
	if err != nil {
		t.Fatalf("runSubscriptions: %v", err)
	}
	for _, plan := range res.Subscriptions[0].BasePlans {
		for _, offer := range plan.Offers {
			if offer.OfferID == "orphan" {
				t.Errorf("offer for base plan %q was filed under %q", offer.BasePlanID, plan.BasePlanID)
			}
		}
	}
}

func TestSubscriptionGetReadsOneWithItsOffers(t *testing.T) {
	fake := subscriptionAPI()
	api := fake.handler(t)
	client := newTestClient(t, api)

	res, err := runSubscription(context.Background(), client, SubscriptionArgs{ProductID: "premium"})
	if err != nil {
		t.Fatalf("runSubscription: %v", err)
	}
	if res.Subscription.ProductID != "premium" {
		t.Fatalf("subscription = %+v", res.Subscription)
	}
	if len(res.Subscription.BasePlans[0].Offers) != 2 {
		t.Errorf("offers were not inlined: %+v", res.Subscription.BasePlans[0])
	}
	// Scoped to the one subscription, not the whole app.
	const scoped = "/androidpublisher/v3/applications/com.example.app/subscriptions/premium/basePlans/-/offers"
	if !api.sawCall("GET", scoped) {
		t.Errorf("offers should be read for this subscription only; calls = %v", api.calls())
	}

	if _, err := runSubscription(context.Background(), client, SubscriptionArgs{}); err == nil {
		t.Error("product_id should be required")
	}
}

func TestSubscriptionsRenderAsBasePlanRows(t *testing.T) {
	fake := subscriptionAPI()
	client := newTestClient(t, fake.handler(t))

	res, err := runSubscriptions(context.Background(), client, SubscriptionsArgs{})
	if err != nil {
		t.Fatalf("runSubscriptions: %v", err)
	}
	rows, fields := res.tableRows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per base plan", len(rows))
	}
	table := formatTable(newStyles(nil), rows, fields)
	for _, want := range []string{"monthly", "P1M", "ACTIVE", "yearly", "INACTIVE"} {
		if !strings.Contains(table, want) {
			t.Errorf("table missing %q:\n%s", want, table)
		}
	}
}

// --- writes ---

// TestDeactivateBasePlanTakesTwoConfirmations: closing a base plan to new
// subscribers takes every offer under it with it.
func TestDeactivateBasePlanTakesTwoConfirmations(t *testing.T) {
	isolateState(t)
	fake := subscriptionAPI()
	api := fake.handler(t)
	client := newTestClient(t, api)
	ctx := context.Background()

	preview, err := runSetBasePlanState(ctx, client, SetBasePlanStateArgs{
		ProductID: "premium", BasePlanID: "monthly", State: "inactive",
	})
	if err != nil {
		t.Fatalf("runSetBasePlanState: %v", err)
	}
	for _, want := range []string{
		"Deactivate base plan monthly", "ACTIVE → INACTIVE",
		"Existing subscribers keep their subscription",
		"2 offer(s)", "intro, winter",
	} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}

	first, err := applyConfirmed(ctx, client, "set_base_plan_state", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.Applied {
		t.Fatal("deactivating a base plan must take two confirmations")
	}
	if len(fake.posts) != 0 {
		t.Fatalf("nothing may be sent before the second confirmation: %v", fake.posts)
	}

	if _, err := applyConfirmed(ctx, client, "set_base_plan_state", first.ConfirmToken); err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	const path = "/androidpublisher/v3/applications/com.example.app/subscriptions/premium/basePlans/monthly:deactivate"
	body, ok := fake.posts[path]
	if !ok {
		t.Fatalf("no deactivate was sent; posts = %v", fake.posts)
	}
	for _, want := range []string{`"packageName":"com.example.app"`, `"productId":"premium"`, `"basePlanId":"monthly"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request body missing %s: %s", want, body)
		}
	}
}

// TestActivateBasePlanAppliesOnOneConfirmation: turning a plan back on is not
// destructive, so it does not pay the second-confirmation tax.
func TestActivateBasePlanAppliesOnOneConfirmation(t *testing.T) {
	isolateState(t)
	fake := subscriptionAPI()
	client := newTestClient(t, fake.handler(t))
	ctx := context.Background()

	preview, err := runSetBasePlanState(ctx, client, SetBasePlanStateArgs{
		ProductID: "premium", BasePlanID: "yearly", State: "active",
	})
	if err != nil {
		t.Fatalf("runSetBasePlanState: %v", err)
	}
	if !strings.Contains(preview.Preview, "INACTIVE → ACTIVE") {
		t.Errorf("preview should show the transition:\n%s", preview.Preview)
	}

	applied, err := applyConfirmed(ctx, client, "set_base_plan_state", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !applied.Applied {
		t.Fatal("activating should apply on the first confirmation")
	}
	const path = "/androidpublisher/v3/applications/com.example.app/subscriptions/premium/basePlans/yearly:activate"
	if _, ok := fake.posts[path]; !ok {
		t.Errorf("no activate was sent; posts = %v", fake.posts)
	}
}

func TestSetBasePlanStateRejectsNoOpsAndUnknownPlans(t *testing.T) {
	isolateState(t)
	fake := subscriptionAPI()
	client := newTestClient(t, fake.handler(t))
	ctx := context.Background()

	_, err := runSetBasePlanState(ctx, client, SetBasePlanStateArgs{
		ProductID: "premium", BasePlanID: "monthly", State: "active",
	})
	if err == nil || !strings.Contains(err.Error(), "already ACTIVE") {
		t.Errorf("err = %v, want a no-op reported rather than staged", err)
	}

	_, err = runSetBasePlanState(ctx, client, SetBasePlanStateArgs{
		ProductID: "premium", BasePlanID: "weekly", State: "active",
	})
	// The API's own 404 names neither the plan nor what does exist.
	if err == nil || !strings.Contains(err.Error(), "monthly, yearly") {
		t.Errorf("err = %v, want it to list the base plans that do exist", err)
	}

	for _, args := range []SetBasePlanStateArgs{
		{BasePlanID: "monthly", State: "active"},
		{ProductID: "premium", State: "active"},
		{ProductID: "premium", BasePlanID: "monthly"},
		{ProductID: "premium", BasePlanID: "monthly", State: "on"},
	} {
		if _, err := runSetBasePlanState(ctx, client, args); err == nil {
			t.Errorf("args %+v should be rejected", args)
		}
	}
	if len(fake.posts) != 0 {
		t.Errorf("nothing may be sent while previewing: %v", fake.posts)
	}
}

func TestSetOfferStateAppliesOnOneConfirmation(t *testing.T) {
	isolateState(t)
	fake := subscriptionAPI()
	// The single-offer read is served from the offers store by id.
	fake.subscriptions["premium"] = subscriptionFixture
	client := newTestClient(t, fake.handler(t))
	ctx := context.Background()

	preview, err := runSetOfferState(ctx, client, SetOfferStateArgs{
		ProductID: "premium", BasePlanID: "monthly", OfferID: "intro", State: "inactive",
	})
	if err != nil {
		t.Fatalf("runSetOfferState: %v", err)
	}
	for _, want := range []string{"Deactivate offer intro", "base plan monthly", "ACTIVE → INACTIVE"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}

	applied, err := applyConfirmed(ctx, client, "set_offer_state", preview.ConfirmToken)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !applied.Applied {
		t.Fatal("an offer state change applies on the first confirmation")
	}
	const path = "/androidpublisher/v3/applications/com.example.app/subscriptions/premium/basePlans/monthly/offers/intro:deactivate"
	body, ok := fake.posts[path]
	if !ok {
		t.Fatalf("no deactivate was sent; posts = %v", fake.posts)
	}
	if !strings.Contains(body, `"offerId":"intro"`) {
		t.Errorf("request body missing the offer id: %s", body)
	}
}

// TestSetOfferStateWarnsAboutTheBasePlan: activating an offer under a plan that
// is off changes nothing a user can see.
func TestSetOfferStateWarnsAboutTheBasePlan(t *testing.T) {
	isolateState(t)
	fake := subscriptionAPI()
	client := newTestClient(t, fake.handler(t))

	preview, err := runSetOfferState(context.Background(), client, SetOfferStateArgs{
		ProductID: "premium", BasePlanID: "monthly", OfferID: "winter", State: "active",
	})
	if err != nil {
		t.Fatalf("runSetOfferState: %v", err)
	}
	if !strings.Contains(preview.Preview, "only live while its base plan is active") {
		t.Errorf("preview should state the dependency:\n%s", preview.Preview)
	}
}

// TestMonetizationTokensAreBoundToTheirTool: a preview staged by one tool must
// not be applicable through another.
func TestMonetizationTokensAreBoundToTheirTool(t *testing.T) {
	isolateState(t)
	fake := subscriptionAPI()
	client := newTestClient(t, fake.handler(t))
	ctx := context.Background()

	preview, err := runSetBasePlanState(ctx, client, SetBasePlanStateArgs{
		ProductID: "premium", BasePlanID: "yearly", State: "active",
	})
	if err != nil {
		t.Fatalf("runSetBasePlanState: %v", err)
	}
	if _, err := applyConfirmed(ctx, client, "set_offer_state", preview.ConfirmToken); err == nil {
		t.Fatal("a base plan token must not apply through the offer tool")
	}
	if len(fake.posts) != 0 {
		t.Errorf("nothing may be sent: %v", fake.posts)
	}
}

// TestDirectWritePreviewDoesNotPromiseValidation: the monetization resources
// are not edit-scoped. The preview's closing sentence is the last thing someone
// reads before confirming a price or availability change, so it must not
// promise that Play will validate the change first — for these writes, nothing
// does. Edit-scoped writes keep saying so, because for them it is true.
func TestDirectWritePreviewDoesNotPromiseValidation(t *testing.T) {
	isolateState(t)
	fake := subscriptionAPI()
	client := newTestClient(t, fake.handler(t))

	preview, err := runSetBasePlanState(context.Background(), client, SetBasePlanStateArgs{
		ProductID: "premium", BasePlanID: "yearly", State: "active",
	})
	if err != nil {
		t.Fatalf("runSetBasePlanState: %v", err)
	}
	if strings.Contains(preview.Preview, "opens a fresh edit") {
		t.Errorf("a non-edit-scoped write must not claim it is staged in an edit:\n%s", preview.Preview)
	}
	for _, want := range []string{"not edit-scoped", "takes effect immediately"} {
		if !strings.Contains(preview.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview.Preview)
		}
	}
}

func TestPlayApplyNoteDependsOnDispatch(t *testing.T) {
	direct := playApplyNote(dispatchDirect)
	if !strings.Contains(direct, "immediately") {
		t.Errorf("direct note = %q, want it to say the change is immediate", direct)
	}
	// Every other route runs inside an edit, which the API validates first.
	for _, dispatch := range []string{dispatchEdit, dispatchTrack, dispatchUpload, dispatchImages, dispatchListingSync} {
		note := playApplyNote(dispatch)
		if !strings.Contains(note, "validates it, and commits") {
			t.Errorf("note for %q = %q, want the edit sentence", dispatch, note)
		}
	}
}
