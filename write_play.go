package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	// dispatchUpload uploads an artifact and, unless asked not to, adds it to a
	// track — both inside one edit, because an artifact uploaded into an edit
	// that is never committed does not exist.
	dispatchUpload = "upload"
	// dispatchDeobfuscation uploads a mapping or native-symbol file against an
	// already-uploaded version code.
	dispatchDeobfuscation = "deobfuscation"
	// dispatchImages uploads store listing images, which cannot go through
	// dispatchEdit because an image upload is a resumable transfer rather than
	// a JSON body.
	dispatchImages = "images"
	// dispatchListingSync applies a whole reconciliation plan — text, uploads,
	// and deletions across every locale — inside one edit, so a failure on any
	// locale commits nothing.
	dispatchListingSync = "listing_sync"
)

// editRequest is one REST call inside an edit, addressed relative to the edit
// itself ("listings/en-US", "tracks/production").
type editRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
	// Query carries the call's query parameters. Some of the non-edit
	// resources put real behaviour there rather than in the body —
	// `autoConvertMissingPrices` on an in-app product decides whether the
	// regions you did not price get one at all — so it has to survive staging
	// alongside the body.
	Query map[string]string `json:"query,omitempty"`
	// Describe names what this call does, for the error message when it is the
	// one that fails. A multi-locale listing sync that fails must say which
	// locale, and the path alone does not read as an explanation.
	Describe string `json:"describe,omitempty"`
}

// values renders the staged query parameters, or nil when there are none.
func (r editRequest) values() url.Values {
	if len(r.Query) == 0 {
		return nil
	}
	query := url.Values{}
	for k, v := range r.Query {
		query.Set(k, v)
	}
	return query
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
//
// There are two modes. A create or a promotion supplies a complete Release,
// which replaces the one carrying the same version codes or is appended. An
// update supplies a Patch instead: the fields to change, merged into whatever
// the release looks like *at apply time*. That matters because a staged token
// may be confirmed minutes later — turning the rollout dial should change the
// fraction, not quietly revert release notes somebody edited in between.
type trackPayload struct {
	Track string `json:"track"`
	// Release is the complete release to insert or replace, as the API's
	// Track.Release shape. It is matched against the existing releases by
	// version code.
	Release json.RawMessage `json:"release,omitempty"`
	// MatchVersionCodes names the release a Patch applies to. Empty means the
	// track's only release, which is what makes `--rollout 0.5` work without
	// the caller having to look up a version code first.
	MatchVersionCodes []string `json:"match_version_codes,omitempty"`
	// Patch holds the fields to merge into the matched release.
	Patch json.RawMessage `json:"patch,omitempty"`
	// PatchRemove names fields to clear. Completing a release means removing
	// userFraction, not setting it to 1: the API rejects `completed` carrying
	// a fraction.
	PatchRemove []string `json:"patch_remove,omitempty"`
	// RemoveOtherDrafts drops draft releases that the new one supersedes.
	// Completed and in-progress releases are never removed by this.
	RemoveOtherDrafts       bool `json:"remove_other_drafts,omitempty"`
	ChangesNotSentForReview bool `json:"changes_not_sent_for_review,omitempty"`
}

// uploadPayload is the staged intent for an artifact upload.
type uploadPayload struct {
	FilePath    string `json:"file_path"`
	ContentType string `json:"content_type"`
	// SHA256 is the hash the preview was built from. The apply refuses when the
	// file has changed since: a token confirmed after a rebuild would otherwise
	// upload something nobody previewed.
	SHA256 string `json:"sha256"`
	// Kind is "bundle" or "apk"; they have different endpoints.
	Kind  string `json:"kind"`
	Track string `json:"track,omitempty"`
	// Release is the release to add the uploaded artifact to, without its
	// version codes — those are only known once the upload returns.
	Release                 *trackRelease `json:"release,omitempty"`
	RemoveOtherDrafts       bool          `json:"remove_other_drafts,omitempty"`
	ChangesNotSentForReview bool          `json:"changes_not_sent_for_review,omitempty"`
}

// deobfuscationPayload is the staged intent for a mapping or symbol upload.
type deobfuscationPayload struct {
	FilePath    string `json:"file_path"`
	SHA256      string `json:"sha256"`
	VersionCode string `json:"version_code"`
	// Type is "proguard" or "nativeCode".
	Type                    string `json:"type"`
	ChangesNotSentForReview bool   `json:"changes_not_sent_for_review,omitempty"`
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
	// ScopedDelete narrows a deletion to one item the caller named by id.
	ScopedDelete bool
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
		ApplyNote:       playApplyNote(req.Dispatch),
		Track:           req.Track,
		RolloutFraction: req.RolloutFraction,
		RequiresDouble:  req.RequiresDouble,
		ScopedDelete:    req.ScopedDelete,
	})
	if err != nil {
		return WriteResult{}, err
	}
	return previewResult(p), nil
}

