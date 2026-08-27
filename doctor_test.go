package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// stubAPIError stands in for a platform's API error type: doctor classifies by
// the isClientError interface, never by any concrete error.
type stubAPIError struct {
	status int
}

func (e *stubAPIError) Error() string       { return fmt.Sprintf("api error %d", e.status) }
func (e *stubAPIError) isClientError() bool { return e.status >= 400 && e.status < 500 }
func (e *stubAPIError) isThrottled() bool   { return e.status == http.StatusTooManyRequests }

func TestLiveVerdictFor(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want liveResult
	}{
		{"no error is ready", nil, liveOK},
		{"403 from the API is definitive", &stubAPIError{status: http.StatusForbidden}, liveFailed},
		{"404 from the API is definitive", &stubAPIError{status: http.StatusNotFound}, liveFailed},
		// A 500 means we could not get a verdict. Reporting it as a broken
		// setup would send users to re-check credentials that are fine.
		{"500 from the API is inconclusive", &stubAPIError{status: http.StatusInternalServerError}, liveInconclusive},
		// The exception to the 4xx rule: a quota is not a verdict about the
		// credential, and the same call succeeds once the window moves.
		{"429 from the API is rate limiting, not a rejection", &stubAPIError{status: http.StatusTooManyRequests}, liveInconclusive},
		{"transport failure is inconclusive", errors.New("dial tcp: connection refused"), liveInconclusive},
		{
			name: "invalid_grant from the token endpoint is definitive",
			err:  &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}},
			want: liveFailed,
		},
		{
			// Rate limiting on the token endpoint is not a broken setup.
			name: "429 from the token endpoint is inconclusive",
			err:  &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
			want: liveInconclusive,
		},
		{
			name: "a wrapped API error is still classified",
			err:  fmt.Errorf("probe edits: %w", &stubAPIError{status: http.StatusUnauthorized}),
			want: liveFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := liveVerdictFor(tc.err); got != tc.want {
				t.Errorf("liveVerdictFor(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestLiveResultSeverityOrder(t *testing.T) {
	// A multi-platform doctor run exits on its worst result, so the ordering is
	// load-bearing, not cosmetic.
	ordered := []liveResult{liveOK, liveOffline, liveUnverified, liveInconclusive, liveFailed, liveUnconfigured}
	for i := 1; i < len(ordered); i++ {
		if !ordered[i].worseThan(ordered[i-1]) {
			t.Errorf("%v should rank worse than %v", ordered[i], ordered[i-1])
		}
	}
}

func TestStatusLineVerdicts(t *testing.T) {
	tests := []struct {
		res  liveResult
		err  error
		want string
	}{
		{liveOK, nil, "READY"},
		{liveOffline, nil, "READY"},
		{liveUnverified, nil, "READY"},
		{liveUnconfigured, errors.New("set PLAY_SERVICE_ACCOUNT_FILE"), "NOT READY — set PLAY_SERVICE_ACCOUNT_FILE"},
		{liveFailed, nil, "NOT READY"},
		{liveInconclusive, nil, "INCONCLUSIVE"},
	}
	for _, tc := range tests {
		if got := statusLine(styles{}, tc.res, tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("statusLine(%v) = %q, want it to contain %q", tc.res, got, tc.want)
		}
	}
}

func TestPlatformVerdictExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		verdict  platformVerdict
		wantCode int // 0 means "no error returned"
	}{
		{"ready exits clean", platformVerdict{result: liveOK}, 0},
		{"offline exits clean", platformVerdict{result: liveOffline}, 0},
		// A credential Google accepts, with nothing to probe it against, is not
		// something CI should fail over.
		{"unverified exits clean", platformVerdict{result: liveUnverified}, 0},
		{"inconclusive exits 2", platformVerdict{result: liveInconclusive, err: errors.New("unreachable")}, 2},
		{"failed exits 1", platformVerdict{result: liveFailed, err: errors.New("rejected")}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.verdict.exit()
			if tc.wantCode == 0 {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			var ex *exitErr
			if !errors.As(err, &ex) {
				t.Fatalf("expected an exitErr, got %v", err)
			}
			if ex.code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", ex.code, tc.wantCode)
			}
		})
	}
}

// TestPlatformVerdictUnconfiguredSurfacesError is the one verdict whose error
// the user must see: nothing has been printed above it to explain the failure.
func TestPlatformVerdictUnconfiguredSurfacesError(t *testing.T) {
	want := errors.New("set PLAY_SERVICE_ACCOUNT_FILE or run `rollout login play`")
	err := (&platformVerdict{result: liveUnconfigured, err: want}).exit()
	if !errors.Is(err, want) {
		t.Errorf("unconfigured verdict lost its error: %v", err)
	}
}

func TestReportProbeMarkers(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantMarker string
		want       liveResult
	}{
		{"definitive failures are marked ✗", &stubAPIError{status: http.StatusForbidden}, "✗", liveFailed},
		{"unreachable API is marked ?", errors.New("connection refused"), "?", liveInconclusive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := reportProbe(&buf, "edit probe        ", tc.err)
			if got != tc.want {
				t.Errorf("verdict = %v, want %v", got, tc.want)
			}
			if !strings.Contains(buf.String(), tc.wantMarker) {
				t.Errorf("line %q should contain %q", buf.String(), tc.wantMarker)
			}
		})
	}
}

func TestConfiguredPlatformsSkipsUnconfigured(t *testing.T) {
	configured := &Platform{Name: "a", Configured: func() bool { return true }}
	unconfigured := &Platform{Name: "b", Configured: func() bool { return false }}
	// A platform that does not answer the question counts as configured, so an
	// omitted hook can never hide it from `rollout doctor`.
	silent := &Platform{Name: "c"}

	targets, skipped := configuredPlatforms([]*Platform{configured, unconfigured, silent})
	if len(targets) != 2 || targets[0] != configured || targets[1] != silent {
		t.Fatalf("unexpected targets: %v", targets)
	}
	if len(skipped) != 1 || skipped[0] != "b" {
		t.Errorf("expected b to be skipped, got %v", skipped)
	}
}

// TestConfiguredPlatformsFallsBackToAll is the new-user case: `rollout doctor`
// with nothing set up must report what to set up, not print an empty report.
func TestConfiguredPlatformsFallsBackToAll(t *testing.T) {
	none := []*Platform{
		{Name: "a", Configured: func() bool { return false }},
		{Name: "b", Configured: func() bool { return false }},
	}
	targets, skipped := configuredPlatforms(none)
	if len(targets) != 2 {
		t.Errorf("expected every platform to be checked, got %d", len(targets))
	}
	if len(skipped) != 0 {
		t.Errorf("nothing should be reported as skipped, got %v", skipped)
	}
}
