package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Play's half of the write path: what a staged write carries, and where a
// confirmed one lands. Everything before and after — the confirm token, the
// TTL, double confirmation, the guard rails, the audit line — is shared (see
// safety.go, write_tool.go, and guards.go), so a second platform supplies only
// this file's equivalent.

// Dispatch routes a confirmed Play write.
const (
	// dispatchEdit runs an ordered list of edit-scoped REST calls inside one
	// edit. Store listings, images, app details, testers, and country
	// availability are all this shape: address a resource under the edit and
	// PUT, PATCH, or DELETE it.
	dispatchEdit = "edit"
	// dispatchTrack is a release write. It is separate from dispatchEdit
	// because it must read the track inside the edit before writing it — see
	// applyTrackWrite.
	dispatchTrack = "track"
	// dispatchDirect is a write that is not edit-scoped at all. Replying to a
	// review is the one that exists today: reviews live outside the publishing
	// transaction, so wrapping one in an edit would open and commit an empty
	// edit for no reason.
	dispatchDirect = "direct"
)

// editRequest is one REST call inside an edit, addressed relative to the edit
// itself ("listings/en-US", "tracks/production").
type editRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
	// Describe names what this call does, for the error message when it is the
	// one that fails. A multi-locale listing sync that fails must say which
	// locale, and the path alone does not read as an explanation.
	Describe string `json:"describe,omitempty"`
}

// editPayload is the staged intent for dispatchEdit and dispatchDirect.
type editPayload struct {
	Requests []editRequest `json:"requests"`
	// ChangesNotSentForReview commits without queueing the changes for review.
	// Google requires it for some apps and rejects it for others, so it is
	// decided by the staging tool rather than defaulted here.
	ChangesNotSentForReview bool `json:"changes_not_sent_for_review,omitempty"`
}

// trackPayload is the staged intent for a release write: which track, and what
// the one release being touched should look like afterwards.
//
// It names the release rather than carrying the whole track, because a track
// holds several releases at once — a completed one, an in-progress staged
// rollout, a draft — and writing the whole array is how in-progress rollouts
// get silently dropped.
type trackPayload struct {
	Track string `json:"track"`
	// Release is the release to insert or replace, as the API's Track.Release
	// shape. It is matched against the existing releases by version code.
	Release json.RawMessage `json:"release"`
	// RemoveOtherDrafts drops draft releases that the new one supersedes.
	// Completed and in-progress releases are never removed by this.
	RemoveOtherDrafts       bool `json:"remove_other_drafts,omitempty"`
	ChangesNotSentForReview bool `json:"changes_not_sent_for_review,omitempty"`
}

// track is the API's Track resource.
type track struct {
	Track    string            `json:"track"`
	Releases []json.RawMessage `json:"releases"`
}

// stagePlayWriteRequest is what a Play write tool hands to the staging helper.
type stagePlayWriteRequest struct {
	Tool        string
	PackageName string
	Summary     string
	Dispatch    string
	Payload     any
	// Track and RolloutFraction declare the facts the guard rails evaluate.
	Track           string
	RolloutFraction *float64
	// RequiresDouble forces a second confirmation for a write the guard rails
	// would not catch by name.
	RequiresDouble bool
}

// previewPlayWrite stages a Play write and returns its preview. Every Play
// write tool goes through here: it is what guarantees the platform is stamped
// and the guard rails see the write before a token exists.
func previewPlayWrite(req stagePlayWriteRequest) (WriteResult, error) {
	payload, err := json.Marshal(req.Payload)
	if err != nil {
		return WriteResult{}, fmt.Errorf("encode the staged %s payload: %w", req.Tool, err)
	}
	p, err := stageWrite(pendingWrite{
		Platform:        playPlatformName,
		Tool:            req.Tool,
		PackageName:     req.PackageName,
		Summary:         req.Summary,
		Dispatch:        req.Dispatch,
		Payload:         payload,
		Track:           req.Track,
		RolloutFraction: req.RolloutFraction,
		RequiresDouble:  req.RequiresDouble,
	})
	if err != nil {
		return WriteResult{}, err
	}
	return previewResult(p), nil
}

