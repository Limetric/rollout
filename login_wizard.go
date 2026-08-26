package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

// The guided sign-in. `rollout login play` with no flags walks a first-time
// user through the three things Google Play actually needs — a credential, an
// app, and access granted in the Console — and ends with the same live probe
// `rollout doctor play` runs.

// prompter is the wizard's input surface. The real implementation reads a TTY;
// tests inject a fake with scripted answers.
type prompter interface {
	line(prompt string) (string, error)
	secret(prompt string) (string, error)
	confirm(prompt string, def bool) (bool, error)
}

// ttyPrompter reads line-oriented input from a terminal. It reads one byte at a
// time (no buffering ahead) so a masked term.ReadPassword on the same fd never
// loses input that a buffered reader would have swallowed.
type ttyPrompter struct {
	in  io.Reader
	out io.Writer
	fd  int // terminal fd for masked reads; <0 means "not a terminal"
}

func newTTYPrompter(in io.Reader, out io.Writer, fd int) *ttyPrompter {
	return &ttyPrompter{in: in, out: out, fd: fd}
}

// readLine reads up to (not including) the next '\n'. It returns io.EOF only
// when the stream ends with no data read, so the caller can treat that as an
// abort.
func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return sb.String(), nil
			}
			if buf[0] != '\r' {
				sb.WriteByte(buf[0])
			}
		}
		if err != nil {
			if err == io.EOF {
				if sb.Len() == 0 {
					return "", io.EOF
				}
				return sb.String(), nil
			}
			return "", err
		}
	}
}

