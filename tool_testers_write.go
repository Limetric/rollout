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

// SetTestersArgs replaces the tester groups on a track.
type SetTestersArgs struct {
	PackageName string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Track       string   `json:"track" jsonschema:"the testing track (internal, alpha, beta, or a custom closed-testing track)"`
	Groups      []string `json:"groups" jsonschema:"the Google Group addresses that may test this track; this replaces the current list"`

	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty" jsonschema:"commit without submitting the changes for review"`
	Confirm                 string `json:"confirm,omitempty" jsonschema:"a confirm token returned by a previous preview call; omit to preview"`
}

// runSetTesters stages or applies a tester-group change.
//
// The API replaces the whole list, so the preview diffs it: "set testers" that
// silently drops the three groups you did not mention is exactly the mistake a
// full-replacement endpoint invites.
func runSetTesters(ctx context.Context, c *Client, args SetTestersArgs) (WriteResult, error) {
	const tool = "set_testers"
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return WriteResult{}, err
	}
	if args.Confirm != "" {
		return applyConfirmed(ctx, c, tool, args.Confirm)
	}
	if args.Track == "" {
		return WriteResult{}, fmt.Errorf("track is required — pass --track internal")
	}
	groups, err := normalizeGoogleGroups(args.Groups)
	if err != nil {
		return WriteResult{}, err
	}

	current, err := runTesters(ctx, c, TestersArgs{PackageName: pkg, Track: args.Track})
	if err != nil {
		return WriteResult{}, err
	}

	body, err := json.Marshal(map[string]any{"googleGroups": groups})
	if err != nil {
		return WriteResult{}, err
	}
	return previewPlayWrite(stagePlayWriteRequest{
		Tool: tool, PackageName: pkg, Dispatch: dispatchEdit, Track: args.Track,
		Summary: fmt.Sprintf("Set the testers of %s: %s", args.Track, describeGroupDiff(current.GoogleGroups, groups)),
		Payload: editPayload{
			Requests: []editRequest{{
				Method: http.MethodPut, Path: "testers/" + args.Track, Body: body,
				Describe: "testers on " + args.Track,
			}},
			ChangesNotSentForReview: args.ChangesNotSentForReview,
		},
	})
}

// normalizeGoogleGroups validates and de-duplicates the group list.
//
// Only Google Groups work here: the Publisher API has no per-email tester list,
// so an address that is not a group is silently accepted by the API and then
// tests nothing. Rejecting the obvious cases up front is cheap.
func normalizeGoogleGroups(groups []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range groups {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !strings.Contains(part, "@") {
				return nil, fmt.Errorf("%q is not an email address — testers are Google Groups, for example qa@googlegroups.com", part)
			}
			if seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no groups given — pass --groups qa@googlegroups.com (an empty list would remove every tester; pass --groups \"\" is not enough to say that on purpose)")
	}
	sort.Strings(out)
	return out, nil
}

// describeGroupDiff renders what a full replacement actually changes.
func describeGroupDiff(before, after []string) string {
	added, removed := diffStrings(before, after)
	switch {
	case len(added) == 0 && len(removed) == 0:
		return "no change (" + strings.Join(after, ", ") + ")"
	case len(removed) == 0:
		return "adding " + strings.Join(added, ", ")
	case len(added) == 0:
		return "removing " + strings.Join(removed, ", ")
	default:
		return "adding " + strings.Join(added, ", ") + "; removing " + strings.Join(removed, ", ")
	}
}

// diffStrings reports what after adds and drops relative to before.
func diffStrings(before, after []string) (added, removed []string) {
	inBefore := map[string]bool{}
	for _, s := range before {
		inBefore[s] = true
	}
	inAfter := map[string]bool{}
	for _, s := range after {
		inAfter[s] = true
		if !inBefore[s] {
			added = append(added, s)
		}
	}
	for _, s := range before {
		if !inAfter[s] {
			removed = append(removed, s)
		}
	}
	return added, removed
}

// --- CLI front-end ---

var setTestersArgs SetTestersArgs

var setTestersCmd = &cobra.Command{
	Use:         "set",
	Short:       "Replace the Google Groups testing a track (previews first; --confirm to apply)",
	Annotations: mcpTool("set_testers"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayWrite(cmd, setTestersArgs, runSetTesters)
	},
}

func init() {
	addPackageFlag(setTestersCmd, &setTestersArgs.PackageName)
	setTestersCmd.Flags().StringVar(&setTestersArgs.Track, "track", "", "testing track (required)")
	setTestersCmd.Flags().StringArrayVar(&setTestersArgs.Groups, "groups", nil, "Google Group addresses (repeatable or comma-separated)")
	setTestersCmd.Flags().BoolVar(&setTestersArgs.ChangesNotSentForReview, "changes-not-sent-for-review", false, "commit without submitting the changes for review")
	addConfirmFlag(setTestersCmd, &setTestersArgs.Confirm)
	testersCmd.AddCommand(setTestersCmd)
}
