package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testServiceAccountJSON is a syntactically complete key. It is never used to
// sign anything: a loopback base URL puts the config in test mode, where the
// token source is static and no network auth happens.
const testServiceAccountJSON = `{"type":"service_account","project_id":"p","client_email":"bot@p.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nnot-a-real-key\n-----END PRIVATE KEY-----\n"}`

// playDoctorAgainst runs the Play doctor against a fake API and returns its
// verdict plus everything it printed.
func playDoctorAgainst(t *testing.T, baseURL string, offline bool) (liveResult, string, error) {
	t.Helper()
	clearPlayEnv(t)
	t.Setenv("PLAY_SERVICE_ACCOUNT_JSON", testServiceAccountJSON)
	t.Setenv("PLAY_PACKAGE_NAME", "com.example.app")
	t.Setenv("PLAY_API_BASE_URL", baseURL)

	// `rollout doctor` reads the global --config flag; keep it pointed at
	// nothing so a real config file cannot leak in.
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })

	var out bytes.Buffer
	res, err := playDoctor(context.Background(), &out, offline)
	return res, out.String(), err
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
			wantOutput: []string{"edit probe", "edit-1", "com.example.app"},
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

// TestPlayDoctorWithoutAPackageDoesNotFail: credentials resolve and nothing was
// rejected; there is simply no app to probe.
func TestPlayDoctorWithoutAPackageDoesNotFail(t *testing.T) {
	clearPlayEnv(t)
	t.Setenv("PLAY_SERVICE_ACCOUNT_JSON", testServiceAccountJSON)
	t.Setenv("PLAY_API_BASE_URL", "http://127.0.0.1:1")
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })

	var out bytes.Buffer
	res, err := playDoctor(context.Background(), &out, false)
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out.String())
	}
	if res != liveOffline {
		t.Fatalf("verdict = %v, want offline-ready\n%s", res, out.String())
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("doctor should say the probe was skipped:\n%s", out.String())
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
