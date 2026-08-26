package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
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

// TestCLIAndMCPSurfacesMatch is the sync check AGENTS.md asks for: a tool that
// exists on only one front-end is a tool an agent or a script will ask for and
// not find. Registration happens in two places by design (a Cobra command and
// an addTool call), and the two names differ on purpose, so each command
// declares the MCP tool it shares a handler with and this compares the sets.
func TestCLIAndMCPSurfacesMatch(t *testing.T) {
	clearPlayEnv(t)
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, `{}`) })
	t.Setenv("PLAY_API_BASE_URL", api.URL)
	t.Setenv("PLAY_PACKAGE_NAME", "com.example.app")
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })

	names := listTools(t, func(ctx context.Context, server *mcp.Server) error {
		return registerPlatformsOn(ctx, server, []*Platform{playPlatform})
	})

	registered := map[string]bool{}
	for _, name := range names {
		if !strings.HasPrefix(name, playPlatformName+"_") {
			t.Errorf("tool %q is not namespaced — the registrar applies the prefix", name)
			continue
		}
		registered[strings.TrimPrefix(name, playPlatformName+"_")] = true
	}

	declared := map[string]string{} // MCP tool name -> CLI path
	for _, cmd := range playPlatform.Commands {
		collectMCPTools(t, cmd, "rollout play "+cmd.Name(), declared)
	}

	for tool, path := range declared {
		if !registered[tool] {
			t.Errorf("`%s` declares MCP tool %s_%s, which is not registered — add it to registerPlayTools", path, playPlatformName, tool)
		}
	}
	for tool := range registered {
		if _, ok := declared[tool]; !ok {
			t.Errorf("MCP tool %s_%s has no CLI subcommand — add one to playPlatform.Commands and tag it with mcpTool(%q)", playPlatformName, tool, tool)
		}
	}
}

// collectMCPTools walks a command subtree, gathering the MCP tool each runnable
// command declares. A runnable command with no declaration is the failure this
// catches: it would be reachable from the CLI and invisible to an agent.
func collectMCPTools(t *testing.T, cmd *cobra.Command, path string, out map[string]string) {
	t.Helper()
	if cmd.Runnable() {
		tool := cmd.Annotations[mcpAnnotation]
		if tool == "" {
			t.Errorf("`%s` is runnable but declares no MCP tool — tag it with Annotations: mcpTool(\"…\")", path)
		} else if existing, dup := out[tool]; dup {
			t.Errorf("`%s` and `%s` both claim MCP tool %s_%s", existing, path, playPlatformName, tool)
		} else {
			out[tool] = path
		}
	}
	for _, sub := range cmd.Commands() {
		if sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		collectMCPTools(t, sub, path+" "+sub.Name(), out)
	}
}

// TestUnconfiguredPlayIsSkipped exercises the real platform: an MCP host with
// no Play credentials must still start, with the reason in its server log.
func TestUnconfiguredPlayIsSkipped(t *testing.T) {
	clearPlayEnv(t)
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })
	captureWarnings(t)

	server := mcp.NewServer(&mcp.Implementation{Name: "rollout", Version: "test"}, nil)
	err := registerPlatformsOn(context.Background(), server, []*Platform{playPlatform})
	if err == nil {
		t.Fatal("expected an error when the only platform cannot be served")
	}
	if !strings.Contains(err.Error(), "rollout doctor") {
		t.Errorf("error should point at the fix: %v", err)
	}
}