func (p *ttyPrompter) line(prompt string) (string, error) {
	fmt.Fprint(p.out, prompt)
	s, err := readLine(p.in)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func (p *ttyPrompter) secret(prompt string) (string, error) {
	fmt.Fprint(p.out, prompt)
	if p.fd >= 0 && term.IsTerminal(p.fd) {
		b, err := term.ReadPassword(p.fd)
		if err != nil {
			return "", err
		}
		fmt.Fprintln(p.out) // ReadPassword swallows the newline; restore it on success
		return strings.TrimSpace(string(b)), nil
	}
	s, err := readLine(p.in)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func (p *ttyPrompter) confirm(prompt string, def bool) (bool, error) {
	suffix := " [y/N]: "
	if def {
		suffix = " [Y/n]: "
	}
	fmt.Fprint(p.out, prompt+suffix)
	s, err := readLine(p.in)
	if err != nil {
		return false, err
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return def, nil
	}
	return s == "y" || s == "yes", nil
}

// secretHint returns a non-revealing suffix hint for an existing secret, so the
// wizard can ask "keep this one?" about a credential it must not print.
func secretHint(s string) string {
	if len(s) <= 6 {
		return "…"
	}
	return "…" + s[len(s)-6:]
}

// expandHome expands a leading ~/ and strips surrounding quotes and space, so a
// path dragged into the terminal pastes cleanly.
func expandHome(path string) string {
	path = strings.Trim(strings.TrimSpace(path), "'\"")
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

const (
	urlPublisherAPI = "https://console.cloud.google.com/apis/library/androidpublisher.googleapis.com"
	urlReportingAPI = "https://console.cloud.google.com/apis/library/playdeveloperreporting.googleapis.com"
	urlCredentials  = "https://console.cloud.google.com/apis/credentials"
	urlPlayUsers    = "https://play.google.com/console/developers/users-and-permissions"
)

// offerToOpen prints an instruction and URL and (unless --no-browser) offers to
// open it, then waits for the user to press Enter.
func offerToOpen(p prompter, out io.Writer, instruction, url string, openFn func(string) error) error {
	fmt.Fprintf(out, "   %s\n   → %s\n", instruction, url)
	if loginNoBrowser {
		_, err := p.line("   Press Enter when done.")
		return err
	}
	open, err := p.confirm("   Open this now?", true)
	if err != nil {
		return err
	}
	if open {
		if e := openFn(url); e != nil {
			fmt.Fprintf(out, "   (couldn't open a browser: %v — open the URL above manually)\n", e)
		}
	}
	_, err = p.line("   Press Enter when done.")
	return err
}

// confirmBrowserOpen wraps the sign-in's openFn so the consent URL is always
// shown first and, unless --no-browser, the user is asked before a browser is
// launched. Declining leaves the URL on screen; the loopback server waits for
// the callback either way.
func confirmBrowserOpen(p prompter, out io.Writer, port int, openFn func(string) error) func(string) error {
	return func(u string) error {
		fmt.Fprintf(out, "   Sign in to Google to authorize rollout.\n   → %s\n", u)
		if !loginNoBrowser {
			open, err := p.confirm("   Open this now?", true)
			if err != nil {
				return err
			}
			if open {
				if e := openFn(u); e != nil {
					fmt.Fprintf(out, "   (couldn't open a browser: %v — open the URL above manually)\n", e)
				}
			}
		}
		fmt.Fprintf(out, "   Waiting for callback on %s …\n", loopbackRedirectURL(port))
		return nil
	}
}

// runLoginWizard is the interactive `rollout login play`.
func runLoginWizard(ctx context.Context, out io.Writer, p prompter, cfg *PlayConfig, target string, openFn func(string) error, port int) error {
	fmt.Fprintln(out, "=== Google Play sign-in ===")
	fmt.Fprintln(out, "rollout can authenticate two ways. A service-account key is what Google")
	fmt.Fprintln(out, "recommends and the only thing that works headlessly in CI; signing in as")
	fmt.Fprintln(out, "yourself is quicker on a laptop.")
	fmt.Fprintln(out)

	useServiceAccount, err := p.confirm("Use a service-account JSON key?", true)
	if err != nil {
		return err
	}
	if useServiceAccount {
		if err := wizardServiceAccount(ctx, out, p, cfg, target, openFn); err != nil {
			return err
		}
	} else if err := wizardOAuth(ctx, out, p, cfg, target, openFn, port); err != nil {
		return err
	}

	if err := wizardPackageName(out, p, cfg, target); err != nil {
		return err
	}
	fmt.Fprintln(out)
	return verifyLogin(ctx, out, cfg)
}

// wizardServiceAccount collects and validates a key file, re-prompting on a bad
// path rather than making the user restart the wizard.
func wizardServiceAccount(_ context.Context, out io.Writer, p prompter, cfg *PlayConfig, target string, openFn func(string) error) error {
	if cfg.ServiceAccountFile != "" {
		keep, err := p.confirm(fmt.Sprintf("Found a service-account key (%s). Keep it?", cfg.ServiceAccountFile), true)
		if err != nil {
			return err
		}
		if keep {
			if _, err := cfg.readServiceAccountKey(); err == nil {
				return nil
			}
			fmt.Fprintln(out, "   That key is no longer readable; let's pick another.")
		}
	}

	fmt.Fprintln(out, "\n1. Enable the two APIs rollout uses in your Cloud project.")
	if err := offerToOpen(p, out, "Google Play Android Developer API", urlPublisherAPI, openFn); err != nil {
		return err
	}
	if err := offerToOpen(p, out, "Google Play Developer Reporting API (vitals)", urlReportingAPI, openFn); err != nil {
		return err
	}
	fmt.Fprintln(out, "\n2. Create a service account and download a JSON key.")
	if err := offerToOpen(p, out, "Cloud Console → Credentials", urlCredentials, openFn); err != nil {
		return err
	}

	for {
		path, err := p.line("\n   Path to the service-account JSON key: ")
		if err != nil {
			return err
		}
		cfg.ServiceAccountFile = expandHome(path)
		cfg.serviceAccountJSON = ""
		key, err := cfg.readServiceAccountKey()
		if err != nil {
			fmt.Fprintf(out, "   %v\n", err)
			continue
		}
		if err := saveServiceAccountPath(target, cfg.ServiceAccountFile); err != nil {
			return err
		}
		// The OAuth client, if any, is now unused: the service-account key wins.
		cfg.ClientID, cfg.ClientSecret = "", ""
		fmt.Fprintf(out, "   ✓ %s\n", key.ClientEmail)

		fmt.Fprintln(out, "\n3. Grant that service account access to your app.")
		fmt.Fprintf(out, "   Invite %s and give it \"Release to production, exclude devices, and use\n   Play App Signing\" plus \"View app information\" for vitals.\n", key.ClientEmail)
		return offerToOpen(p, out, "Play Console → Users & permissions", urlPlayUsers, openFn)
	}
}

// wizardOAuth walks the user-sign-in path: an OAuth client, then the browser.
func wizardOAuth(ctx context.Context, out io.Writer, p prompter, cfg *PlayConfig, target string, openFn func(string) error, port int) error {
	creds, err := wizardGatherClient(p, out, cfg, openFn)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\n   Authorizing…")
	refreshToken, err := signInLoopback(ctx, cfg, creds, confirmBrowserOpen(p, out, port, openFn), port)
	if err != nil {
		return err
	}
	if err := savePlayCredentials(target, creds, refreshToken); err != nil {
		return err
	}
	cfg.ClientID, cfg.ClientSecret = creds.clientID, creds.clientSecret
	// A key configured earlier would win over the sign-in that just happened.
	cfg.ServiceAccountFile, cfg.serviceAccountJSON = "", ""
	fmt.Fprintf(out, "   ✓ Signed in. Credentials written to %s\n", target)
	return nil
}

// wizardGatherClient resolves the OAuth client: reuse the configured one, read
// a downloaded Desktop-app JSON, or take an id/secret pair by hand.
func wizardGatherClient(p prompter, out io.Writer, cfg *PlayConfig, openFn func(string) error) (clientCreds, error) {
	if cfg.ClientID != "" && cfg.ClientSecret != "" {
		keep, err := p.confirm(fmt.Sprintf("   Found an OAuth client (%s). Keep it?", secretHint(cfg.ClientID)), true)
		if err != nil {
			return clientCreds{}, err
		}
		if keep {
			return clientCreds{clientID: cfg.ClientID, clientSecret: cfg.ClientSecret, kind: "config"}, nil
		}
	}

	fmt.Fprintln(out, "\n1. Enable the two APIs rollout uses in your Cloud project.")
	if err := offerToOpen(p, out, "Google Play Android Developer API", urlPublisherAPI, openFn); err != nil {
		return clientCreds{}, err
	}
	if err := offerToOpen(p, out, "Google Play Developer Reporting API (vitals)", urlReportingAPI, openFn); err != nil {
		return clientCreds{}, err
	}
	fmt.Fprintln(out, "\n2. Create an OAuth 2.0 Client ID of type \"Desktop app\" and download its JSON.")
	if err := offerToOpen(p, out, "Cloud Console → Credentials", urlCredentials, openFn); err != nil {
		return clientCreds{}, err
	}

	for {
		path, err := p.line("\n   Path to the downloaded client JSON (or press Enter to type the id/secret): ")
		if err != nil {
			return clientCreds{}, err
		}
		if path == "" {
			id, err := p.line("   Client ID: ")
			if err != nil {
				return clientCreds{}, err
			}
			secret, err := p.secret("   Client secret: ")
			if err != nil {
				return clientCreds{}, err
			}
			if id == "" || secret == "" {
				fmt.Fprintln(out, "   Both halves are needed.")
				continue
			}
			return clientCreds{clientID: id, clientSecret: secret, kind: "config"}, nil
		}
		data, err := os.ReadFile(expandHome(path))
		if err != nil {
			fmt.Fprintf(out, "   %v\n", err)
			continue
		}
		creds, err := parseCredentialsJSON(data)
		if err != nil {
			fmt.Fprintf(out, "   %v\n", err)
			continue
		}
		if creds.clientID == "" || creds.clientSecret == "" {
			fmt.Fprintln(out, "   That file has no client_id/client_secret.")
			continue
		}
		if creds.kind == "web" {
			fmt.Fprintln(out, "   Warning: this is a Web-application client; loopback sign-in expects a Desktop app. Trying anyway.")
		}
		return creds, nil
	}
}

// wizardPackageName offers to persist a default app, so every later command can
// omit --package.
func wizardPackageName(out io.Writer, p prompter, cfg *PlayConfig, target string) error {
	if cfg.PackageName != "" {
		fmt.Fprintf(out, "\nDefault app: %s\n", cfg.PackageName)
		return nil
	}
	pkg, err := p.line("\nDefault app package name (e.g. com.example.app, or Enter to skip): ")
	if err != nil {
		return err
	}
	if pkg == "" {
		return nil
	}
	if !validPackageName(pkg) {
		fmt.Fprintf(out, "   %q is not a package name — skipping. Set it later with `rollout config play set-package`.\n", pkg)
		return nil
	}
	if err := upsertConfigKey(target, playConfigTable, "package_name", pkg); err != nil {
		return err
	}
	cfg.PackageName = pkg
	fmt.Fprintf(out, "   ✓ Default app set to %s\n", pkg)
	return nil
}
