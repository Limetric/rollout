package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testServiceAccountJSON is a syntactically complete key. It is never used to
// sign anything: a loopback base URL puts the config in test mode, where the
// token source is static and no network auth happens.
const testServiceAccountJSON = `{"type":"service_account","project_id":"p","client_email":"bot@p.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nnot-a-real-key\n-----END PRIVATE KEY-----\n"}`

// playDoctorAgainst runs the Play doctor against a fake API and returns its
// verdict plus everything it printed.
func playDoctorAgainst(t *testing.T, baseURL string, offline bool) (liveResult, string, error) {
	t.Helper()
	playDoctorEnv(t, baseURL)
	t.Setenv("PLAY_PACKAGE_NAME", "com.example.app")

	var out bytes.Buffer
	res, err := playDoctor(context.Background(), &out, offline)
	return res, out.String(), err
}

// playDoctorEnv points every credential and every endpoint at the test's own
// fixtures. All three base URLs matter: doctor probes the Publisher API, the
// Reporting API and Cloud Storage, and one left at its default would send an
// offline unit test to the real Google.
func playDoctorEnv(t *testing.T, baseURL string) {
	t.Helper()
	clearPlayEnv(t)
	// A probe that gets a 5xx retries with backoff; at the real delay the
	// inconclusive cases alone would dominate the package's test time.
	originalDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = originalDelay })
	t.Setenv("PLAY_SERVICE_ACCOUNT_JSON", testServiceAccountJSON)
	t.Setenv("PLAY_API_BASE_URL", baseURL)
	t.Setenv("PLAY_REPORTING_BASE_URL", baseURL)
	t.Setenv("PLAY_STORAGE_BASE_URL", baseURL)

	// `rollout doctor` reads the global --config flag; keep it pointed at
	// nothing so a real config file cannot leak in.
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })
}

func TestPlayDoctorVerdicts(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		want       liveResult
		wantOutput []string
	}{
		{
			name:       "an app the credential can edit is ready",
			status:     http.StatusOK,
			body:       `{"id":"edit-1","expiryTimeSeconds":"1700000000"}`,
			want:       liveOK,
			wantOutput: []string{"publish probe", "edit-1", "com.example.app"},
		},
		{
			// The single most common setup failure: the key works, but nobody
			// invited it in the Console.
			name:       "403 points at Users & permissions",
			status:     http.StatusForbidden,
			body:       `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`,
			want:       liveFailed,
			wantOutput: []string{"Users & permissions"},
		},
		{
			name:       "404 explains that new apps are invisible to the API",
			status:     http.StatusNotFound,
			body:       `{"error":{"code":404,"message":"Package not found: com.example.app","status":"NOT_FOUND"}}`,
			want:       liveFailed,
			wantOutput: []string{"com.example.app", "uploaded artifact"},
		},
		{
			// A 500 means we could not get a verdict. Calling that a broken
			// setup would send the user to re-check credentials that are fine.
			name:   "500 is inconclusive, not broken",
			status: http.StatusInternalServerError,
			body:   `{"error":{"code":500,"message":"Internal error","status":"INTERNAL"}}`,
			want:   liveInconclusive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var deleted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleted = append(deleted, r.URL.Path)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			res, output, err := playDoctorAgainst(t, srv.URL, false)
			if res != tc.want {
				t.Fatalf("verdict = %v (err %v), want %v\n%s", res, err, tc.want, output)
			}
			for _, want := range tc.wantOutput {
				if !strings.Contains(output, want) {
					t.Errorf("doctor output missing %q:\n%s", want, output)
				}
			}
			// A probe that opened an edit must clean it up: leaving one behind
			// on every doctor run would litter the app's edit list.
			if tc.want == liveOK && len(deleted) != 1 {
				t.Errorf("expected the probe edit to be deleted, saw %v", deleted)
			}
		})
	}
}

