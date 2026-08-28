package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// playShowConfig is the Play platform's Platform.ShowConfig hook: the resolved
// settings for `rollout config show`, with credentials redacted.
//
// The service-account key is described by its `client_email` and file path,
// never by its contents: that email is the one thing the user actually needs
// (it is what they invite in Play Console → Users & permissions), and the
// private key must never reach a terminal scrollback or a CI log.
func playShowConfig(out io.Writer) error {
	cfg, err := loadPlayConfig(configPath)
	if err != nil {
		return err
	}
	s := newStyles(out)
	store := describeTokenStore(playTokenPolicy.Platform)
	settings := []struct{ label, value string }{
		{"credential mode:     ", playCredentialModeSummary(cfg)},
		{"service account:     ", playServiceAccountSummary(cfg)},
		{"client id:           ", orNone(cfg.ClientID)},
		{"client secret:       ", redactSecret(cfg.ClientSecret)},
		{"token store:         ", store.location()},
		{"saved sign-in:       ", store.describe(playTokenPolicy)},
		{"package name:        ", orNone(cfg.PackageName)},
		{"developer id:        ", orNone(cfg.DeveloperID)},
		{"reports bucket:      ", orNone(cfg.ReportsBucket)},
		{"api base url:        ", cfg.BaseURL},
		{"reporting base url:  ", cfg.ReportingBaseURL},
		{"storage base url:    ", cfg.StorageBaseURL},
		{"scopes:              ", strings.Join(cfg.scopes(), ", ")},
	}
	for _, set := range settings {
		fmt.Fprintf(out, "%s %s\n", s.label(set.label), s.value(set.value))
	}
	return nil
}

// playCredentialModeSummary names the mode and, when both are configured, says
// which one wins — otherwise a user who set up both is left guessing why their
// browser sign-in is being ignored.
func playCredentialModeSummary(cfg *PlayConfig) string {
	mode := cfg.credentialMode()
	if mode == credentialServiceAccount && cfg.ClientID != "" {
		return string(mode) + " (a service-account key is configured, so the OAuth client is unused)"
	}
	if mode == credentialNone {
		return "none — set PLAY_SERVICE_ACCOUNT_FILE or run `rollout login play`"
	}
	return string(mode)
}

// playServiceAccountSummary describes the configured key by the identity it
// carries, which is what the user has to grant access to in the Console.
func playServiceAccountSummary(cfg *PlayConfig) string {
	if cfg.ServiceAccountFile == "" && cfg.serviceAccountJSON == "" {
		return "(not set)"
	}
	origin := cfg.ServiceAccountFile
	if origin == "" {
		origin = "PLAY_SERVICE_ACCOUNT_JSON"
	}
	key, err := cfg.readServiceAccountKey()
	if err != nil {
		// `config show` reports the setup as it is; an unreadable key is a
		// finding, not a reason to fail the command.
		return fmt.Sprintf("%s — unusable: %v", origin, err)
	}
	return fmt.Sprintf("%s (%s)", key.ClientEmail, origin)
}

// playSetPackageCmd persists package_name so every Play command can omit
// --package. PLAY_PACKAGE_NAME still overrides the file value.
var playSetPackageCmd = &cobra.Command{
	Use:   "set-package <package-name>",
	Short: "Persist a default Android package name so --package can be omitted",
	Long:  "Write [play].package_name to the rollout config file (the --config path if given,\notherwise the default location — see `rollout config path`).\n\nOther keys in the file are preserved, but comments are not: the file is\nre-encoded from its parsed form. PLAY_PACKAGE_NAME overrides the file value.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pkg := strings.TrimSpace(args[0])
		if !validPackageName(pkg) {
			return fmt.Errorf("invalid package name %q — expected a reverse-DNS Android application ID, e.g. com.example.app", args[0])
		}
		path, err := writableConfigPath(configPath)
		if err != nil {
			return err
		}
		if err := upsertConfigKey(path, playConfigTable, "package_name", pkg); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		s := newStyles(out)
		fmt.Fprintf(out, "%s default package set to %s in %s\n", s.markOK(), s.accent(pkg), s.muted(path))
		return nil
	},
}

