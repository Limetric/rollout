package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// playDoctor is the Play platform's Platform.Doctor hook: it prints the
// resolved credential summary, then (unless offline) probes the live API.
func playDoctor(ctx context.Context, out io.Writer, offline bool) (liveResult, error) {
	cfg, err := loadPlayConfig(configPath)
	if err != nil {
		return liveUnconfigured, err
	}
	store := describeTokenStore(playTokenPolicy.Platform)
	fmt.Fprintf(out, "credential mode:    %s\n", playCredentialModeSummary(cfg))
	fmt.Fprintf(out, "service account:    %s\n", playServiceAccountSummary(cfg))
	fmt.Fprintf(out, "token store:        %s\n", store.location())
	fmt.Fprintf(out, "saved sign-in:      %s\n", store.describe(playTokenPolicy))
	fmt.Fprintf(out, "package name:       %s\n", orNone(cfg.PackageName))
	fmt.Fprintf(out, "developer id:       %s\n", orNone(cfg.DeveloperID))
	fmt.Fprintf(out, "reports bucket:     %s\n", orNone(cfg.ReportsBucket))
	fmt.Fprintf(out, "api base url:       %s\n", cfg.BaseURL)
	fmt.Fprintf(out, "reporting base url: %s\n", cfg.ReportingBaseURL)
	fmt.Fprintf(out, "scopes:             %s\n", strings.Join(cfg.scopes(), ", "))

	if err := cfg.validate(); err != nil {
		return liveUnconfigured, err
	}
	// A credential that cannot even be assembled would surface below as an
	// unreachable API rather than as the file — or missing sign-in — problem it
	// is, so it is checked here, offline, where the message can name the fix.
	switch cfg.credentialMode() {
	case credentialServiceAccount:
		if _, err := cfg.readServiceAccountKey(); err != nil {
			return liveUnconfigured, err
		}
	case credentialOAuthUser:
		if store.ReadErr != nil {
			return liveUnconfigured, store.ReadErr
		}
		if store.Token == nil {
			return liveUnconfigured, fmt.Errorf("an OAuth client is configured but nobody has signed in — run `%s`", playTokenPolicy.loginCommand())
		}
	}
	if offline {
		return liveOffline, nil
	}
	fmt.Fprintln(out)
	return runPlayDoctorLive(ctx, out, cfg)
}

// runPlayDoctorLive proves the setup works by opening and immediately deleting
// an edit on the configured app.
//
// There is no cheaper probe. `edits` cannot be listed, and the read endpoints
// all need an edit anyway, so an insert + delete is both the smallest call that
// touches the publishing API and the one that proves what actually matters:
// that the credential is linked to a Play Console account *and* has access to
// this specific app. A token that mints fine can still be a service account
// nobody invited.
func runPlayDoctorLive(ctx context.Context, out io.Writer, cfg *PlayConfig) (liveResult, error) {
	pkg, err := cfg.resolvePackage("")
	if err != nil {
		fmt.Fprintf(out, "edit probe:          skipped (%v)\n", err)
		// Credentials resolve and nothing was rejected; there is simply no app
		// to probe. Reporting that as broken would be wrong.
		return liveOffline, nil
	}

	client, err := newPlayHTTPClient(ctx, cfg)
	if err != nil {
		return reportProbe(out, "credentials:        ", err), err
	}

	editID, err := probeInsertEdit(ctx, client, cfg, pkg)
	if err != nil {
		return reportProbe(out, "edit probe:         ", err), err
	}
	// Best-effort cleanup: an abandoned edit expires on its own, but leaving
	// one behind on every doctor run would litter the app's edit list.
	probeDeleteEdit(ctx, client, cfg, pkg, editID)
	fmt.Fprintf(out, "edit probe:          ✓ opened and deleted edit %s on %s\n", editID, pkg)
	return liveOK, nil
}

func probeInsertEdit(ctx context.Context, client *http.Client, cfg *PlayConfig, pkg string) (string, error) {
	url := fmt.Sprintf("%s/androidpublisher/v3/applications/%s/edits", cfg.BaseURL, pkg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", playUserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", playProbeError(resp, pkg)
	}
	var edit struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&edit); err != nil {
		return "", fmt.Errorf("decode the edit the API returned: %w", err)
	}
	return edit.ID, nil
}

func probeDeleteEdit(ctx context.Context, client *http.Client, cfg *PlayConfig, pkg, editID string) {
	if editID == "" {
		return
	}
	url := fmt.Sprintf("%s/androidpublisher/v3/applications/%s/edits/%s", cfg.BaseURL, pkg, editID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", playUserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// playProbeError turns a rejected probe into the sentence that names the fix.
// The two failures that actually happen — a service account nobody invited, and
// a package that isn't under this developer account — are indistinguishable
// from Google's generic message, so doctor says which one it is.
func playProbeError(resp *http.Response, pkg string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	detail := playAPIErrorMessage(body)
	err := &playProbeStatusError{status: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		err.message = fmt.Sprintf("the API rejected the credential (%d %s) — invite the service account in Play Console → Users & permissions and grant it access to %s (releases need \"Release to production, exclude devices, and use Play App Signing\")", resp.StatusCode, detail, pkg)
	case http.StatusNotFound:
		err.message = fmt.Sprintf("app %s not found (%d %s) — check the package name, and note that a new app must be created in the Console and have one uploaded artifact before the API can see it", pkg, resp.StatusCode, detail)
	default:
		err.message = fmt.Sprintf("open an edit on %s: %d %s", pkg, resp.StatusCode, detail)
	}
	return err
}

// playAPIErrorMessage pulls the human-readable half out of Google's error
// envelope, falling back to the raw body when it is not one.
func playAPIErrorMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return trimmed
	}
	return http.StatusText(len(body))
}

// playProbeStatusError carries the HTTP status so doctor can tell a definitive
// rejection (4xx — the setup is wrong) from a transient one (5xx — we simply
// could not get a verdict).
type playProbeStatusError struct {
	status  int
	message string
}

func (e *playProbeStatusError) Error() string       { return e.message }
func (e *playProbeStatusError) isClientError() bool { return e.status >= 400 && e.status < 500 }

// playUserAgent identifies this binary to the API, so Google's quota and abuse
// tooling can attribute traffic to rollout rather than to "Go-http-client".
func playUserAgent() string { return "rollout/" + versionString() }
