package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
)

// doctorOffline backs the `--offline` flag: by default doctor makes real API
// calls to verify the setup works; --offline skips them and only checks that
// credentials resolve (fast, deterministic, no network — for CI/offline).
var doctorOffline bool

// doctorCmd reports whether the setup works. By default it probes the API so
// "ready" means real calls succeed, not just that the credential strings are
// present. --offline reduces it to the cheap config-only check.
//
// The checks themselves belong to each platform (Platform.Doctor); this command
// only sequences them, prints the status line, and picks the exit code.
var doctorCmd = &cobra.Command{
	Use:   "doctor [platform]",
	Short: "Check that an app store's setup works (config + live API check)",
	Long:  "Verify that credentials resolve and that real API calls succeed.\n\nWith no argument every configured platform is checked in turn; pass a platform\nname (e.g. `rollout doctor play`) to check just one.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		targets := platforms()
		var skipped []string
		if len(args) == 1 {
			p, err := lookupPlatform(args[0])
			if err != nil {
				return err
			}
			// Named explicitly: check it whether or not it is set up. "I haven't
			// configured this yet" is exactly what the user is asking about.
			targets = []*Platform{p}
		} else {
			targets, skipped = configuredPlatforms(targets)
		}
		if len(targets) == 0 {
			return errors.New("no app stores are compiled into this binary")
		}

		out := cmd.OutOrStdout()
		s := newStyles(out)
		defer func() {
			for _, name := range skipped {
				fmt.Fprintf(out, "\n%s\n", s.muted(fmt.Sprintf("skipped %s — not configured. Run `rollout doctor %s` to see what it needs.", name, name)))
			}
		}()
		var worst *platformVerdict
		for i, p := range targets {
			if i > 0 {
				fmt.Fprintln(out)
			}
			if len(targets) > 1 {
				fmt.Fprintf(out, "%s\n", s.header(fmt.Sprintf("=== %s (%s) ===", p.Title, p.Name)))
			}
			if p.Doctor == nil {
				fmt.Fprintf(out, "%s has no health checks yet.\n", p.Title)
				continue
			}
			res, err := p.Doctor(cmd.Context(), out, doctorOffline)
			fmt.Fprint(out, statusLine(s, res, err))
			if worst == nil || res.worseThan(worst.result) {
				worst = &platformVerdict{result: res, err: err}
			}
		}
		if worst == nil {
			return nil
		}
		return worst.exit()
	},
}

// configuredPlatforms splits a platform list into the ones the user has set up
// and the names of the ones they haven't, so a plain `rollout doctor` reports
// on the stores in use instead of failing over a platform nobody signed in to.
//
// When nothing is configured every platform is returned: a brand-new user runs
// `rollout doctor` precisely to be told what to set up, and an empty report
// would answer nothing.
func configuredPlatforms(all []*Platform) (targets []*Platform, skipped []string) {
	for _, p := range all {
		if p.configured() {
			targets = append(targets, p)
		} else {
			skipped = append(skipped, p.Name)
		}
	}
	if len(targets) == 0 {
		return all, nil
	}
	return targets, skipped
}

// platformVerdict pairs a platform's doctor outcome with the error that
// explains it, so the command can exit on the worst result across platforms.
type platformVerdict struct {
	result liveResult
	err    error
}

// exit turns a verdict into the command's return value. Each result maps to a
// distinct exit code, and only liveUnconfigured surfaces its error to the user
// (the live probes already printed their own diagnostics).
func (v *platformVerdict) exit() error {
	switch v.result {
	// liveUnverified exits clean: a credential Google accepts, with no app to
	// probe, is a setup nobody can call broken. The status line says what is
	// missing; failing CI over it would only teach users to stop running doctor.
	case liveOK, liveOffline, liveUnverified:
		return nil
	case liveUnconfigured:
		return v.err
	case liveInconclusive:
		return &exitErr{code: 2, err: v.err}
	default: // liveFailed
		return &exitErr{code: 1, err: v.err}
	}
}

// liveResult is the outcome of a platform's doctor check.
type liveResult int