// applyMutation makes *Client a mutationApplier: it executes a consumed pending
// write through the route its Dispatch selects.
//
// The staged record describes the *intent*, never an open edit — edits expire
// and a token may be confirmed from another process, so the whole
// insert → mutate → validate → commit sequence happens here, inside the
// confirm call, and any failure along the way deletes the edit (see edit.go).
func (c *Client) applyMutation(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	switch p.Dispatch {
	case dispatchTrack:
		return c.applyTrackWrite(ctx, p)
	case dispatchDirect:
		return c.applyDirectWrite(ctx, p)
	case dispatchEdit:
		return c.applyEditWrite(ctx, p)
	default:
		return nil, fmt.Errorf("staged write %q has an unknown dispatch %q — this token was written by a different version of rollout; re-run the original command", p.Tool, p.Dispatch)
	}
}

// applyEditWrite runs an ordered list of edit-scoped calls inside one edit.
//
// Partial success is not success. The calls run inside a single edit, so a
// failure part-way through aborts the whole transaction and nothing commits —
// which is what makes a multi-locale listing sync all-or-nothing rather than
// leaving half the store listings updated.
func (c *Client) applyEditWrite(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	var payload editPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return nil, fmt.Errorf("corrupt staged payload for %s: %w", p.Tool, err)
	}
	if len(payload.Requests) == 0 {
		return nil, fmt.Errorf("staged write %s has no operations", p.Tool)
	}

	var results []json.RawMessage
	editID, err := c.withEdit(ctx, p.PackageName, true,
		commitOptions{ChangesNotSentForReview: payload.ChangesNotSentForReview},
		func(e *editSession) error {
			for _, req := range payload.Requests {
				var out json.RawMessage
				if err := e.run(ctx, req, &out); err != nil {
					return err
				}
				if len(out) > 0 {
					results = append(results, out)
				}
			}
			return nil
		})
	if err != nil {
		return &applyOutcome{EditID: editID}, err
	}
	return &applyOutcome{EditID: editID, Detail: p.Summary, Results: results}, nil
}

// run issues one staged request inside this edit, naming what it was doing when
// it fails.
func (e *editSession) run(ctx context.Context, req editRequest, out *json.RawMessage) error {
	var body any
	if len(req.Body) > 0 {
		body = req.Body
	}
	err := e.c.doWrite(ctx, req.Method, e.path(req.Path), nil, body, out)
	if err == nil {
		return nil
	}
	if req.Describe != "" {
		// Naming the failing item is the difference between "the sync failed"
		// and "the sync failed on de-DE" — and nothing committed either way.
		return fmt.Errorf("%s: %w", req.Describe, err)
	}
	return err
}

// applyTrackWrite updates exactly one release in a track, leaving every other
// release alone.
//
// This is the rule the whole file exists for. A track holds several releases at
// once — a completed one still serving most users, an in-progress staged
// rollout, a draft — and the API's tracks.update replaces the entire releases[]
// array. Writing the array a tool assembled from its own arguments is how an
// in-progress rollout silently disappears. So the track is read *inside the
// edit*, the one release with a matching version code is replaced, and
// everything else is written back exactly as it came.
func (c *Client) applyTrackWrite(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	var payload trackPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return nil, fmt.Errorf("corrupt staged payload for %s: %w", p.Tool, err)
	}
	if payload.Track == "" || len(payload.Release) == 0 {
		return nil, fmt.Errorf("staged write %s names no track or release", p.Tool)
	}

	var result json.RawMessage
	editID, err := c.withEdit(ctx, p.PackageName, true,
		commitOptions{ChangesNotSentForReview: payload.ChangesNotSentForReview},
		func(e *editSession) error {
			var current track
			if err := e.c.do(ctx, http.MethodGet, e.path("tracks/"+payload.Track), nil, nil, &current); err != nil {
				return fmt.Errorf("read track %s: %w", payload.Track, err)
			}
			merged, err := mergeRelease(current.Releases, payload.Release, payload.RemoveOtherDrafts)
			if err != nil {
				return err
			}
			body := track{Track: payload.Track, Releases: merged}
			return e.c.doWrite(ctx, http.MethodPut, e.path("tracks/"+payload.Track), nil, body, &result)
		})
	if err != nil {
		return &applyOutcome{EditID: editID}, err
	}
	return &applyOutcome{EditID: editID, Detail: p.Summary, Results: []json.RawMessage{result}}, nil
}