// TestPlayDoctorUnreachableAPIIsInconclusive: a connection failure says nothing
// about whether the credentials are right.
func TestPlayDoctorUnreachableAPIIsInconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	res, output, _ := playDoctorAgainst(t, url, false)
	if res != liveInconclusive {
		t.Fatalf("verdict = %v, want inconclusive\n%s", res, output)
	}
}

// TestPlayDoctorOfflineNeedsOnlyAKeyAndPackage is issue #2's acceptance: the
// offline check must pass with nothing but a key and a package name.
func TestPlayDoctorOfflineNeedsOnlyAKeyAndPackage(t *testing.T) {
	res, output, err := playDoctorAgainst(t, "", true)
	if err != nil {
		t.Fatalf("offline doctor: %v\n%s", err, output)
	}
	if res != liveOffline {
		t.Fatalf("verdict = %v, want offline-ready\n%s", res, output)
	}
	for _, want := range []string{"serviceAccount", "bot@p.iam.gserviceaccount.com", "com.example.app"} {
		if !strings.Contains(output, want) {
			t.Errorf("doctor output missing %q:\n%s", want, output)
		}
	}
}

// TestPlayDoctorNeverPrintsThePrivateKey guards the whole point of redaction:
// doctor output lands in terminal scrollback and CI logs.
func TestPlayDoctorNeverPrintsThePrivateKey(t *testing.T) {
	_, output, _ := playDoctorAgainst(t, "", true)
	for _, secret := range []string{"not-a-real-key", "BEGIN PRIVATE KEY"} {
		if strings.Contains(output, secret) {
			t.Fatalf("doctor leaked the private key:\n%s", output)
		}
	}
}

func TestPlayDoctorRejectsAnUnparseableKey(t *testing.T) {
	clearPlayEnv(t)
	t.Setenv("PLAY_SERVICE_ACCOUNT_JSON", `{"installed":{"client_id":"abc"}}`)
	t.Setenv("PLAY_PACKAGE_NAME", "com.example.app")
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })

	var out bytes.Buffer
	// Offline, so the only thing that can fail is the key itself — and it must
	// be reported as a key problem, not as an unreachable API.
	res, err := playDoctor(context.Background(), &out, true)
	if res != liveUnconfigured {
		t.Fatalf("verdict = %v, want unconfigured (err %v)", res, err)
	}
	if err == nil || !strings.Contains(err.Error(), "client_email") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

// playFake serves the three services doctor probes, each with its own canned
// response, so a test can make one surface healthy and another refuse — which
// is the whole point of probing them separately.
type playFake struct {
	editStatus, appsStatus, objectsStatus int
	editBody, appsBody, objectsBody       string
	appsCalls, objectCalls                int
}