const (
	liveOK           liveResult = iota // the API answered and real calls work
	liveOffline                        // --offline: credentials resolve, API not probed
	liveUnverified                     // the API accepted the credential but nothing could be proved about what it may do
	liveInconclusive                   // couldn't reach the API (transport/5xx) — setup unconfirmed, not broken
	liveFailed                         // the API definitively rejected us (4xx) — setup is broken
	liveUnconfigured                   // credentials didn't even resolve
)

// worseThan orders results by severity so a multi-platform run exits on its
// worst outcome. The iota order is already severity order.
func (r liveResult) worseThan(other liveResult) bool { return r > other }

// statusLine renders the verdict a platform's doctor reached. The verdict
// itself is colored — it is the one line a user scans for — while the
// explanation after it stays plain so it remains readable at a glance.
func statusLine(s styles, res liveResult, err error) string {
	label := s.label("status:")
	switch res {
	case liveOK:
		return fmt.Sprintf("\n%s %s %s\n", label, s.success("READY"), s.muted("(live check passed)"))
	case liveOffline:
		return fmt.Sprintf("\n%s %s — credentials resolve (offline check). Run `rollout doctor` to verify against the API.\n", label, s.success("READY"))
	case liveUnverified:
		// Deliberately generic: what is missing, and how to supply it, is the
		// platform's business and its probe lines above have already said it.
		return fmt.Sprintf("\n%s %s — the API accepted the credential, but nothing above could confirm what it may do (see the probes).\n", label, s.success("READY"))
	case liveUnconfigured:
		return fmt.Sprintf("\n%s %s — %v\n", label, s.failure("NOT READY"), err)
	case liveInconclusive:
		return fmt.Sprintf("\n%s %s — credentials resolve, but the API couldn't be reached (network/transient). Setup unconfirmed, not necessarily broken.\n", label, s.warning("INCONCLUSIVE"))
	default: // liveFailed
		return fmt.Sprintf("\n%s %s — the API rejected the request (see above)\n", label, s.failure("NOT READY"))
	}
}

// definitiveAPIError is implemented by a platform's API error type when it can
// tell a definitive rejection (4xx — the request or credentials are wrong) from
// a transient one. It keeps liveVerdictFor free of any platform's error types.
type definitiveAPIError interface {
	isClientError() bool
}

// liveVerdictFor classifies a live-probe error. A 4xx from the platform's API is
// definitive — the request or credentials are wrong (liveFailed). So is a 4xx
// from the OAuth token endpoint (oauth2.RetrieveError): invalid_grant means
// the refresh token is revoked or the service-account key is disabled. Anything
// else — a 5xx, a connection failure — means we simply couldn't get a verdict
// (liveInconclusive), which must not be reported as a broken setup.
func liveVerdictFor(err error) liveResult {
	if err == nil {
		return liveOK
	}
	var apiErr definitiveAPIError
	if errors.As(err, &apiErr) && apiErr.isClientError() {
		return liveFailed
	}
	var oauthErr *oauth2.RetrieveError
	if errors.As(err, &oauthErr) && oauthErr.Response != nil {
		switch oauthErr.Response.StatusCode {
		// Definitive credential failures. 429/5xx from the token endpoint
		// stay inconclusive — rate limiting is not a broken setup.
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			return liveFailed
		}
	}
	return liveInconclusive
}

// reportProbe prints a failed probe line — ✗ for a definitive failure, ? for an
// inconclusive one — and returns the classification. label should be padded to
// align with the ✓ lines (a trailing space follows the marker).
func reportProbe(out io.Writer, label string, err error) liveResult {
	s := newStyles(out)
	verdict := liveVerdictFor(err)
	marker := s.markUnknown()
	prefix := "could not reach the API: "
	if verdict == liveFailed {
		marker = s.markFail()
		prefix = ""
	}
	fmt.Fprintf(out, "%s %s %s%v\n", s.label(label), marker, prefix, err)
	return verdict
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorOffline, "offline", false, "skip the live API check; only verify that credentials resolve")
}
