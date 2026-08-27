package main

import (
	"context"

	"github.com/spf13/cobra"
)

// playPlatformName is Google Play's namespace. It is a constant rather than a
// read of playPlatform.Name so the write path can stamp it on a staged
// mutation without the package-level initializers forming a cycle (the platform
// value refers to the commands, which refer back to the write path).
const playPlatformName = "play"

// playPlatform is the Google Play surface: `rollout play …` on the CLI and
// `play_…` over MCP.
//
// Registration happens in a package-level variable initializer, which Go runs
// before any init() function — so main.go's init() sees this platform no matter
// how the files sort.
var playPlatform = registerPlatform(&Platform{
	Name:  playPlatformName,
	Title: "Google Play",
	Short: "Google Play Console release and listing management",

	// Keep this list in sync with registerPlayTools in mcp_play.go — every tool
	// is exposed both ways, backed by the same handler.
	Commands: []*cobra.Command{
		// Reads.
		appsCmd,
		tracksCmd,
		releasesCmd,
		artifactsCmd,
		listingCmd,
		imagesCmd,
		detailsCmd,
		testersCmd,
		countriesCmd,
		deviceTiersCmd,
		usersCmd,
		editCmd,

		// Monetization.
		productsCmd,
		productCmd,
		subscriptionsCmd,
		subscriptionCmd,

		// Release management writes.
		uploadCmd,
		releaseCmd,
		promoteCmd,
		deobfuscationCmd,

		// App-level utilities.
		internalShareCmd,
		dataSafetyCmd,

		// Developer account users and permissions.
		userCmd,

		// Reviews.
		reviewsCmd,
		reviewCmd,

		// Reporting.
		vitalsCmd,
		errorsCmd,
		errorCmd,
		anomaliesCmd,
		reportsCmd,
	},

	Login:          playLoginCmd,
	ConfigCommands: []*cobra.Command{playSetPackageCmd, playSetDeveloperIDCmd, playSetReportsBucketCmd},

	RegisterMCP: registerPlayTools,
	NewApplier:  func(ctx context.Context) (mutationApplier, error) { return newPlayClient(ctx) },
	ShowConfig:  playShowConfig,
	Doctor:      playDoctor,
	Configured:  playConfigured,
})
