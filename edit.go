package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Play's publishing API stages every change inside an *edit*: a server-side
// transaction opened with edits.insert, mutated through the resources hanging
// off it, and made real with edits.commit. Reads need one too — a track, a
// listing, an image list can only be fetched inside an edit.
//
// Two rules follow, and both are load-bearing:
//
//   - An edit is owned by one call. Edits expire, and opening a new one
//     invalidates the previous one for the same app, so an edit ID persisted
//     across processes is a guaranteed 409 later. Nothing here ever stores one.
//   - Every write validates before it commits, and deletes the edit on any
//     failure. Without the delete, a failed write leaves a half-staged edit
//     that the next call inherits or collides with.

// editSession is one open edit. It is not safe for concurrent use: an edit is
// a transaction, and interleaving two callers' mutations inside one is exactly
// the bug the per-call ownership rule exists to prevent.
type editSession struct {
	c   *Client
	pkg string
	id  string
}

// appEdit is the API's edit resource.
type appEdit struct {
	ID                string `json:"id"`
	ExpiryTimeSeconds string `json:"expiryTimeSeconds"`
}

// editPath builds a path under this edit, e.g. "tracks/production".
func (e *editSession) path(suffix string) string {
	p := fmt.Sprintf("applications/%s/edits/%s", e.pkg, e.id)
	if suffix != "" {
		p += "/" + suffix
	}
	return p
}

// openEdit starts a new edit for pkg.
func (c *Client) openEdit(ctx context.Context, pkg string) (*editSession, error) {
	var edit appEdit
	if err := c.doWrite(ctx, http.MethodPost, fmt.Sprintf("applications/%s/edits", pkg), nil, nil, &edit); err != nil {
		return nil, fmt.Errorf("open an edit on %s: %w", pkg, err)
	}
	if edit.ID == "" {
		return nil, fmt.Errorf("open an edit on %s: the API returned no edit id", pkg)
	}
	return &editSession{c: c, pkg: pkg, id: edit.ID}, nil
}

// get reads this edit's own resource, which is how a caller learns when it
// expires.
func (e *editSession) get(ctx context.Context) (*appEdit, error) {
	var edit appEdit
	if err := e.c.do(ctx, http.MethodGet, e.path(""), nil, nil, &edit); err != nil {
		return nil, fmt.Errorf("read edit %s: %w", e.id, err)
	}
	return &edit, nil
}

// validate asks the API to check the staged changes without applying them.
// It is the dry run every write does before committing.
func (e *editSession) validate(ctx context.Context) error {
	if err := e.c.do(ctx, http.MethodPost, e.path("")+":validate", nil, nil, nil); err != nil {
		return fmt.Errorf("validate edit %s: %w", e.id, err)
	}
	return nil
}

// commitOptions carries the flags edits.commit accepts.
type commitOptions struct {
	// ChangesNotSentForReview commits without submitting the changes for
	// review. Google requires it for apps whose changes would otherwise be
	// queued for review, and rejects it for apps where a review is mandatory —
	// so it is a per-call decision, not a default.
	ChangesNotSentForReview bool
}

// commit applies the staged changes. It is never retried: a 5xx may mean the
// server committed and lost the response, and a repeated commit publishes
// twice.
func (e *editSession) commit(ctx context.Context, opts commitOptions) (*appEdit, error) {
	query := url.Values{}
	if opts.ChangesNotSentForReview {
		query.Set("changesNotSentForReview", "true")
	}
	var edit appEdit
	if err := e.c.doWrite(ctx, http.MethodPost, e.path("")+":commit", query, nil, &edit); err != nil {
		return nil, fmt.Errorf("commit edit %s: %w", e.id, err)
	}
	return &edit, nil
}

// delete abandons the edit. Best-effort by convention: an abandoned edit also
// expires on its own, so a failed delete is worth a warning but never worth
// replacing the caller's real error.
func (e *editSession) delete(ctx context.Context) error {
	if err := e.c.doWrite(ctx, http.MethodDelete, e.path(""), nil, nil, nil); err != nil {
		return fmt.Errorf("delete edit %s: %w", e.id, err)
	}
	return nil
}

// withEdit runs fn inside a fresh edit and returns the edit's ID.
//
// Read mode (commit false) always deletes the edit — a read that leaves edits
// behind burns quota and litters the app's edit list.
//
// Write mode (commit true) validates and then commits, and deletes the edit on
// any failure along the way, so a rejected write leaves nothing staged. The
// deletion uses a background context: the common reason a write fails is a
// cancelled context, and cleaning up with the same cancelled context would skip
// the cleanup exactly when it is needed.
func (c *Client) withEdit(ctx context.Context, pkg string, commit bool, opts commitOptions, fn func(*editSession) error) (editID string, err error) {
	e, err := c.openEdit(ctx, pkg)
	if err != nil {
		return "", err
	}
	abandon := func() {
		cleanupCtx, cancel := cleanupContext()
		defer cancel()
		if delErr := e.delete(cleanupCtx); delErr != nil {
			warnOnce("could not delete edit %s on %s (%v) — it expires on its own, but a stale edit can collide with the next call.", e.id, pkg, delErr)
		}
	}

	if err := fn(e); err != nil {
		abandon()
		return e.id, err
	}
	if !commit {
		abandon()
		return e.id, nil
	}
	if err := e.validate(ctx); err != nil {
		abandon()
		return e.id, err
	}
	if _, err := e.commit(ctx, opts); err != nil {
		abandon()
		return e.id, err
	}
	return e.id, nil
}

// cleanupContext gives a best-effort cleanup call its own deadline, detached
// from a caller's context that may already be cancelled. The common reason a
// write fails is exactly that cancellation, and cleaning up with the same dead
// context would skip the cleanup precisely when it is needed.
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}
