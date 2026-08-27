package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A hand-rolled REST client for the Android Publisher API v3 and the Play
// Developer Reporting API v1beta1.
//
// google.golang.org/api/androidpublisher/v3 was the alternative. It pulls in
// the whole google-api-go-client tree, hides the wire format the tools expose
// 1:1, and makes httptest fakes awkward. A thin client is a few hundred lines
// and every call is a plain net/http request against an overridable base URL.

// publisherAPIPath is the version-carrying prefix every Publisher call sits
// under. It is the single place the API version is spelled.
const publisherAPIPath = "androidpublisher/v3"

// Client talks to Google Play. It is safe for concurrent use.
type Client struct {
	cfg  *PlayConfig
	http *http.Client
	// stream is the client for transfers whose size is not known in advance —
	// media uploads and report downloads.
	stream *http.Client
}

// newPlayClient builds a Play client from the resolved configuration (the
// global --config flag plus the environment). Shared by every `rollout play`
// subcommand and by the MCP server's play_* tools.
func newPlayClient(ctx context.Context) (*Client, error) {
	cfg, err := loadPlayConfig(configPath)
	if err != nil {
		return nil, err
	}
	return NewClient(ctx, cfg)
}

// NewClient builds a Client from an already-loaded config.
func NewClient(ctx context.Context, cfg *PlayConfig) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	authed, err := newPlayHTTPClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Media transfers move arbitrarily large bodies, and http.Client.Timeout
	// covers the whole transfer — the 60s cap that keeps an ordinary API call
	// from hanging would abort any upload, or any report download, slower than
	// that. Let the request context bound these instead.
	stream := &http.Client{Transport: authed.Transport}
	return &Client{cfg: cfg, http: authed, stream: stream}, nil
}

// platformName makes *Client a mutationApplier: Play's namespace, which the
// confirm path checks a staged write against before applying it.
func (c *Client) platformName() string { return playPlatformName }

// resolvePackage returns the app a tool call should act on: the explicit
// argument when given, otherwise the configured default. Every handler calls
// this, so the default works identically for CLI flags and MCP tool arguments.
func (c *Client) resolvePackage(explicit string) (string, error) {
	return c.cfg.resolvePackage(explicit)
}

// resolveDeveloperID returns the developer account the users and grants tools
// act on. Only they need it: everything else is addressed by package name.
func (c *Client) resolveDeveloperID(explicit string) (string, error) {
	return c.cfg.resolveDeveloperID(explicit)
}

// --- request plumbing ---

// retryMaxAttempts bounds how many times a transient failure is retried.
const retryMaxAttempts = 5

// retryBaseDelay is a var so tests can shrink it.
var retryBaseDelay = 500 * time.Millisecond

// retryPolicy decides which failures are worth another attempt.
type retryPolicy int

const (
	// retryIdempotent retries 429 and 5xx. Safe for GETs and for the
	// idempotent parts of an edit (get, validate).
	retryIdempotent retryPolicy = iota
	// retryNever is for calls that must not be repeated: edits.commit and
	// uploads. A 5xx there may mean the server did the work and lost the
	// response, and a retried commit would publish twice.
	retryNever
)

func (p retryPolicy) retryable(status int) bool {
	if p == retryNever {
		return false
	}
	return status == http.StatusTooManyRequests || status >= 500
}

// backoffDelay computes the sleep before the next attempt: Retry-After when the
// server sent one, otherwise base*2^(attempt-1) plus up to 50% jitter, capped
// so a long Retry-After cannot stall a CLI run indefinitely.
func backoffDelay(attempt int, retryAfter string) time.Duration {
	const maxDelay = 30 * time.Second
	if retryAfter != "" {
		if secs, err := strconv.Atoi(retryAfter); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, maxDelay)
		}
	}
	d := retryBaseDelay << (attempt - 1)
	return min(d+time.Duration(mathrand.Int64N(int64(d)/2+1)), maxDelay)
}

// do issues one JSON request and decodes the response into out (which may be
// nil). path is relative to the Publisher API root; query may be nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.doAt(ctx, c.cfg.BaseURL+"/"+publisherAPIPath+"/"+path, method, query, body, out, policyFor(method))
}

