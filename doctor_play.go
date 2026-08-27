package main

import (
	"context"
	"errors"
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
		{"storage base url:  ", cfg.StorageBaseURL},
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

// runPlayDoctorLive probes every API surface this configuration can reach.
//
// rollout talks to as many as three services, each with its own permission and
// its own way of being half-configured: the Publisher API (everything that
// ships), the Reporting API (vitals, errors, anomalies, app listing), and the
// GCS bucket holding the CSV report exports. Proving one says nothing about the
// others — a Release Manager credential publishes perfectly and is refused by
// Reporting — so each is probed and reported on its own line.
func runPlayDoctorLive(ctx context.Context, out io.Writer, cfg *PlayConfig) (liveResult, error) {
	client, err := NewClient(ctx, cfg)
	if err != nil {
		return reportProbe(out, "credentials:        ", err), err
	}

	publish := probePublish(ctx, out, client, cfg)
	reporting := probeReporting(ctx, out, client, cfg)
	if publish.skipped {
		// No app to publish-probe, so Reporting is the only evidence there is
		// and it stops being advisory. A permission denial still is not a
		// broken setup: the token was accepted and only authorization failed,
		// which proves the credential is real without proving what it may do.
		reporting.advisory = false
		if reporting.result == liveOK || isPermissionDenied(reporting.err) {
			// Either way the credential is real — a listing proves it outright,
			// a permission denial proves the token was accepted and only the
			// grant is missing. Neither proves this credential may publish to
			// any particular app, and only the publish probe can.
			reporting = probeOutcome{result: liveUnverified}
		}
	}
	reports := probeReportsBucket(ctx, out, client, cfg)

	worst := probeOutcome{result: liveOK}
	for _, o := range []probeOutcome{publish, reporting, reports} {
		if !o.advisory && o.result.worseThan(worst.result) {
			worst = o
		}
	}
	return worst.result, worst.err
}

// probeOutcome is one surface's result. advisory marks a capability gap that
// must not decide the overall verdict: a credential that publishes but cannot
// read the Reporting API is a working setup for every write rollout performs,
// and reporting it as NOT READY would send users to fix what isn't broken.
type probeOutcome struct {
	result   liveResult
	err      error
	advisory bool
	// skipped marks a probe that never ran, so the caller can tell "this
	// surface is fine" from "this surface was never looked at".
	skipped bool
}

// probePublish proves the Publisher API works by opening and immediately
// deleting an edit on the configured app.
//
// There is no cheaper probe. `edits` cannot be listed, and every read endpoint
// needs an edit anyway, so an insert + delete is both the smallest call that
// touches the publishing API and the one that proves what actually matters:
// that the credential is linked to a Play Console account *and* has access to
// this specific app. A token that mints fine can still belong to a service
// account nobody invited.
func probePublish(ctx context.Context, out io.Writer, client *Client, cfg *PlayConfig) probeOutcome {
	s := newStyles(out)
	const label = "publish probe:      "
	pkg, err := cfg.resolvePackage("")
	if err != nil {
		fmt.Fprintf(out, "%s %s\n", s.label(label), s.warning(fmt.Sprintf("skipped (%v)", err)))
		return probeOutcome{result: liveOffline, advisory: true, skipped: true}
	}

	// Read mode: withEdit deletes the edit on the way out, so a doctor run
	// leaves nothing behind in the app's edit list.
	editID, err := client.withEdit(ctx, pkg, false, commitOptions{}, func(*editSession) error { return nil })
	if err != nil {
		return probeOutcome{result: reportProbe(out, label, err), err: err}
	}
	fmt.Fprintf(out, "%s %s opened and deleted edit %s on %s\n", s.label(label), s.markOK(), editID, s.accent(pkg))
	return probeOutcome{result: liveOK}
}

// probeReporting proves the Reporting API works, via the apps:search call that
// `play apps` is built on — the only listing endpoint either API offers.
//
// Its failure is routine rather than exceptional. Reporting is a separate
// service with a separate grant (*View app information*) and a separate Cloud
// API to enable, so the common outcome for a release-only credential is a 403
// that says nothing bad about the publishing setup. That is reported as a
// missing capability, not as a broken tool.
func probeReporting(ctx context.Context, out io.Writer, client *Client, cfg *PlayConfig) probeOutcome {
	s := newStyles(out)
	const label = "reporting probe:    "
	const indent = "                    "
	apps, err := client.searchApps(ctx)
	if err != nil {
		hinted := reportingPermissionHint(err)
		var apiErr *apiError
		if isPermissionDenied(err) && errors.As(err, &apiErr) {
			fmt.Fprintf(out, "%s %s %s\n", s.label(label), s.markUnknown(), s.warning("vitals, errors and app listing unavailable — publishing is unaffected"))
			// The bare API message plus the Reporting grant, and not the
			// generic 403 hint: this refusal has nothing to do with release
			// permissions, and advising them would send the user to the wrong
			// checkbox.
			fmt.Fprintf(out, "%s%s\n", indent, s.muted(apiErr.bare()))
			fmt.Fprintf(out, "%s%s\n", indent, s.muted(reportingGrantHint))
			return probeOutcome{result: liveFailed, err: hinted, advisory: true}
		}
		return probeOutcome{result: reportProbe(out, label, hinted), err: hinted, advisory: true}
	}

	fmt.Fprintf(out, "%s %s %s visible\n", s.label(label), s.markOK(), s.accent(plural(len(apps), "app")))
	// With no default app set, the listing is the answer to the question the
	// user is about to ask, so spend the lines on it rather than making them
	// run `play apps` next.
	if cfg.PackageName == "" && len(apps) > 0 {
		for _, app := range apps {
			fmt.Fprintf(out, "%s%s\n", indent, s.muted(app.PackageName))
		}
		fmt.Fprintf(out, "%s%s\n", indent, s.muted("set a default with `rollout config play set-package <package>`"))
	}
	return probeOutcome{result: liveOK}
}

// probeReportsBucket proves the CSV report bucket can be read, and runs only
// when one is configured — asking about a bucket nobody set is noise.
//
// Unlike Reporting, a failure here is not advisory: setting `reports_bucket` is
// an explicit statement that this capability is wanted, so a bucket that cannot
// be read is a misconfiguration rather than a capability the user declined.
func probeReportsBucket(ctx context.Context, out io.Writer, client *Client, cfg *PlayConfig) probeOutcome {
	if cfg.ReportsBucket == "" {
		return probeOutcome{result: liveOK, skipped: true}
	}
	s := newStyles(out)
	const label = "reports probe:      "
	if err := client.listReportObjects(ctx, cfg.ReportsBucket); err != nil {
		hinted := reportsBucketHint(err, cfg)
		return probeOutcome{result: reportProbe(out, label, hinted), err: hinted}
	}
	fmt.Fprintf(out, "%s %s %s readable\n", s.label(label), s.markOK(), s.accent("gs://"+cfg.ReportsBucket))
	return probeOutcome{result: liveOK}
}

// reportsBucketHint names the fix for a refused bucket read. For a signed-in
// user the likeliest cause is invisible from the config: the storage scope is
// requested only when a reports bucket is set, so a sign-in that predates the
// setting holds a token without it, and no amount of re-configuring helps.
func reportsBucketHint(err error, cfg *PlayConfig) error {
	if !isPermissionDenied(err) && !isUnauthenticated(err) {
		return err
	}
	if cfg.credentialMode() == credentialOAuthUser {
		return fmt.Errorf("%w — if you signed in before setting reports_bucket, the saved token predates the Cloud Storage scope; run `%s` again to re-consent", err, playTokenPolicy.loginCommand())
	}
	return fmt.Errorf("%w — grant this credential read access to the bucket in Play Console → Users & permissions (report access), and make sure the bucket name is the `pubsite_prod_rev_…` one shown there", err)
}

// isPermissionDenied reports whether the API accepted the credential and
// refused the operation — the token is real, the grant is missing.
func isPermissionDenied(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden
}

// isUnauthenticated reports whether the credential itself was rejected.
func isUnauthenticated(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

// playUserAgent identifies this binary to the API, so Google's quota and abuse
// tooling can attribute traffic to rollout rather than to "Go-http-client".
func playUserAgent() string { return "rollout/" + versionString() }
