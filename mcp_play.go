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

	addTool(reg, client, "edit_status",
		"Check whether these credentials can actually publish to this app, by opening and immediately discarding an edit. Call this before staging a write: a credential that authenticates fine can still lack access to a given app.",
		runEditStatus)

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

	return nil
}
