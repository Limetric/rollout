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
	_ = client
	// Tools land here as the phase-2 issues do — read tools first, then the
	// release, listing, review, and vitals writes. Each one is a single
	// addTool(reg, client, "<name>", "<description>", handler) call, and its
	// CLI subcommand goes into playPlatform.Commands.
	return nil
}