func (f *playFake) start(t *testing.T) string {
	t.Helper()
	status := func(code int) int {
		if code == 0 {
			return http.StatusOK
		}
		return code
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "apps:search"):
			f.appsCalls++
			w.WriteHeader(status(f.appsStatus))
			_, _ = w.Write([]byte(orDefault(f.appsBody, `{"apps":[]}`)))
		case strings.HasPrefix(r.URL.Path, "/"+storageAPIPath+"/"):
			f.objectCalls++
			w.WriteHeader(status(f.objectsStatus))
			_, _ = w.Write([]byte(orDefault(f.objectsBody, `{"items":[]}`)))
		default:
			w.WriteHeader(status(f.editStatus))
			_, _ = w.Write([]byte(orDefault(f.editBody, `{"id":"edit-1"}`)))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

const (
	permissionDeniedBody = `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`
	unauthenticatedBody  = `{"error":{"code":401,"message":"Request had invalid authentication credentials","status":"UNAUTHENTICATED"}}`
)

// TestPlayDoctorReportingGapDoesNotBreakTheVerdict: Reporting is a separate
// service with a separate grant, so a credential that publishes but cannot read
// it is a working setup — the gap is reported, the verdict stays READY.
func TestPlayDoctorReportingGapDoesNotBreakTheVerdict(t *testing.T) {
	fake := &playFake{appsStatus: http.StatusForbidden, appsBody: permissionDeniedBody}
	res, output, err := playDoctorAgainst(t, fake.start(t), false)
	if res != liveOK {
		t.Fatalf("verdict = %v (err %v), want ready\n%s", res, err, output)
	}
	for _, want := range []string{"publishing is unaffected", "View app information"} {
		if !strings.Contains(output, want) {
			t.Errorf("doctor output missing %q:\n%s", want, output)
		}
	}
}

// TestPlayDoctorProbesReporting: the vitals tools live on a different service
// from every write, and a doctor that never calls it cannot vouch for them.
func TestPlayDoctorProbesReporting(t *testing.T) {
	fake := &playFake{appsBody: `{"apps":[{"packageName":"com.example.app"},{"packageName":"com.example.other"}]}`}
	res, output, err := playDoctorAgainst(t, fake.start(t), false)
	if res != liveOK {
		t.Fatalf("verdict = %v (err %v)\n%s", res, err, output)
	}
	if fake.appsCalls != 1 {
		t.Errorf("reporting probe made %d calls, want 1", fake.appsCalls)
	}
	if !strings.Contains(output, "2 apps visible") {
		t.Errorf("doctor should report what Reporting can see:\n%s", output)
	}
}

// TestPlayDoctorWithoutAPackage covers the fallback: with no app to open an
// edit on, Reporting is the only evidence available, and what it says decides
// how much doctor may claim.
func TestPlayDoctorWithoutAPackage(t *testing.T) {
	tests := []struct {
		name       string
		fake       playFake
		want       liveResult
		wantOutput []string
	}{
		{
			// The credential works, but nothing proved it may publish to any
			// particular app — so doctor says exactly that.
			name:       "a listing proves the credential without proving publish access",
			fake:       playFake{appsBody: `{"apps":[{"packageName":"com.example.app"}]}`},
			want:       liveUnverified,
			wantOutput: []string{"com.example.app", "set-package"},
		},
		{
			// 403 means the token was accepted and only the grant is missing:
			// the credential is real, which is all we can honestly claim.
			name:       "a permission denial still proves the credential is real",
			fake:       playFake{appsStatus: http.StatusForbidden, appsBody: permissionDeniedBody},
			want:       liveUnverified,
			wantOutput: []string{"publishing is unaffected"},
		},
		{
			// 401 is the credential itself being rejected — that is broken.
			name: "a rejected credential is not ready",
			fake: playFake{appsStatus: http.StatusUnauthorized, appsBody: unauthenticatedBody},
			want: liveFailed,
		},
		{
			name: "an unreachable Reporting API is inconclusive",
			fake: playFake{appsStatus: http.StatusInternalServerError, appsBody: `{"error":{"code":500,"message":"Internal error","status":"INTERNAL"}}`},
			want: liveInconclusive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake
			playDoctorEnv(t, fake.start(t))

			var out bytes.Buffer
			res, err := playDoctor(context.Background(), &out, false)
			output := out.String()
			if res != tc.want {
				t.Fatalf("verdict = %v (err %v), want %v\n%s", res, err, tc.want, output)
			}
			if !strings.Contains(output, "skipped") {
				t.Errorf("doctor should say the publish probe was skipped:\n%s", output)
			}
			for _, want := range tc.wantOutput {
				if !strings.Contains(output, want) {
					t.Errorf("doctor output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

// TestPlayDoctorProbesTheReportsBucketOnlyWhenConfigured: asking about a bucket
// nobody set is noise, and a bucket that was set is a capability the user
// explicitly asked for.
func TestPlayDoctorProbesTheReportsBucketOnlyWhenConfigured(t *testing.T) {
	t.Run("unset means unprobed", func(t *testing.T) {
		fake := &playFake{}
		if res, out, err := playDoctorAgainst(t, fake.start(t), false); res != liveOK {
			t.Fatalf("verdict = %v (err %v)\n%s", res, err, out)
		}
		if fake.objectCalls != 0 {
			t.Errorf("doctor read a bucket nobody configured (%d calls)", fake.objectCalls)
		}
	})

	t.Run("a readable bucket is reported", func(t *testing.T) {
		fake := &playFake{}
		playDoctorAgainst(t, fake.start(t), true) // seed env, offline: no probes
		t.Setenv("PLAY_REPORTS_BUCKET", "pubsite_prod_rev_123")
		t.Setenv("PLAY_PACKAGE_NAME", "com.example.app")

		var out bytes.Buffer
		res, err := playDoctor(context.Background(), &out, false)
		if res != liveOK {
			t.Fatalf("verdict = %v (err %v)\n%s", res, err, out.String())
		}
		if fake.objectCalls != 1 {
			t.Errorf("bucket probe made %d calls, want 1", fake.objectCalls)
		}
		if !strings.Contains(out.String(), "gs://pubsite_prod_rev_123") {
			t.Errorf("doctor should name the bucket it read:\n%s", out.String())
		}
	})

	t.Run("a refused bucket is a broken setup", func(t *testing.T) {
		fake := &playFake{objectsStatus: http.StatusForbidden, objectsBody: permissionDeniedBody}
		playDoctorAgainst(t, fake.start(t), true)
		t.Setenv("PLAY_REPORTS_BUCKET", "pubsite_prod_rev_123")
		t.Setenv("PLAY_PACKAGE_NAME", "com.example.app")

		var out bytes.Buffer
		res, err := playDoctor(context.Background(), &out, false)
		if res != liveFailed {
			t.Fatalf("verdict = %v (err %v), want failed\n%s", res, err, out.String())
		}
	})
}

// TestReportsBucketHintNamesTheRightFix: for a signed-in user the likeliest
// cause is invisible in the config — the saved token predates the scope — and
// no amount of re-configuring fixes it.
func TestReportsBucketHintNamesTheRightFix(t *testing.T) {
	refused := &apiError{Status: http.StatusForbidden, Message: "denied"}

	oauth := &PlayConfig{ClientID: "id", ClientSecret: "secret", ReportsBucket: "b"}
	if got := reportsBucketHint(refused, oauth).Error(); !strings.Contains(got, "rollout login play") {
		t.Errorf("a signed-in user should be told to re-consent: %s", got)
	}

	sa := &PlayConfig{ServiceAccountFile: "/keys/play.json", ReportsBucket: "b"}
	if got := reportsBucketHint(refused, sa).Error(); !strings.Contains(got, "Users & permissions") {
		t.Errorf("a service account should be told where to grant access: %s", got)
	}

	// A failure that is not a refusal must pass through untouched: inventing a
	// permissions story for a 500 sends the user to fix what is not broken.
	transient := &apiError{Status: http.StatusInternalServerError, Message: "boom"}
	if got := reportsBucketHint(transient, oauth); got != error(transient) {
		t.Errorf("a 500 should pass through unchanged, got %v", got)
	}
}

// TestPlayDoctorSendsTheAuthHeaderAndUserAgent: the probe has to look like a
// real call, or it proves nothing about a real call.
func TestPlayDoctorSendsTheAuthHeaderAndUserAgent(t *testing.T) {
	var gotAuth, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gotAuth, gotUA, gotPath = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"edit-1"}`))
	}))
	defer srv.Close()

	if res, out, err := playDoctorAgainst(t, srv.URL, false); res != liveOK {
		t.Fatalf("verdict = %v (err %v)\n%s", res, err, out)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("probe sent Authorization %q", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "rollout/") {
		t.Errorf("probe sent User-Agent %q, want it to identify rollout", gotUA)
	}
	if gotPath != "/androidpublisher/v3/applications/com.example.app/edits" {
		t.Errorf("probe called %q", gotPath)
	}
}
