package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// Shared wiring for the `rollout play …` tools. Every tool is one typed Args
// struct plus a pure handler; this file is what connects that handler to a
// Cobra subcommand, and what records the MCP name it is also served under.

// mcpAnnotation is the Cobra annotation key carrying a command's MCP tool name.
//
// The two front-ends name things differently on purpose. The CLI groups by
// noun so `rollout play release --help` shows everything you can do to a
// release; MCP tool names lead with the verb, because an agent scans a flat
// list of ~35 names and `play_halt_release` reads as an action where
// `play_release_halt` reads as a namespace. The mapping is therefore declared,
// not derived — and mcp_test.go fails the build when a command and its
// registration disagree.
const mcpAnnotation = "rollout.mcp_tool"

// mcpTool records which MCP tool a Cobra command shares its handler with.
func mcpTool(name string) map[string]string {
	return map[string]string{mcpAnnotation: name}
}

// runPlayRead builds the client, runs a read handler, and renders the result in
// the requested format. Every `rollout play` read subcommand is one call to it.
func runPlayRead[A, R any](cmd *cobra.Command, args A, format string, handler func(context.Context, *Client, A) (R, error)) error {
	client, err := newPlayClient(cmd.Context())
	if err != nil {
		return err
	}
	res, err := handler(cmd.Context(), client, args)
	if err != nil {
		return err
	}
	return printResult(cmd.OutOrStdout(), format, res)
}

// runPlayWrite builds the client, runs a write handler, and prints the result.
// Writes always print JSON: the interesting output is a preview and a confirm
// token, not a table.
func runPlayWrite[A any](cmd *cobra.Command, args A, handler func(context.Context, *Client, A) (WriteResult, error)) error {
	client, err := newPlayClient(cmd.Context())
	if err != nil {
		return err
	}
	res, err := handler(cmd.Context(), client, args)
	if err != nil {
		return err
	}
	if err := printJSON(cmd.OutOrStdout(), res); err != nil {
		return err
	}
	// The next step goes to stderr so stdout stays valid JSON for jq pipelines.
	if !res.Applied && res.ConfirmToken != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "\n%s", res.Preview)
	}
	return nil
}

// addPackageFlag registers the --package flag every Play tool accepts.
func addPackageFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "package", "", "Android package name (falls back to the configured default)")
}

// jsonRow marshals a value into a table row. Read results carry the API's own
// JSON so agents see the wire format; the table renderer reads dotted paths out
// of these rows.
func jsonRow(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// jsonRows marshals a slice of values into table rows.
func jsonRows[T any](values []T) []json.RawMessage {
	rows := make([]json.RawMessage, 0, len(values))
	for _, v := range values {
		rows = append(rows, jsonRow(v))
	}
	return rows
}

// addConfirmFlag registers the --confirm flag every Play write tool accepts.
func addConfirmFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "confirm", "", "confirm token from a previous preview")
}
