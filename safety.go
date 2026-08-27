package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file implements the confirm-token flow shared by every write tool. The
// rule: no mutating API call executes on first request. A write tool returns a
// human-readable preview plus a short-lived token; the caller re-invokes with
// that token to actually apply the change.
//
// The token store is file-backed (under stateDir) so it survives across the
// stateless CLI invocations a skill makes, and works the same inside a
// long-lived `rollout mcp` session.

// confirmTTL bounds how long a pending confirmation is valid.
const confirmTTL = 10 * time.Minute

// PendingMutation is what a write tool stages for confirmation.
//
// It describes the *intent* of a write, never an in-flight API session. Play's
// publishing API stages changes inside an edit that expires, and a token may be
// confirmed minutes later from another process — so an edit ID must never be
// persisted here. The applier opens a fresh edit, mutates, validates, and
// commits, all within the confirm call (see edit.go).
type PendingMutation struct {
	Token string `json:"token"`
	// Platform is the namespace whose API applies this write.
	// `rollout confirm` reads it to build the right client, so a token staged
	// by one platform is never applied against another's API.
	Platform string `json:"platform"`
	Tool     string `json:"tool"`
	// PackageName is the app the write acts on, in whatever form the platform
	// names apps (an Android package name for Play).
	PackageName string `json:"package_name"`
	Summary     string `json:"summary"`
	// Dispatch selects the apply route within the platform ("track",
	// "listing", "images", …); the empty value is the platform's default.
	Dispatch string `json:"dispatch,omitempty"`
	// Payload is the platform's own operation payload, kept opaque here so
	// this file never learns what any store's writes look like.
	Payload json.RawMessage `json:"payload,omitempty"`
	// ApplyNote is one sentence saying what confirming actually does, supplied
	// by the platform because only it knows whether a given write is
	// transactional. It is not decoration: a preview that promises the API will
	// validate the change first is describing a safety net, and for the
	// resources that are not edit-scoped that net does not exist.
	ApplyNote string `json:"apply_note,omitempty"`
	// Track and RolloutFraction restate the two facts the shared guard rails
	// need when a token is confirmed: which track is being written and what
	// staged-rollout fraction it would end up at. They are declared by the
	// staging tool so guards.go can re-check the configuration in force *now*
	// without decoding a platform-specific payload.
	Track           string   `json:"track,omitempty"`
	RolloutFraction *float64 `json:"rollout_fraction,omitempty"`
	// ScopedDelete marks a deletion the caller narrowed to one item they named
	// by id. Deleting everything of a kind is the case where nobody can say
	// afterwards what was there; deleting the one thing you asked for by id is
	// not, so it does not take a second confirmation.
	ScopedDelete bool      `json:"scoped_delete,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// RequiresDouble marks destructive operations that need a second
	// confirmation. DoubleConfirmed is set once the first confirm has been
	// consumed and the mutation re-staged under a fresh token.
	RequiresDouble  bool `json:"requires_double,omitempty"`
	DoubleConfirmed bool `json:"double_confirmed,omitempty"`
}

// pendingWrite is what a platform hands to stageWrite. It is a struct rather
// than a parameter list because tools fill in different subsets of it, and a
// positional call would say nothing about which.
type pendingWrite struct {
	// Platform is the namespace whose API will apply this write — the one piece
	// of routing this file keeps, so `rollout confirm <token>` can find the
	// right client without knowing what any platform's payload looks like.
	Platform string
	// Tool is the tool staging the write; a token is bound to it.
	Tool string
	// PackageName is the app the write acts on.
	PackageName string
	// Summary is the human-readable description shown in the preview.
	Summary string
	// Dispatch selects the apply route within the platform.
	Dispatch string
	// Payload is the platform's own operation payload.
	Payload json.RawMessage
	// ApplyNote is what confirming will do; see PendingMutation.ApplyNote.
	ApplyNote string
	// Track and RolloutFraction declare what the shared guard rails re-check
	// at confirm time.
	Track           string
	RolloutFraction *float64
	// ScopedDelete narrows a deletion to one named item; see PendingMutation.
	ScopedDelete bool
	// RequiresDouble forces a second confirmation for a write whose tool name
	// does not already imply one.
	RequiresDouble bool
}

// stageWrite persists a pending write and returns its confirm token.
func stageWrite(w pendingWrite) (*PendingMutation, error) {
	if w.Platform == "" {
		return nil, fmt.Errorf("internal error: %s staged a write without a platform", w.Tool)
	}
	// The guard rails run before a token is handed out, so a blocked or
	// over-cap write never gets a confirmation the user could act on. They run
	// again at confirm time (see enforceGuards) against the configuration in
	// force then.
	safety := loadSafetyConfig()
	if err := checkBlockedOperation(w.Tool, safety); err != nil {
		return nil, err
	}
	if err := checkRolloutFraction(w.RolloutFraction, safety); err != nil {
		return nil, err
	}
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	p := &PendingMutation{
		Token:           tok,
		Platform:        w.Platform,
		Tool:            w.Tool,
		PackageName:     w.PackageName,
		Summary:         w.Summary,
		Dispatch:        w.Dispatch,
		Payload:         w.Payload,
		ApplyNote:       w.ApplyNote,
		Track:           w.Track,
		RolloutFraction: w.RolloutFraction,
		ScopedDelete:    w.ScopedDelete,
		CreatedAt:       time.Now().UTC(),
		RequiresDouble:  w.RequiresDouble || requiresDoubleConfirmation(w, safety),
	}
	dir, err := stateDir()
	if err != nil {
		// Fail loudly: the token store is disk-backed only, so a token staged
		// without persistence could never be confirmed — handing one out would
		// promise an apply that must fail.
		return nil, fmt.Errorf("confirmation store unavailable (%v) — writes need a usable config directory; set HOME/XDG_CONFIG_HOME", err)
	}
	sweepExpired(dir)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("stage confirmation: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pending-"+tok+".json"), data, 0o600); err != nil {
		return nil, fmt.Errorf("stage confirmation: %w", err)
	}
	return p, nil
}

// sweepExpired removes pending files past their TTL so abandoned previews
// don't accumulate in the state dir forever. Best-effort. Includes .claimed
// leftovers a crash between claim and remove could strand.
func sweepExpired(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "pending-") ||
			(!strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".json.claimed")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > confirmTTL {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// validToken reports whether s has the exact shape newToken generates
// (16 lowercase hex chars). Anything else is rejected before it can reach the
// filesystem — the token is caller-supplied input and must never influence
// which path is read or removed.
func validToken(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// pendingPath validates a caller-supplied token and returns the path of its
// pending file. The shape check runs before the token can touch the filesystem.
func pendingPath(token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("no confirmation token provided")
	}
	if !validToken(token) {
		return "", fmt.Errorf("malformed confirmation token %q — expected the 16-character token from the preview", token)
	}
	dir, err := stateDir()
	if err != nil {
		return "", fmt.Errorf("confirmation store unavailable: %w", err)
	}
	return filepath.Join(dir, "pending-"+token+".json"), nil
}

// peekMutation loads a pending mutation by token WITHOUT consuming it, so
// pre-checks (e.g. blocked operations) can fail before the single-use token is
// irrevocably claimed.
func peekMutation(token string) (*PendingMutation, error) {
	path, err := pendingPath(strings.TrimSpace(token))
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unknown or already-used confirmation token %q", strings.TrimSpace(token))
	}
	var p PendingMutation
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("corrupt confirmation %q: %w", strings.TrimSpace(token), err)
	}
	return &p, nil
}

// consumeMutation loads and deletes a pending mutation by token, rejecting
// unknown or expired tokens.
func consumeMutation(token string) (*PendingMutation, error) {
	token = strings.TrimSpace(token)
	path, err := pendingPath(token)
	if err != nil {
		return nil, err
	}
	// Claim the pending file atomically before reading: two concurrent
	// confirms must not both apply the same staged mutation. Only the rename
	// winner proceeds; the file stays single-use even if the apply later fails.
	claimed := path + ".claimed"
	if err := os.Rename(path, claimed); err != nil {
		return nil, fmt.Errorf("unknown or already-used confirmation token %q", token)
	}
	data, err := os.ReadFile(claimed)
	_ = os.Remove(claimed)
	if err != nil {
		return nil, fmt.Errorf("read confirmation %q: %w", token, err)
	}
	var p PendingMutation
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("corrupt confirmation %q: %w", token, err)
	}
	if time.Since(p.CreatedAt) > confirmTTL {
		return nil, fmt.Errorf("confirmation token %q expired (valid for %s); re-run the command to get a fresh one", token, confirmTTL)
	}
	return &p, nil
}

// restageForDoubleConfirm re-stages a consumed destructive mutation under a
// fresh token that must be confirmed once more before it applies.
func restageForDoubleConfirm(p *PendingMutation) (*PendingMutation, error) {
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	p.Token = tok
	p.DoubleConfirmed = true
	p.CreatedAt = time.Now().UTC()
	dir, err := stateDir()
	if err != nil {
		return nil, fmt.Errorf("confirmation store unavailable: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("stage second confirmation: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pending-"+tok+".json"), data, 0o600); err != nil {
		return nil, fmt.Errorf("stage second confirmation: %w", err)
	}
	return p, nil
}

// applyOutcome is what a platform's applier hands back once a confirmed write
// has really been executed. EditID names the Play edit that carried it, which
// is the only handle a user has to correlate a change with the Console's
// activity log.
type applyOutcome struct {
	EditID  string
	Detail  string
	Results []json.RawMessage
}

// mutationApplier executes a confirmed write against one platform's API. Each
// platform's client implements it, so staging, confirming, double-confirmation,
// and auditing are shared and only the final API call is platform-specific.
type mutationApplier interface {
	// platformName is the namespace this client writes to. A staged write
	// records the platform that created it, and the two must agree before
	// anything is applied: tool names are not unique across platforms — every
	// store has some form of update_listing — so the tool binding alone would
	// let one platform's token be handed to another platform's API.
	platformName() string
	applyMutation(ctx context.Context, p *PendingMutation) (*applyOutcome, error)
}

// platform is the namespace that staged this write.
func (p *PendingMutation) platform() string { return p.Platform }

// previewText renders a staged mutation for a human/agent to review.
func (p *PendingMutation) previewText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "PREVIEW — %s on %s\n", p.Tool, p.PackageName)
	fmt.Fprintf(&b, "%s\n", p.Summary)
	b.WriteString("Nothing has been changed yet.")
	if p.ApplyNote != "" {
		b.WriteString(" " + p.ApplyNote)
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "\nTo apply, re-run with --confirm %s (or run: rollout confirm %s)\n", p.Token, p.Token)
	return b.String()
}

// auditLog appends one JSON line describing an applied mutation, on success
// and on failure alike — a write that the API rejected is exactly the event a
// user reconstructing a release wants to find. Best-effort: audit failures
// never block or fail the operation.
func auditLog(p *PendingMutation, outcome *applyOutcome, applyErr error) {
	dir, err := stateDir()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	entry := map[string]any{
		"time":     time.Now().UTC().Format(time.RFC3339),
		"platform": p.Platform,
		"tool":     p.Tool,
		"package":  p.PackageName,
		"summary":  p.Summary,
		"applied":  applyErr == nil,
		"token":    p.Token,
	}
	if applyErr != nil {
		entry["error"] = applyErr.Error()
	}
	if outcome != nil && outcome.EditID != "" {
		entry["edit_id"] = outcome.EditID
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	fmt.Fprintln(f, string(line))
}

func newToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