// doWrite is do for a call that must never be retried automatically.
func (c *Client) doWrite(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.doAt(ctx, c.cfg.BaseURL+"/"+publisherAPIPath+"/"+path, method, query, body, out, retryNever)
}

// policyFor picks the default retry policy from the method. A GET can always be
// repeated; anything that mutates has to opt in explicitly.
func policyFor(method string) retryPolicy {
	if method == http.MethodGet {
		return retryIdempotent
	}
	return retryNever
}

// doAt is do against a fully-qualified URL, so the Reporting API and the upload
// endpoints share one implementation of headers, retries, and error parsing.
func (c *Client) doAt(ctx context.Context, rawURL, method string, query url.Values, body, out any, policy retryPolicy) error {
	data, err := c.fetch(ctx, rawURL, method, query, body, fetchOptions{policy: policy, accept: "application/json", client: c.http})
	if err != nil {
		return err
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s response: %w", rawURL, err)
		}
	}
	return nil
}

// fetchOptions tunes one raw request: which failures to retry, what to accept,
// which HTTP client to send on, and how many bytes to take.
//
// client is a choice rather than always c.http because a report download has no
// bounded size: it belongs on the streaming client, where the request context
// is the deadline instead of the 60s whole-transfer cap. maxBytes is the other
// half of that — an unbounded deadline and an unbounded body together are how a
// process runs out of memory.
type fetchOptions struct {
	policy retryPolicy
	accept string
	client *http.Client
	// maxBytes caps the response body; 0 takes it whole. A response past the
	// cap is an error, never a truncation — half a CSV parses as a whole one.
	maxBytes int64
}

// fetch issues one request under the retry policy and returns the raw response
// body. doAt decodes JSON on top of it; the Cloud Storage reader downloads
// media through it, which is why the response is bytes rather than a decoded
// value — a CSV export is not JSON, and it should still get the same headers,
// backoff, and Google error-envelope parsing as everything else.
func (c *Client) fetch(ctx context.Context, rawURL, method string, query url.Values, body any, opts fetchOptions) ([]byte, error) {
	if len(query) > 0 {
		rawURL += "?" + query.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}

	for attempt := 1; ; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", opts.accept)
		req.Header.Set("User-Agent", playUserAgent())
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := opts.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("call %s %s: %w", method, rawURL, err)
		}
		body := io.Reader(resp.Body)
		if opts.maxBytes > 0 {
			// One byte past the cap, so an over-long body is detectable rather
			// than silently truncated into something that still parses.
			body = io.LimitReader(resp.Body, opts.maxBytes+1)
		}
		data, readErr := io.ReadAll(body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s response: %w", rawURL, readErr)
		}
		if opts.maxBytes > 0 && int64(len(data)) > opts.maxBytes && resp.StatusCode < 300 {
			return nil, fmt.Errorf("%s is larger than the %d MB rollout will read into memory", rawURL, opts.maxBytes>>20)
		}

		if resp.StatusCode >= 300 {
			apiErr := parseAPIError(resp.StatusCode, data)
			if attempt < retryMaxAttempts && opts.policy.retryable(resp.StatusCode) {
				select {
				case <-time.After(backoffDelay(attempt, resp.Header.Get("Retry-After"))):
					continue
				case <-ctx.Done():
					return nil, apiErr
				}
			}
			return nil, apiErr
		}
		return data, nil
	}
}

// --- errors ---

// apiError is a non-2xx response from a Google API. It carries the HTTP status
// so callers can tell a definitive client error (4xx — the request or the
// credentials are wrong) from a transient server error (5xx), and the reason
// code from Google's error envelope, which is what the actionable messages key
// off. A transport failure (no response at all) is a plain error, not this type.
type apiError struct {
	Status  int
	Reason  string
	Message string
}

func (e *apiError) Error() string {
	if hint := e.hint(); hint != "" {
		return e.bare() + " — " + hint
	}
	return e.bare()
}

// bare renders what the API actually said, without rollout's hint. A caller
// that supplies its own — the Reporting probes, whose 403 has nothing to do
// with release permissions — would otherwise print two pieces of advice, one of
// them for the wrong API.
func (e *apiError) bare() string {
	return fmt.Sprintf("Play API %d (%s): %s", e.Status, e.reasonOrStatus(), e.Message)
}

