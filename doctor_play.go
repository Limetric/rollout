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
	s := newStyles(out)
	store := describeTokenStore(playTokenPolicy.Platform)
	settings := []struct{ label, value string }{
		{"credential mode:   ", playCredentialModeSummary(cfg)},
		{"service account:   ", playServiceAccountSummary(cfg)},
		{"token store:       ", store.location()},
		{"saved sign-in:     ", store.describe(playTokenPolicy)},
		{"package name:      ", orNone(cfg.PackageName)},
		{"developer id:      ", orNone(cfg.DeveloperID)},
		{"reports bucket:    ", orNone(cfg.ReportsBucket)},
		{"api base url:      ", cfg.BaseURL},
		{"reporting base url:", cfg.ReportingBaseURL},
		{"scopes:            ", strings.Join(cfg.scopes(), ", ")},
	}
	for _, set := range settings {
		fmt.Fprintf(out, "%s %s\n", s.label(set.label), s.value(set.value))
	}

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
	s := newStyles(out)
	pkg, err := cfg.resolvePackage("")
	if err != nil {
		fmt.Fprintf(out, "%s %s\n", s.label("edit probe:         "), s.warning(fmt.Sprintf("skipped (%v)", err)))
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
	fmt.Fprintf(out, "%s %s opened and deleted edit %s on %s\n", s.label("edit probe:         "), s.markOK(), editID, s.accent(pkg))
	return liveOK, nil
}

// playUserAgent identifies this binary to the API, so Google's quota and abuse
// tooling can attribute traffic to rollout rather than to "Go-http-client".
func playUserAgent() string { return "rollout/" + versionString() }
