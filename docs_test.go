package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// declaredTools collects every MCP tool the CLI declares. mcp_test.go proves
// this set equals what the MCP server registers, so documenting against it
// documents the served surface.
func declaredTools(t *testing.T) map[string]bool {
	t.Helper()
	tools := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if name := cmd.Annotations[mcpAnnotation]; name != "" {
			tools[playPlatformName+"_"+name] = true
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	for _, cmd := range playPlatform.Commands {
		walk(cmd)
	}
	return tools
}

var toolMentionPattern = regexp.MustCompile(`play_[a-z_]+`)

// mentionedTools finds every `play_*` name a document refers to.
func mentionedTools(t *testing.T, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	found := map[string]bool{}
	for _, match := range toolMentionPattern.FindAllString(string(data), -1) {
		found[match] = true
	}
	return found
}

// TestDocsCoverEveryTool is the acceptance check for the docs: a tool that is
// served but undocumented is a tool nobody will find, and a documented tool
// that does not exist is worse — it sends someone to run a command that fails.
//
// goads keeps these tables in sync by hand; a test is cheaper.
func TestDocsCoverEveryTool(t *testing.T) {
	declared := declaredTools(t)
	if len(declared) == 0 {
		t.Fatal("no tools declared — the walk is broken, not the docs")
	}

	for _, doc := range []string{"docs/play.md", "docs/name-map.md"} {
		t.Run(doc, func(t *testing.T) {
			mentioned := mentionedTools(t, doc)

			var missing []string
			for tool := range declared {
				if !mentioned[tool] {
					missing = append(missing, tool)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s does not mention: %s", doc, strings.Join(missing, ", "))
			}

			var unknown []string
			for tool := range mentioned {
				if !declared[tool] {
					unknown = append(unknown, tool)
				}
			}
			sort.Strings(unknown)
			if len(unknown) > 0 {
				t.Errorf("%s documents tools that do not exist: %s", doc, strings.Join(unknown, ", "))
			}
		})
	}
}

// TestNameMapDocumentsEveryCLIPath: the map's whole purpose is answering "what
// is this tool called on the other side", which needs both names present.
func TestNameMapDocumentsEveryCLIPath(t *testing.T) {
	data, err := os.ReadFile("docs/name-map.md")
	if err != nil {
		t.Fatalf("read name map: %v", err)
	}
	body := string(data)

	var walk func(*cobra.Command, string)
	walk = func(cmd *cobra.Command, path string) {
		if cmd.Annotations[mcpAnnotation] != "" {
			if !strings.Contains(body, path) {
				t.Errorf("docs/name-map.md does not document `%s`", path)
			}
		}
		for _, sub := range cmd.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			walk(sub, path+" "+sub.Name())
		}
	}
	for _, cmd := range playPlatform.Commands {
		walk(cmd, "rollout play "+cmd.Name())
	}
}

// TestSkillListsEveryCommand keeps the agent skill honest: a command it does not
// know about is one it will never reach for, and one it invents is one that
// fails in front of a user.
func TestSkillListsEveryCommand(t *testing.T) {
	data, err := os.ReadFile("plugins/rollout/skills/rollout/SKILL.md")
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	body := string(data)

	var walk func(*cobra.Command, string)
	walk = func(cmd *cobra.Command, path string) {
		if cmd.Annotations[mcpAnnotation] != "" && !strings.Contains(body, "`"+path+"`") {
			t.Errorf("SKILL.md does not list the `%s` command", path)
		}
		for _, sub := range cmd.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			walk(sub, path+" "+sub.Name())
		}
	}
	for _, cmd := range playPlatform.Commands {
		walk(cmd, cmd.Name())
	}
}

// TestDocsReferenceRealFiles catches a link to a document that was renamed or
// never written.
func TestDocsReferenceRealFiles(t *testing.T) {
	linkPattern := regexp.MustCompile(`\]\((docs/[a-z-]+\.md|[a-z-]+\.md)[^)]*\)`)
	for _, doc := range []string{"README.md", "docs/play.md", "docs/reporting.md", "docs/name-map.md"} {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		dir := "."
		if strings.Contains(doc, "/") {
			dir = doc[:strings.LastIndex(doc, "/")]
		}
		for _, match := range linkPattern.FindAllStringSubmatch(string(data), -1) {
			target := match[1]
			candidates := []string{target, dir + "/" + target}
			var found bool
			for _, candidate := range candidates {
				if _, err := os.Stat(candidate); err == nil {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s links to %s, which does not exist", doc, target)
			}
		}
	}
}