// mergeRelease replaces the release carrying the same version codes and keeps
// every other one, so a write to a track never drops a rollout it did not
// mention.
func mergeRelease(existing []json.RawMessage, incoming json.RawMessage, removeOtherDrafts bool) ([]json.RawMessage, error) {
	incomingCodes, err := releaseVersionCodes(incoming)
	if err != nil {
		return nil, fmt.Errorf("the staged release: %w", err)
	}

	merged := make([]json.RawMessage, 0, len(existing)+1)
	replaced := false
	for _, release := range existing {
		codes, err := releaseVersionCodes(release)
		if err != nil {
			// An existing release we cannot parse is not ours to discard: keep
			// it verbatim rather than dropping something live from the track.
			merged = append(merged, release)
			continue
		}
		if sameVersionCodes(codes, incomingCodes) {
			merged = append(merged, incoming)
			replaced = true
			continue
		}
		if removeOtherDrafts && isDraft(release) {
			continue
		}
		merged = append(merged, release)
	}
	if !replaced {
		merged = append(merged, incoming)
	}
	return merged, nil
}

// releaseInfo is the part of a Track.Release the merge needs.
type releaseInfo struct {
	VersionCodes []string `json:"versionCodes"`
	Status       string   `json:"status"`
}

func releaseVersionCodes(raw json.RawMessage) ([]string, error) {
	var info releaseInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("release is not a Track.Release object: %w", err)
	}
	if len(info.VersionCodes) == 0 {
		return nil, fmt.Errorf("release has no versionCodes")
	}
	return info.VersionCodes, nil
}

func isDraft(raw json.RawMessage) bool {
	var info releaseInfo
	return json.Unmarshal(raw, &info) == nil && strings.EqualFold(info.Status, "draft")
}

// sameVersionCodes reports whether two releases carry the same artifacts. Order
// is not significant — a multi-APK release lists its codes in whatever order
// the caller built them.
func sameVersionCodes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, code := range a {
		seen[code]++
	}
	for _, code := range b {
		seen[code]--
		if seen[code] < 0 {
			return false
		}
	}
	return true
}

// applyDirectWrite runs staged calls that are not edit-scoped. Review replies
// are the case that exists: reviews live outside the publishing transaction, so
// wrapping one in an edit would open and commit an empty edit for nothing.
func (c *Client) applyDirectWrite(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	var payload editPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return nil, fmt.Errorf("corrupt staged payload for %s: %w", p.Tool, err)
	}
	if len(payload.Requests) == 0 {
		return nil, fmt.Errorf("staged write %s has no operations", p.Tool)
	}

	var results []json.RawMessage
	for _, req := range payload.Requests {
		var out json.RawMessage
		var body any
		if len(req.Body) > 0 {
			body = req.Body
		}
		if err := c.doWrite(ctx, req.Method, req.Path, nil, body, &out); err != nil {
			if req.Describe != "" {
				return nil, fmt.Errorf("%s: %w", req.Describe, err)
			}
			return nil, err
		}
		if len(out) > 0 {
			results = append(results, out)
		}
	}
	return &applyOutcome{Detail: p.Summary, Results: results}, nil
}
