package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// listTools spins up an MCP server, registers tools through the given
// callback, and lists what a host would see.
func listTools(t *testing.T, register func(ctx context.Context, server *mcp.Server) error) []string {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "rollout", Version: "test"}, nil)
	if err := register(ctx, server); err != nil {
		t.Fatalf("register: %v", err)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	session, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, len(res.Tools))
	for i, tool := range res.Tools {
		names[i] = tool.Name
	}
	return names
}

type stubArgs struct {
	Track string `json:"track" jsonschema:"the track to read"`
}

type stubResult struct {
	Track  string `json:"track"`
	Source string `json:"source"`
}

// TestRegistrarAppliesPlatformPrefix is the rule AGENTS.md states: tool names
// are written unprefixed at the registration site, and the registrar namespaces
// them. A hand-written prefix would double up.
func TestRegistrarAppliesPlatformPrefix(t *testing.T) {
	names := listTools(t, func(ctx context.Context, server *mcp.Server) error {
		reg := &toolRegistrar{server: server, prefix: playPlatformName + "_"}
		addTool(reg, struct{}{}, "tracks", "List tracks",
			func(_ context.Context, _ struct{}, args stubArgs) (stubResult, error) {
				return stubResult{Track: args.Track, Source: "handler"}, nil
			})
		return nil
	})

	if len(names) != 1 || names[0] != "play_tracks" {
		t.Fatalf("registered tools = %v, want [play_tracks]", names)
	}
}

// TestUnconfiguredPlatformIsSkippedNotFatal: an MCP host must still get the
// platforms that do work. A second, unconfigured store cannot take the server
// down with it.
func TestUnconfiguredPlatformIsSkippedNotFatal(t *testing.T) {
	working := &Platform{
		Name: "working", Title: "Working Store",
		RegisterMCP: func(_ context.Context, reg *toolRegistrar) error {
			addTool(reg, struct{}{}, "tracks", "List tracks",
				func(_ context.Context, _ struct{}, _ stubArgs) (stubResult, error) { return stubResult{}, nil })
			return nil
		},
	}
	broken := &Platform{
		Name: "broken", Title: "Broken Store",
		RegisterMCP: func(context.Context, *toolRegistrar) error {
			return errors.New("no credentials")
		},
	}

	names := listTools(t, func(ctx context.Context, server *mcp.Server) error {
		return registerPlatformsOn(ctx, server, []*Platform{working, broken})
	})
	if len(names) != 1 || names[0] != "working_tracks" {
		t.Fatalf("registered tools = %v, want only the working platform's", names)
	}
}

// TestNoServablePlatformIsAnError: an MCP host cannot surface an empty tool
// list as a setup problem, so that case has to fail loudly.
func TestNoServablePlatformIsAnError(t *testing.T) {
	broken := &Platform{
		Name: "broken", Title: "Broken Store",
		RegisterMCP: func(context.Context, *toolRegistrar) error { return errors.New("no credentials") },
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "rollout", Version: "test"}, nil)
	err := registerPlatformsOn(context.Background(), server, []*Platform{broken})
	if err == nil {
		t.Fatal("expected an error when no platform could be served")
	}
	if !strings.Contains(err.Error(), "rollout doctor") {
		t.Errorf("error should point at the fix: %v", err)
	}
}

// TestToolResultCarriesTextAndStructuredContent: hosts read one or the other,
// so a tool has to return both.
func TestToolResultCarriesTextAndStructuredContent(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "rollout", Version: "test"}, nil)
	reg := &toolRegistrar{server: server, prefix: playPlatformName + "_"}
	addTool(reg, struct{}{}, "tracks", "List tracks",
		func(_ context.Context, _ struct{}, args stubArgs) (stubResult, error) {
			return stubResult{Track: args.Track, Source: "handler"}, nil
		})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	session, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer session.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "play_tracks",
		Arguments: map[string]any{"track": "production"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("tool returned no text content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", res.Content[0])
	}
	var decoded stubResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatalf("text content is not JSON: %v", err)
	}
	if decoded.Track != "production" {
		t.Errorf("text content = %+v, want the handler's result", decoded)
	}
	if res.StructuredContent == nil {
		t.Error("tool returned no structured content")
	}
}

func TestToolErrorNamesTheTool(t *testing.T) {
	err := toolError("play_tracks", errors.New("boom"))
	if !strings.Contains(err.Error(), "play_tracks") {
		t.Errorf("error should name the tool: %v", err)
	}
}
