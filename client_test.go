package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest is one call the fake API saw.
type recordedRequest struct {
	Method string
	Path   string
	// RequestURI is the raw target as it arrived, before Go decodes escapes.
	// Path alone cannot show whether an object name was escaped, because %2F
	// decodes back into the slash it was hiding.
	RequestURI string
	Query      string
	Body       string
	Header     http.Header
}

// fakePlayAPI is an httptest server that records every request and answers from
// a handler the test supplies.
type fakePlayAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
}

func newFakePlayAPI(t *testing.T, handler http.HandlerFunc) *fakePlayAPI {
	t.Helper()
	api := &fakePlayAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record only the head of the body and hand the rest back untouched:
		// the upload tests stream tens of megabytes through here, and
		// consuming the body would leave the handler with nothing to write.
		head, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))
		body := head
		api.mu.Lock()
		api.requests = append(api.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, RequestURI: r.RequestURI, Query: r.URL.RawQuery,
			Body: string(body), Header: r.Header.Clone(),
		})
		api.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(api.Close)
	return api
}

func (a *fakePlayAPI) seen() []recordedRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]recordedRequest, len(a.requests))
	copy(out, a.requests)
	return out
}

// calls renders the request log as "METHOD /path" strings, for assertions that
// care about the sequence rather than the details.
func (a *fakePlayAPI) calls() []string {
	seen := a.seen()
	out := make([]string, len(seen))
	for i, r := range seen {
		out[i] = r.Method + " " + r.Path
	}
	return out
}

func (a *fakePlayAPI) sawCall(method, path string) bool {
	for _, c := range a.calls() {
		if c == method+" "+path {
			return true
		}
	}
	return false
}

// newTestClient builds a Client pointed at a fake API. A loopback base URL puts
// the config in test mode, so the token source is static and nothing reaches
// the network.
func newTestClient(t *testing.T, api *fakePlayAPI) *Client {
	t.Helper()
	clearPlayEnv(t)
	cfg := &PlayConfig{PackageName: "com.example.app"}
	cfg.BaseURL = api.URL
	cfg.ReportingBaseURL = api.URL
	// Every endpoint points at the fake, including the one no test in this file
	// uses: a base URL left at its default would send an offline unit test to
	// the real Google the moment a handler started calling it.
	cfg.StorageBaseURL = api.URL
	client, err := NewClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// shrinkBackoff makes the retry tests run in milliseconds rather than seconds.
func shrinkBackoff(t *testing.T) {
	t.Helper()
	original := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = original })
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestClientSendsVersionedPathAndHeaders(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
	})
	client := newTestClient(t, api)

	var edit appEdit
	if err := client.do(context.Background(), http.MethodGet, "applications/com.example.app/edits/edit-1", nil, nil, &edit); err != nil {
		t.Fatalf("do: %v", err)
	}
	if edit.ID != "edit-1" {
		t.Errorf("decoded %+v", edit)
	}

	req := api.seen()[0]
	if req.Path != "/androidpublisher/v3/applications/com.example.app/edits/edit-1" {
		t.Errorf("request path = %q", req.Path)
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "rollout/") {
		t.Errorf("User-Agent = %q, want it to identify rollout", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-access-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
}

func TestAPIErrorEnvelopeIsParsed(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantReason string
		wantHint   string
	}{
		{
			name:       "permission denied points at the console page",
			status:     http.StatusForbidden,
			body:       `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED","errors":[{"reason":"insufficientPermissions","message":"Insufficient Permission"}]}}`,
			wantReason: "insufficientPermissions",
			wantHint:   "Users & permissions",
		},
		{
			name:       "not found explains invisible new apps",
			status:     http.StatusNotFound,
			body:       `{"error":{"code":404,"message":"Package not found","status":"NOT_FOUND"}}`,
			wantReason: "NOT_FOUND",
			wantHint:   "uploaded artifact",
		},
		{
			name:       "a committed edit says to re-run",
			status:     http.StatusConflict,
			body:       `{"error":{"code":409,"message":"Edit has already been committed","status":"FAILED_PRECONDITION","errors":[{"reason":"editAlreadyCommitted","message":"Edit is already committed"}]}}`,
			wantReason: "editAlreadyCommitted",
			wantHint:   "fresh edit",
		},
		{
			name:       "quota names the real limits",
			status:     http.StatusTooManyRequests,
			body:       `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`,
			wantReason: "RESOURCE_EXHAUSTED",
			wantHint:   "200,000 requests/day",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := parseAPIError(tc.status, []byte(tc.body))
			var apiErr *apiError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected an *apiError, got %T", err)
			}
			if apiErr.Status != tc.status || apiErr.Reason != tc.wantReason {
				t.Errorf("parsed %+v", apiErr)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("error should name the fix (%q): %v", tc.wantHint, err)
			}
			// doctor classifies by this, and a 4xx must read as a broken setup.
			if !apiErr.isClientError() {
				t.Errorf("%d should be a definitive client error", tc.status)
			}
		})
	}
}

