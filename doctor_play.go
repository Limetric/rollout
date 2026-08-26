package main

import (
	"context"
	"fmt"
	"io"
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
// There is no cheaper probe. `edits` cannot be listed, and every read endpoint
// needs an edit anyway, so an insert + delete is both the smallest call that
// touches the publishing API and the one that proves what actually matters:
// that the credential is linked to a Play Console account *and* has access to
// this specific app. A token that mints fine can still belong to a service
// account nobody invited.
func runPlayDoctorLive(ctx context.Context, out io.Writer, cfg *PlayConfig) (liveResult, error) {
	pkg, err := cfg.resolvePackage("")
	if err != nil {
		fmt.Fprintf(out, "edit probe:          skipped (%v)\n", err)
		// Credentials resolve and nothing was rejected; there is simply no app
		// to probe. Reporting that as broken would be wrong.
		return liveOffline, nil
	}

	client, err := NewClient(ctx, cfg)
	if err != nil {
		return reportProbe(out, "credentials:        ", err), err
	}

	// Read mode: withEdit deletes the edit on the way out, so a doctor run
	// leaves nothing behind in the app's edit list.
	editID, err := client.withEdit(ctx, pkg, false, commitOptions{}, func(*editSession) error { return nil })
	if err != nil {
		return reportProbe(out, "edit probe:         ", err), err
	}
	fmt.Fprintf(out, "edit probe:          ✓ opened and deleted edit %s on %s\n", editID, pkg)
	return liveOK, nil
}

// playUserAgent identifies this binary to the API, so Google's quota and abuse
// tooling can attribute traffic to rollout rather than to "Go-http-client".
func playUserAgent() string { return "rollout/" + versionString() }