// isClientError reports whether the status is 4xx: the server understood the
// request and rejected it based on what we sent, i.e. a setup or argument
// problem the user must fix rather than a transient failure.
func (e *apiError) isClientError() bool { return e.Status >= 400 && e.Status < 500 }

// isThrottled reports whether the request was rate-limited rather than refused.
// It is the one 4xx that says nothing about the setup: the same call, with the
// same credential, succeeds once the quota window moves.
func (e *apiError) isThrottled() bool { return e.Status == http.StatusTooManyRequests }

func (e *apiError) reasonOrStatus() string {
	if e.Reason != "" {
		return e.Reason
	}
	return http.StatusText(e.Status)
}

// hint turns the handful of failures that actually happen into the sentence
// that names the fix. Google's own messages are accurate and useless: "The
// caller does not have permission" does not say which console page to open.
func (e *apiError) hint() string {
	switch {
	case e.Status == http.StatusUnauthorized, e.Status == http.StatusForbidden:
		return "grant the credential access to this app in Play Console → Users & permissions (a release needs \"Release to production, exclude devices, and use Play App Signing\")"
	case e.Status == http.StatusNotFound:
		return "app not found — check the package name; a new app must be created in the Console and have one uploaded artifact before the API can see it"
	case e.Status == http.StatusConflict, strings.Contains(e.Reason, "editAlreadyCommitted"), strings.Contains(e.Reason, "editExpired"):
		return "this edit was invalidated by a newer change; re-run to open a fresh edit"
	case e.Status == http.StatusTooManyRequests:
		return "quota exhausted — the Publisher API allows 200,000 requests/day and the Reporting API 10 queries/second"
	default:
		return ""
	}
}

// parseAPIError turns a non-2xx response into a readable error. Google's
// envelope nests the useful reason code inside error.errors[], which the
// top-level status does not repeat.
func parseAPIError(status int, body []byte) error {
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	err := &apiError{Status: status}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		err.Message = envelope.Error.Message
		err.Reason = envelope.Error.Status
		if len(envelope.Error.Errors) > 0 {
			if reason := envelope.Error.Errors[0].Reason; reason != "" {
				err.Reason = reason
			}
			// The nested message is usually the specific one; the top-level is
			// often a generic restatement of the status.
			if msg := envelope.Error.Errors[0].Message; msg != "" && msg != err.Message {
				err.Message += " (" + msg + ")"
			}
		}
		return err
	}
	err.Message = strings.TrimSpace(string(body))
	if err.Message == "" {
		err.Message = http.StatusText(status)
	}
	return err
}

// isNotFound reports whether err is a 404 from the API. Several read tools
// legitimately ask for something that may not exist (a listing in a locale that
// has none), and a missing thing is an answer, not a failure.
func isNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// --- paging ---

// pagedResponse is the shape both APIs use for a paginated result: Google's
// Publisher endpoints send `tokenPagination.nextPageToken`, the Reporting API
// sends a bare `nextPageToken`.
type pagedResponse struct {
	NextPageToken   string `json:"nextPageToken"`
	TokenPagination struct {
		NextPageToken string `json:"nextPageToken"`
	} `json:"tokenPagination"`
}

func (p pagedResponse) next() string {
	if p.NextPageToken != "" {
		return p.NextPageToken
	}
	return p.TokenPagination.NextPageToken
}

// maxPages bounds a paged read. Reviews and vitals rows can run to thousands of
// pages; a tool that walks them all would blow an agent's context and its
// caller's quota, and the cap is reported rather than silently applied.
const maxPages = 50

// eachPage walks a paginated endpoint, calling fetch with the page token and
// letting it report the next one. It stops at the end of the results, at
// maxPages, or when fetch says to stop.
//
// It returns whether pages were left unread, so a caller can say so instead of
// presenting a truncated list as complete.
func eachPage(fetch func(token string) (next string, more bool, err error)) (truncated bool, err error) {
	token := ""
	for page := 0; page < maxPages; page++ {
		next, more, err := fetch(token)
		if err != nil {
			return false, err
		}
		if !more || next == "" {
			return false, nil
		}
		token = next
	}
	return true, nil
}