// playApplyNote says what confirming a staged write will actually do.
//
// Most Play writes run inside an edit: the API validates the whole transaction
// before it commits, and a rejected write leaves nothing behind. The
// monetization resources and review replies are not edit-scoped, and telling
// someone their price change will be validated first — when it will not — is
// exactly the wrong thing to say in the sentence they read before confirming.
func playApplyNote(dispatch string) string {
	if dispatch == dispatchDirect {
		return "This resource is not edit-scoped: confirming sends the change straight to Play, where it takes effect immediately."
	}
	return "Confirming opens a fresh edit, validates it, and commits."
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
	case dispatchUpload:
		return c.applyUpload(ctx, p)
	case dispatchDeobfuscation:
		return c.applyDeobfuscationUpload(ctx, p)
	case dispatchImages:
		return c.applyImageUploads(ctx, p)
	case dispatchListingSync:
		return c.applyListingSync(ctx, p)
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
	err := e.c.doWrite(ctx, req.Method, e.path(req.Path), req.values(), body, out)
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
// edit*, one release is changed, and everything else is written back exactly as
// it came.
func (c *Client) applyTrackWrite(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	var payload trackPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return nil, fmt.Errorf("corrupt staged payload for %s: %w", p.Tool, err)
	}
	if payload.Track == "" {
		return nil, fmt.Errorf("staged write %s names no track", p.Tool)
	}
	if len(payload.Release) == 0 && len(payload.Patch) == 0 {
		return nil, fmt.Errorf("staged write %s changes nothing", p.Tool)
	}

	var result json.RawMessage
	editID, err := c.withEdit(ctx, p.PackageName, true,
		commitOptions{ChangesNotSentForReview: payload.ChangesNotSentForReview},
		func(e *editSession) error {
			merged, err := e.mergeTrack(ctx, payload)
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

// mergeTrack reads the track inside the edit and produces the releases array to
// write back.
func (e *editSession) mergeTrack(ctx context.Context, payload trackPayload) ([]json.RawMessage, error) {
	var current track
	if err := e.c.do(ctx, http.MethodGet, e.path("tracks/"+payload.Track), nil, nil, &current); err != nil {
		return nil, fmt.Errorf("read track %s: %w", payload.Track, err)
	}
	if len(payload.Patch) > 0 {
		return patchRelease(current.Releases, payload.MatchVersionCodes, payload.Patch, payload.PatchRemove)
	}
	return mergeRelease(current.Releases, payload.Release, payload.RemoveOtherDrafts)
}

// patchRelease merges a partial update into the one matching release, leaving
// every other release in the track untouched.
func patchRelease(existing []json.RawMessage, match []string, patch json.RawMessage, remove []string) ([]json.RawMessage, error) {
	index, err := findRelease(existing, match)
	if err != nil {
		return nil, err
	}

	merged, err := mergeJSONObject(existing[index], patch, remove)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, len(existing))
	copy(out, existing)
	out[index] = merged
	return out, nil
}

// findRelease locates the release a patch applies to. With no version codes it
// requires the track to hold exactly one release — guessing which of several to
// change would be the worst possible default for a publishing tool.
func findRelease(existing []json.RawMessage, match []string) (int, error) {
	if len(match) == 0 {
		switch len(existing) {
		case 0:
			return 0, fmt.Errorf("the track has no releases to update")
		case 1:
			return 0, nil
		default:
			return 0, fmt.Errorf("the track holds %d releases — name the one to change with --version-codes (see `rollout play tracks`)", len(existing))
		}
	}
	for i, raw := range existing {
		codes, err := releaseVersionCodes(raw)
		if err != nil {
			continue
		}
		if sameVersionCodes(codes, match) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no release with version code %s is in this track — it may already have been superseded (see `rollout play tracks`)", strings.Join(match, "+"))
}

// mergeJSONObject applies a patch object over a base object and clears the
// named fields. It works on decoded maps rather than typed structs so a field
// this binary has never heard of survives a patch untouched.
func mergeJSONObject(base, patch json.RawMessage, remove []string) (json.RawMessage, error) {
	merged := map[string]any{}
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, fmt.Errorf("the release being updated is not an object: %w", err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(patch, &fields); err != nil {
		return nil, fmt.Errorf("the staged update is not an object: %w", err)
	}
	for k, v := range fields {
		merged[k] = v
	}
	for _, k := range remove {
		delete(merged, k)
	}
	return json.Marshal(merged)
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
		if err := c.doWrite(ctx, req.Method, req.Path, req.values(), body, &out); err != nil {
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

// applyUpload uploads an artifact and, unless the write asked for upload-only,
// adds it to a track — all inside one edit.
//
// The two steps cannot be split. An artifact uploaded into an edit that is
// never committed does not exist, so an upload followed by a separate release
// write would either commit a bare artifact or lose it entirely.
func (c *Client) applyUpload(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	var payload uploadPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return nil, fmt.Errorf("corrupt staged payload for %s: %w", p.Tool, err)
	}
	if err := verifyStagedFile(payload.FilePath, payload.SHA256); err != nil {
		return nil, err
	}

	endpoint := "bundles"
	if payload.Kind == "apk" {
		endpoint = "apks"
	}

	var uploaded struct {
		VersionCode int64  `json:"versionCode"`
		SHA256      string `json:"sha256"`
		Binary      struct {
			SHA256 string `json:"sha256"`
		} `json:"binary"`
	}
	var trackResult json.RawMessage

	editID, err := c.withEdit(ctx, p.PackageName, true,
		commitOptions{ChangesNotSentForReview: payload.ChangesNotSentForReview},
		func(e *editSession) error {
			if err := c.uploadMedia(ctx, e.path(endpoint), payload.ContentType, payload.FilePath, nil, &uploaded, nil); err != nil {
				return fmt.Errorf("upload %s: %w", payload.FilePath, err)
			}
			if uploaded.VersionCode == 0 {
				return fmt.Errorf("upload %s: the API returned no version code", payload.FilePath)
			}
			if payload.Release == nil || payload.Track == "" {
				return nil
			}

			release := *payload.Release
			release.VersionCodes = []string{fmt.Sprint(uploaded.VersionCode)}
			raw, err := json.Marshal(release)
			if err != nil {
				return err
			}
			merged, err := e.mergeTrack(ctx, trackPayload{
				Track: payload.Track, Release: raw, RemoveOtherDrafts: payload.RemoveOtherDrafts,
			})
			if err != nil {
				return err
			}
			body := track{Track: payload.Track, Releases: merged}
			return e.c.doWrite(ctx, http.MethodPut, e.path("tracks/"+payload.Track), nil, body, &trackResult)
		})
	if err != nil {
		return &applyOutcome{EditID: editID}, err
	}

	detail := fmt.Sprintf("%s uploaded as version code %d", payload.FilePath, uploaded.VersionCode)
	if payload.Track != "" && payload.Release != nil {
		detail += fmt.Sprintf(" and added to %s", payload.Track)
	}
	results := []json.RawMessage{jsonRow(map[string]any{"versionCode": uploaded.VersionCode, "sha256": uploaded.SHA256})}
	if len(trackResult) > 0 {
		results = append(results, trackResult)
	}
	return &applyOutcome{EditID: editID, Detail: detail, Results: results}, nil
}

// applyDeobfuscationUpload attaches a mapping or native-symbol file to an
// already-uploaded version code.
func (c *Client) applyDeobfuscationUpload(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	var payload deobfuscationPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return nil, fmt.Errorf("corrupt staged payload for %s: %w", p.Tool, err)
	}
	if err := verifyStagedFile(payload.FilePath, payload.SHA256); err != nil {
		return nil, err
	}

	var uploaded json.RawMessage
	editID, err := c.withEdit(ctx, p.PackageName, true,
		commitOptions{ChangesNotSentForReview: payload.ChangesNotSentForReview},
		func(e *editSession) error {
			path := e.path(fmt.Sprintf("apks/%s/deobfuscationFiles/%s", payload.VersionCode, payload.Type))
			return c.uploadMedia(ctx, path, "application/octet-stream", payload.FilePath, nil, &uploaded, nil)
		})
	if err != nil {
		return &applyOutcome{EditID: editID}, err
	}
	return &applyOutcome{EditID: editID, Detail: p.Summary, Results: []json.RawMessage{uploaded}}, nil
}

// verifyStagedFile refuses to upload a file that changed after it was
// previewed.
//
// A confirm token outlives the command that produced it, and the obvious way to
// spend that window is another build. Uploading whatever is at the path now
// would ship an artifact nobody looked at — and the hash was already in the
// preview, so this costs one read to close.
func verifyStagedFile(path, want string) error {
	if want == "" {
		return nil
	}
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s has changed since it was previewed (sha256 %s, previewed %s) — re-run the command to preview the current file", path, got, want)
	}
	return nil
}

// fileSHA256 hashes a file. Play reports the same digest back on upload, so it
// is both the preview's identity for the artifact and the check that the file
// did not move under us.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// imagesPayload is the staged intent for an image upload.
type imagesPayload struct {
	Locale                  string        `json:"locale"`
	Uploads                 []imageUpload `json:"uploads"`
	ChangesNotSentForReview bool          `json:"changes_not_sent_for_review,omitempty"`
}

// applyImageUploads uploads store listing images inside one edit.
func (c *Client) applyImageUploads(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	var payload imagesPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return nil, fmt.Errorf("corrupt staged payload for %s: %w", p.Tool, err)
	}
	if len(payload.Uploads) == 0 {
		return nil, fmt.Errorf("staged write %s has no images to upload", p.Tool)
	}

	var results []json.RawMessage
	editID, err := c.withEdit(ctx, p.PackageName, true,
		commitOptions{ChangesNotSentForReview: payload.ChangesNotSentForReview},
		func(e *editSession) error {
			return e.uploadImages(ctx, payload.Locale, payload.Uploads, &results)
		})
	if err != nil {
		return &applyOutcome{EditID: editID}, err
	}
	return &applyOutcome{EditID: editID, Detail: p.Summary, Results: results}, nil
}

// uploadImages sends each image inside an already-open edit, refusing any file
// that changed since the preview hashed it.
func (e *editSession) uploadImages(ctx context.Context, locale string, uploads []imageUpload, results *[]json.RawMessage) error {
	for _, upload := range uploads {
		if err := verifyStagedFile(upload.Path, upload.SHA256); err != nil {
			return err
		}
		contentType, err := contentTypeForArtifact(upload.Path)
		if err != nil {
			return err
		}
		var out json.RawMessage
		path := e.path("listings/" + locale + "/" + upload.Type)
		if err := e.c.uploadMedia(ctx, path, contentType, upload.Path, nil, &out, nil); err != nil {
			return fmt.Errorf("upload %s as %s for %s: %w", upload.Path, upload.Type, locale, err)
		}
		if results != nil && len(out) > 0 {
			*results = append(*results, out)
		}
	}
	return nil
}

// listingSyncPayload is the whole reconciliation plan, embedded so
// `rollout confirm` can apply it from another process without re-reading the
// directory — and so a file that changed since the preview is refused rather
// than silently published.
type listingSyncPayload struct {
	Plans                   []localeSyncPlan `json:"plans"`
	ChangesNotSentForReview bool             `json:"changes_not_sent_for_review,omitempty"`
}

// applyListingSync applies a reconciliation plan across every locale in one
// edit.
//
// Partial success is not success. A sync that updates six locales and fails on
// the seventh has left the store inconsistent in a way nobody asked for, so the
// whole plan runs inside a single edit: any failure aborts it, nothing commits,
// and the error names the locale.
func (c *Client) applyListingSync(ctx context.Context, p *PendingMutation) (*applyOutcome, error) {
	var payload listingSyncPayload
	if err := json.Unmarshal(p.Payload, &payload); err != nil {
		return nil, fmt.Errorf("corrupt staged payload for %s: %w", p.Tool, err)
	}
	if len(payload.Plans) == 0 {
		return nil, fmt.Errorf("staged write %s has nothing to sync", p.Tool)
	}

	var results []json.RawMessage
	editID, err := c.withEdit(ctx, p.PackageName, true,
		commitOptions{ChangesNotSentForReview: payload.ChangesNotSentForReview},
		func(e *editSession) error {
			for _, plan := range payload.Plans {
				if err := e.applyLocalePlan(ctx, plan, &results); err != nil {
					return fmt.Errorf("locale %s: %w", plan.Locale, err)
				}
			}
			return nil
		})
	if err != nil {
		return &applyOutcome{EditID: editID}, err
	}
	return &applyOutcome{EditID: editID, Detail: p.Summary, Results: results}, nil
}

// applyLocalePlan runs one locale's text write, deletions, and uploads.
func (e *editSession) applyLocalePlan(ctx context.Context, plan localeSyncPlan, results *[]json.RawMessage) error {
	if plan.Listing != nil {
		body, err := json.Marshal(apiListing(*plan.Listing))
		if err != nil {
			return err
		}
		var out json.RawMessage
		if err := e.c.doWrite(ctx, http.MethodPut, e.path("listings/"+plan.Locale), nil, json.RawMessage(body), &out); err != nil {
			return err
		}
		if len(out) > 0 {
			*results = append(*results, out)
		}
	}
	// Deletions run before uploads: a type at its image limit cannot accept a
	// replacement until the old one is gone.
	for _, deletion := range plan.Deletes {
		path := e.path("listings/" + plan.Locale + "/" + deletion.Type + "/" + deletion.ID)
		if err := e.c.doWrite(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
			return fmt.Errorf("delete %s image %s: %w", deletion.Type, deletion.ID, err)
		}
	}
	return e.uploadImages(ctx, plan.Locale, plan.Uploads, results)
}