// playSetDeveloperIDCmd persists developer_id, which only the users, grants,
// and report-bucket tools need — it is not part of any publishing call.
var playSetDeveloperIDCmd = &cobra.Command{
	Use:   "set-developer-id <developer-id>",
	Short: "Persist the Play Console developer account ID",
	Long:  "Write [play].developer_id to the rollout config file. It is the numeric ID in the\nPlay Console URL (…/developers/<developer-id>/…), and is needed only by the\nusers and permissions tools. PLAY_DEVELOPER_ID overrides the file value.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := strings.TrimSpace(args[0])
		if !validDeveloperID(id) {
			return fmt.Errorf("invalid developer ID %q — expected the digits from the Play Console URL, e.g. 1234567890", args[0])
		}
		path, err := writableConfigPath(configPath)
		if err != nil {
			return err
		}
		if err := upsertConfigKey(path, playConfigTable, "developer_id", id); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		s := newStyles(out)
		fmt.Fprintf(out, "%s developer ID set to %s in %s\n", s.markOK(), s.accent(id), s.muted(path))
		return nil
	},
}

// playSetReportsBucketCmd persists reports_bucket, the Cloud Storage bucket the
// CSV report exports land in. Setting it does two things beyond naming a
// bucket: it enables the report tools, and it adds the read-only Cloud Storage
// scope to the credential — so a user who signs in first and sets it afterwards
// has to sign in again to pick the scope up.
var playSetReportsBucketCmd = &cobra.Command{
	Use:   "set-reports-bucket <bucket>",
	Short: "Persist the Cloud Storage bucket holding the CSV report exports",
	Long:  "Write [play].reports_bucket to the rollout config file. The name is shown in Play\nConsole \u2192 Download reports as the \"Cloud Storage URI\", and looks like\n`pubsite_prod_rev_1234567890`; a full `gs://\u2026` URI is accepted and trimmed.\n\nSetting it also adds the read-only Cloud Storage scope to the credential, so if\nyou signed in with `rollout login play` first, sign in again afterwards.\nPLAY_REPORTS_BUCKET overrides the file value.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bucket, err := normalizeBucketName(args[0])
		if err != nil {
			return err
		}
		path, err := writableConfigPath(configPath)
		if err != nil {
			return err
		}
		if err := upsertConfigKey(path, playConfigTable, "reports_bucket", bucket); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		s := newStyles(out)
		fmt.Fprintf(out, "%s reports bucket set to %s in %s\n", s.markOK(), s.accent(bucket), s.muted(path))
		if !strings.HasPrefix(bucket, "pubsite_prod") {
			fmt.Fprintf(out, "%s\n", s.warning("that does not look like a Play export bucket — the name is on Play Console \u2192 Download reports and starts with pubsite_prod"))
		}
		fmt.Fprintf(out, "%s\n", s.muted("check it with `rollout doctor play`"))
		return nil
	},
}

// normalizeBucketName accepts what a user is likely to paste: the bare bucket
// name, or the `gs://bucket/` URI the Console shows.
func normalizeBucketName(s string) (string, error) {
	bucket := trimBucketURI(s)
	if bucket == "" {
		return "", fmt.Errorf("empty bucket name \u2014 pass the `pubsite_prod_rev_\u2026` name from Play Console \u2192 Download reports")
	}
	if strings.ContainsAny(bucket, " \t") {
		return "", fmt.Errorf("invalid bucket name %q \u2014 a Cloud Storage bucket name has no spaces", s)
	}
	return bucket, nil
}

// validPackageName reports whether s looks like an Android application ID:
// at least two dot-separated segments, each starting with a letter and holding
// only letters, digits, and underscores.
//
// It is a shape check, not a policy check — the point is to catch a display
// name or an app title typed in by mistake, which would otherwise be persisted
// and then fail on every later command with a 404 from the API.
func validPackageName(s string) bool {
	segments := strings.Split(s, ".")
	if len(segments) < 2 {
		return false
	}
	for _, seg := range segments {
		if seg == "" {
			return false
		}
		for i, r := range seg {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			case (r >= '0' && r <= '9') && i > 0:
			default:
				return false
			}
		}
	}
	return true
}

// validDeveloperID reports whether s is the all-digit ID from the Console URL.
func validDeveloperID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