// TestAPIErrorFallsBackToTheRawBody: an HTML error page from a proxy is not
// Google's envelope, and swallowing it would leave the user with nothing.
func TestAPIErrorFallsBackToTheRawBody(t *testing.T) {
	err := parseAPIError(http.StatusBadGateway, []byte("<html>bad gateway</html>"))
	if !strings.Contains(err.Error(), "bad gateway") {
		t.Errorf("error lost the body: %v", err)
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.isClientError() {
		t.Errorf("502 should not be a client error: %v", err)
	}
}

// TestRetriesTransientFailures: a 429 with Retry-After is the API asking us to
// wait, not to give up.
func TestRetriesTransientFailures(t *testing.T) {
	shrinkBackoff(t)
	var attempts int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "0")
			writeJSON(w, http.StatusTooManyRequests, `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"id":"edit-1"}`)
	})
	client := newTestClient(t, api)

	var edit appEdit
	if err := client.do(context.Background(), http.MethodGet, "applications/com.example.app/edits/edit-1", nil, nil, &edit); err != nil {
		t.Fatalf("do: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want the call to have been retried", attempts)
	}
}

func TestRetriesGiveUpAndReportTheAPIError(t *testing.T) {
	shrinkBackoff(t)
	var attempts int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		writeJSON(w, http.StatusServiceUnavailable, `{"error":{"code":503,"message":"Backend error","status":"UNAVAILABLE"}}`)
	})
	client := newTestClient(t, api)

	err := client.do(context.Background(), http.MethodGet, "applications/com.example.app/edits/edit-1", nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if attempts != retryMaxAttempts {
		t.Errorf("attempts = %d, want %d", attempts, retryMaxAttempts)
	}
	if !strings.Contains(err.Error(), "Backend error") {
		t.Errorf("error lost the API message: %v", err)
	}
}

// TestMutationsAreNeverRetried is the rule a publishing API makes non-negotiable:
// a 5xx may mean the server did the work and lost the response, and a retried
// commit publishes twice.
func TestMutationsAreNeverRetried(t *testing.T) {
	shrinkBackoff(t)
	var attempts int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		writeJSON(w, http.StatusServiceUnavailable, `{"error":{"code":503,"message":"Backend error","status":"UNAVAILABLE"}}`)
	})
	client := newTestClient(t, api)

	e := &editSession{c: client, pkg: "com.example.app", id: "edit-1"}
	if _, err := e.commit(context.Background(), commitOptions{}); err == nil {
		t.Fatal("expected the commit to fail")
	}
	if attempts != 1 {
		t.Errorf("commit was attempted %d times — it must never be retried", attempts)
	}
}

// TestRateLimitedMutationIsAlsoNotRetried: even a 429, which is safe to repeat
// in principle, is left to the caller for a publishing call.
func TestRateLimitedMutationIsAlsoNotRetried(t *testing.T) {
	shrinkBackoff(t)
	var attempts int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		writeJSON(w, http.StatusTooManyRequests, `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`)
	})
	client := newTestClient(t, api)

	if _, err := client.openEdit(context.Background(), "com.example.app"); err == nil {
		t.Fatal("expected the insert to fail")
	}
	if attempts != 1 {
		t.Errorf("edits.insert was attempted %d times", attempts)
	}
}

