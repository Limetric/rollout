package main

import (
	"context"
)

// Every Google Play MCP tool registration lives here.
//
// Two rules the tests enforce:
//
//   - Names are written unprefixed. The registrar applies `play_`, so a tool
//     cannot leak an unnamespaced name by someone forgetting the prefix, and it
//     cannot double it up by remembering too hard.
//   - This list and playPlatform.Commands describe the same set. A tool that
//     exists on only one front-end is a tool an agent or a script will ask for
//     and not find; mcp_test.go compares the two by name.

// registerPlayTools registers every Play tool on the MCP server.
//
// The client is built once, here, rather than per tool call: it resolves
// credentials, and doing that at registration time is what lets a platform
// nobody has configured be skipped with a warning instead of failing every
// tool call at the point of use.
func registerPlayTools(ctx context.Context, reg *toolRegistrar) error {
	client, err := newPlayClient(ctx)
	if err != nil {
		return err
	}
	// --- reads ---

	addTool(reg, client, "apps",
		"List the Google Play apps these credentials can reach. Start here: the returned package_name is what every other play_ tool takes, and the app marked \"default\" is the one used when package_name is omitted.",
		runApps)

	addTool(reg, client, "tracks",
		"Show an app's release tracks (production, beta, alpha, internal, custom closed tests) with the releases in each: status, version codes, staged-rollout percentage, and which locales have release notes.",
		runTracks)

	addTool(reg, client, "releases",
		"List every release across all tracks as a flat table — the quickest answer to \"what version is live where\".",
		runReleases)

	addTool(reg, client, "artifacts",
		"List the app bundles and APKs uploaded for an app, newest version code first, with their SHA-256 hashes.",
		runArtifacts)

	addTool(reg, client, "listing",
		"Read store listing text per locale: title, short description, full description, and promo video URL.",
		runListing)

	addTool(reg, client, "images",
		"List the store listing images for one locale — icon, feature graphic, and screenshots — with the SHA-256 of each, so unchanged images can be recognized without downloading them.",
		runImages)

	addTool(reg, client, "details",
		"Read app-level details: the default listing language and the developer contact website, email, and phone shown on the store page.",
		runDetails)

	addTool(reg, client, "testers",
		"List the Google Groups testing a track. Note that the Play API exposes group-based testers only; individual tester emails exist solely in the Console.",
		runTesters)

	addTool(reg, client, "countries",
		"Show the countries a track is available in, and whether it simply follows production.",
		runCountries)

	addTool(reg, client, "device_tiers",
		"List the app's device tier configurations (the RAM and device-feature tiers used for targeted delivery).",
		runDeviceTiers)

	addTool(reg, client, "users",
		"List the users on the Play Console developer account: their access state, when their access expires, the account-wide permissions they hold, and their per-app grants. Needs the developer account id (the number in the Console URL).",
		runUsers)

	addTool(reg, client, "edit_status",
		"Check whether these credentials can actually publish to this app, by opening and immediately discarding an edit. Call this before staging a write: a credential that authenticates fine can still lack access to a given app.",
		runEditStatus)

	// --- monetization ---

	addTool(reg, client, "products",
		"List an app's in-app products: legacy managed products and the newer one-time products, merged into one list with each row naming the API it came from. Subscriptions are deliberately not here — they have base plans and offers that this shape cannot represent, so read them with play_subscriptions.",
		runProducts)

	addTool(reg, client, "subscriptions",
		"List subscriptions with their base plans and offers inlined. A base plan is the price and billing period; an offer is a temporary promotion extending one. The state on each (DRAFT, ACTIVE, INACTIVE) is what decides whether new subscribers can sign up.",
		runSubscriptions)

	addTool(reg, client, "subscription",
		"Read one subscription by product ID, with its base plans, their regional availability, and every offer hanging off them.",
		runSubscription)

	addTool(reg, client, "update_product",
		"Create or update a managed in-app product. The preview diffs prices region by region, including the ones a write would REMOVE: the API replaces the whole price map rather than merging it, so a region missing from the file loses its price. Previews first and returns a confirm token.",
		runUpdateProduct)

	addTool(reg, client, "set_base_plan_state",
		"Activate or deactivate a subscription's base plan. Deactivating stops new subscribers only — everybody already on the plan keeps their subscription and keeps being billed — and takes two confirmations, because every offer under the base plan goes with it.",
		runSetBasePlanState)

	addTool(reg, client, "set_offer_state",
		"Activate or deactivate a subscription offer. An offer is only live while its base plan is active too. Previews first and returns a confirm token.",
		runSetOfferState)

	// --- release management writes ---

	addTool(reg, client, "upload_artifact",
		"Upload an .aab or .apk and add it to a track. Previews first and returns a confirm token; nothing is uploaded until you call again with it. Defaults to the internal track as a draft release, so uploading a build and shipping it stay separate decisions.",
		runUploadArtifact)

	addTool(reg, client, "create_release",
		"Create a track release from version codes that are already uploaded (see play_artifacts). Previews first and returns a confirm token. Other releases in the track — including an in-progress rollout — are left untouched.",
		runCreateRelease)

	addTool(reg, client, "update_release",
		"Change one release: this is the staged-rollout dial (rollout 0.1 → 0.25 → 0.5), and also how release notes, name, and in-app update priority are changed. Previews first and returns a confirm token.",
		runUpdateRelease)

	addTool(reg, client, "halt_release",
		"Halt a staged rollout, stopping delivery to users who do not already have it. Destructive: takes two confirmations, and resuming later does not undo the time it was gone.",
		runHaltRelease)

	addTool(reg, client, "resume_release",
		"Resume a halted rollout, optionally at a different fraction. Previews first and returns a confirm token.",
		runResumeRelease)

	addTool(reg, client, "complete_release",
		"Roll a release out to every user. On the production track this takes two confirmations: there is no lower fraction to fall back to.",
		runCompleteRelease)

	addTool(reg, client, "promote_release",
		"Promote a release from one track to another, carrying its release notes, name, and update priority across. Defaults to a draft on production and completed elsewhere. Previews first and returns a confirm token.",
		runPromoteRelease)

	addTool(reg, client, "set_testers",
		"Replace the Google Groups that may test a track. The Play API takes group addresses only — individual tester emails exist solely in the Console. The preview diffs the current list against the new one, because this call replaces rather than adds.",
		runSetTesters)

	addTool(reg, client, "set_countries",
		"Set the countries a track's release ships to. Play scopes availability per release, so this patches the release's country targeting. Previews first and returns a confirm token.",
		runSetCountries)

	addTool(reg, client, "upload_deobfuscation",
		"Attach a ProGuard/R8 mapping file or native debug symbols to an uploaded version code, so crash stack traces in Android vitals are readable. Previews first and returns a confirm token.",
		runUploadDeobfuscation)

	// --- store listing writes ---

	addTool(reg, client, "update_listing",
		"Set store listing text for one locale — title (30 chars), short description (80), full description (4000), promo video. The preview diffs each field against what is live. Fields you do not pass are carried over, not blanked.",
		runUpdateListing)

	addTool(reg, client, "delete_listing",
		"Delete a locale's store listing, or every localized listing. Destructive: takes two confirmations, and the API cannot bring the text back.",
		runDeleteListing)

	addTool(reg, client, "upload_images",
		"Upload store listing images for one locale and type. The preview reports each file's real dimensions and warns where Play's constraints are not met (icon 512×512, feature graphic 1024×500, at most 8 screenshots per type).",
		runUploadImages)

	addTool(reg, client, "delete_images",
		"Delete store listing images by id, or every image of one type in a locale. Deleting a whole type takes two confirmations.",
		runDeleteImages)

	addTool(reg, client, "update_details",
		"Set app-level details: the default listing language and the developer contact website, email, and phone shown on the store page.",
		runUpdateDetails)

	addTool(reg, client, "sync_listing",
		"Reconcile a local metadata directory (the fastlane supply layout) with the store: per-locale text and, with images enabled, screenshots and graphics compared by SHA-256 so unchanged files are never re-uploaded. The whole plan is previewed and then applied in one edit — if any locale fails, none of it lands.",
		runSyncListing)

	// --- app-level utilities ---

	addTool(reg, client, "internal_share",
		"Upload an .aab or .apk to internal app sharing and get a link it can be installed from. It joins no track, ships to nobody, and is never reviewed — but the link is real, and the Publisher API cannot withdraw it, so this previews first.",
		runInternalShare)

	addTool(reg, client, "create_device_tier_config",
		"Create a device tier configuration from a JSON file holding a DeviceTierConfig — the RAM and device-feature tiers targeted delivery selects on. Configurations are append-only: this never edits an existing one. Previews first and returns a confirm token.",
		runCreateDeviceTierConfig)

	addTool(reg, client, "update_data_safety",
		"Replace the app's Data safety declaration with the CSV exported from Play Console. Play offers no read for this form, so what is live cannot be shown or restored — the preview says so before the token is handed out.",
		runUpdateDataSafety)

	// --- developer account users and permissions ---

	addTool(reg, client, "invite_user",
		"Invite someone to the developer account, optionally granting them access to named apps in the same call. The preview spells out every permission enum it would send, because these are the arguments nobody can check by reading them back.",
		runInviteUser)

	addTool(reg, client, "set_grant",
		"Set what one user may do with one app. This replaces their permissions on that app rather than adding to them, so the preview diffs the current list against the new one.",
		runSetGrant)

	addTool(reg, client, "revoke_grant",
		"Take away one user's access to one app. Destructive: takes two confirmations, and the API cannot say afterwards what the grant held — the preview names it. Their account-wide permissions are untouched.",
		runRevokeGrant)

	addTool(reg, client, "remove_user",
		"Remove a user from the developer account, dropping every grant they hold at once. Destructive: takes two confirmations, and the preview is the only record of what they could do.",
		runRemoveUser)

	// --- reviews ---

	addTool(reg, client, "reviews",
		"List user reviews with their existing developer replies. Two limits are the API's, not this tool's: Play returns reviews from the last week only, and only for apps that have a live production release — so an empty result is not evidence of no complaints. Star rating, version code, age, and answered-ness are filtered client-side.",
		runReviews)

	addTool(reg, client, "review",
		"Read one review by id, optionally translated.",
		runReview)

	addTool(reg, client, "reply_review",
		"Reply publicly to a review, at most 350 characters. The preview quotes the review being answered, because the failure worth preventing is replying to the wrong one — and unlike a release, a reply is not staged in an edit: it is live the moment it is confirmed.",
		runReplyReview)

	// --- reporting (Android vitals) ---

	addTool(reg, client, "vitals",
		"Query an Android vitals metric set — crash rate, ANR rate, error counts, excessive wakeups, stuck wakelocks, slow rendering, slow starts, or low-memory kills — over a date range, optionally broken down by dimensions such as versionCode or countryCode. Data lags real time by up to a few days, and versions with too few users are withheld for privacy.",
		runVitals)

	addTool(reg, client, "vitals_summary",
		"Check whether an app is healthy enough to keep rolling out: the worst crash and ANR rate in the window against Play's bad-behaviour thresholds (1.09% and 0.47%), with an ok/warn status per metric and a single ok flag to gate on. An unknown rate reports as not-ok — it is a reason to look, not to ship.",
		runVitalsSummary)

	addTool(reg, client, "error_issues",
		"List crash and ANR issue clusters, most-users-affected first, with report counts, distinct users, the failing cause and location, and the version range they span.",
		runErrorIssues)

	addTool(reg, client, "error_reports",
		"List the individual reports behind one issue cluster — this is where the stack traces, device models, and OS versions are.",
		runErrorReports)

	addTool(reg, client, "anomalies",
		"List the metric anomalies Play itself has flagged. An empty list means nothing crossed Play's own detection thresholds, which is not the same as the app being healthy — use play_vitals_summary for that.",
		runAnomalies)

	// --- CSV report exports (Cloud Storage) ---

	addTool(reg, client, "reports_list",
		"List the monthly CSV reports Play has exported to the reports bucket — installs, ratings, crashes, store performance, subscriptions, reviews, sales, and earnings. Start here to see which months exist; the object names it returns are what play_report takes.",
		runReportsList)

	addTool(reg, client, "report",
		"Download and parse one exported CSV report, by kind and month or by exact object name. These numbers exist in no Play API — this bucket is the only source for installs, ratings, store performance, and the financial reports. Rows are capped, and the result says how many the file held; narrow the dimension or raise max_rows for more.",
		runReport)

	addTool(reg, client, "installs",
		"Daily installs, uninstalls, upgrades and active devices over a trailing window, stitched from the monthly exports. Days that have not been exported yet are listed as missing rather than reported as zero.",
		runInstalls)

	addTool(reg, client, "ratings",
		"Daily and cumulative average star rating over a trailing window. Days that have not been exported yet are listed as missing rather than reported as zero.",
		runRatings)

	return nil
}