func TestBackoffDelayHonoursRetryAfterAndCaps(t *testing.T) {
	original := retryBaseDelay
	retryBaseDelay = time.Second
	t.Cleanup(func() { retryBaseDelay = original })

	if got := backoffDelay(1, "5"); got != 5*time.Second {
		t.Errorf("Retry-After should win: %v", got)
	}
	// A server asking for an hour must not stall a CLI run for an hour.
	if got := backoffDelay(1, "3600"); got > 30*time.Second {
		t.Errorf("delay = %v, want it capped", got)
	}
	// Exponential with jitter, still bounded.
	if got := backoffDelay(3, ""); got < 4*time.Second || got > 30*time.Second {
		t.Errorf("delay = %v, want between the base backoff and the cap", got)
	}
	if got := backoffDelay(20, ""); got > 30*time.Second {
		t.Errorf("delay = %v, want it capped", got)
	}
}

func TestPagingConcatenatesAndReportsTruncation(t *testing.T) {
	t.Run("follows both pagination shapes", func(t *testing.T) {
		// The Publisher API nests the token; the Reporting API does not.
		for _, body := range []string{`{"nextPageToken":"p2"}`, `{"tokenPagination":{"nextPageToken":"p2"}}`} {
			var page pagedResponse
			if err := json.Unmarshal([]byte(body), &page); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if page.next() != "p2" {
				t.Errorf("next token from %s = %q", body, page.next())
			}
		}
	})

	t.Run("stops at the end of the results", func(t *testing.T) {
		var pages int
		truncated, err := eachPage(func(token string) (string, bool, error) {
			pages++
			if pages == 3 {
				return "", true, nil
			}
			return fmt.Sprintf("page-%d", pages), true, nil
		})
		if err != nil || truncated {
			t.Fatalf("eachPage: %v, truncated=%v", err, truncated)
		}
		if pages != 3 {
			t.Errorf("walked %d pages", pages)
		}
	})

	t.Run("reports a capped walk instead of silently truncating", func(t *testing.T) {
		var pages int
		truncated, err := eachPage(func(string) (string, bool, error) {
			pages++
			return "more", true, nil
		})
		if err != nil {
			t.Fatalf("eachPage: %v", err)
		}
		if !truncated {
			t.Error("a walk that hit the page cap must say so")
		}
		if pages != maxPages {
			t.Errorf("walked %d pages, want the cap of %d", pages, maxPages)
		}
	})
}

func TestResolvePackageThroughTheClient(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, `{}`) })
	client := newTestClient(t, api)

	if got, err := client.resolvePackage(""); err != nil || got != "com.example.app" {
		t.Errorf("resolvePackage() = %q, %v", got, err)
	}
	if got, err := client.resolvePackage("com.other.app"); err != nil || got != "com.other.app" {
		t.Errorf("resolvePackage(explicit) = %q, %v", got, err)
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(parseAPIError(http.StatusNotFound, []byte(`{"error":{"message":"nope"}}`))) {
		t.Error("a 404 should be recognized")
	}
	if isNotFound(parseAPIError(http.StatusForbidden, []byte(`{"error":{"message":"nope"}}`))) {
		t.Error("a 403 is not a 404")
	}
	if isNotFound(fmt.Errorf("dial tcp: refused")) {
		t.Error("a transport error is not a 404")
	}
}

func TestClientPlatformName(t *testing.T) {
	// The confirm path routes a staged token by this name.
	if (&Client{}).platformName() != playPlatformName {
		t.Error("the Play client must identify itself as the play platform")
	}
}

// TestNewPlayClientResolvesConfiguration is the entry point every CLI command
// and MCP tool goes through, so a config that doesn't resolve has to fail here
// rather than at the first request.
func TestNewPlayClientResolvesConfiguration(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, `{}`) })

	clearPlayEnv(t)
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })
	t.Setenv("PLAY_API_BASE_URL", api.URL)
	t.Setenv("PLAY_PACKAGE_NAME", "com.example.app")

	client, err := newPlayClient(context.Background())
	if err != nil {
		t.Fatalf("newPlayClient: %v", err)
	}
	if got, _ := client.resolvePackage(""); got != "com.example.app" {
		t.Errorf("configured package = %q", got)
	}

	// Without credentials and without test mode, the client must refuse to
	// build instead of failing later with an opaque auth error.
	t.Setenv("PLAY_API_BASE_URL", defaultPlayBaseURL)
	if _, err := newPlayClient(context.Background()); err == nil {
		t.Error("expected an unconfigured client to fail to build")
	}
}
