package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Terminal colors for the human-facing CLI.
//
// Color is presentation, never data. It is applied at the print site — never
// baked into a value a tool returns — so the MCP front-end, `--format json`,
// and the audit log all stay byte-identical to what they were before. Anything
// a pipeline parses (JSON, CSV, the audit log) is left uncolored on purpose.
//
// Whether to color is decided from the writer being printed to, the way
// pgferry does it: a *os.File on a character device gets color, a
// bytes.Buffer (tests, pipes, `| jq`) does not. That means no test has to opt
// out, and redirecting stdout to a file never fills it with escape codes.

// colorMode is the --color flag: auto (a terminal gets color), always, never.
var colorMode = "auto"

// validateColorMode checks the --color flag value. It runs before any command
// so a typo is reported instead of silently meaning "auto".
func validateColorMode(mode string) error {
	switch mode {
	case "auto", "always", "never":
		return nil
	default:
		return fmt.Errorf("unknown --color value %q — use auto, always, or never", mode)
	}
}

// styles renders text with ANSI escapes, or returns it untouched when the
// destination cannot show them. The zero value is the plain renderer, so
// styles{} is always a safe stand-in.
type styles struct{ enabled bool }

// newStyles decides whether out can show color: --color wins, then NO_COLOR
// and TERM=dumb, then whether out is a terminal.
func newStyles(out io.Writer) styles {
	switch colorMode {
	case "always":
		return styles{enabled: true}
	case "never":
		return styles{}
	}
	// https://no-color.org — set to any non-empty value means "no color".
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return styles{}
	}
	f, ok := out.(*os.File)
	if !ok {
		return styles{}
	}
	info, err := f.Stat()
	if err != nil || (info.Mode()&os.ModeCharDevice) == 0 {
		return styles{}
	}
	return styles{enabled: true}
}

// wrap applies an escape code and resets afterwards. Empty text is returned
// as-is so an unset value never becomes a bare pair of escape codes.
func (s styles) wrap(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return code + text + "\033[0m"
}

// The palette. Deliberately small: four semantic colors (success, warning,
// failure, accent) plus three weights (header, prompt, muted). A CLI that uses
// more than that stops reading as a hierarchy and starts reading as noise.
func (s styles) header(text string) string  { return s.wrap("\033[1;36m", text) } // bold cyan
func (s styles) accent(text string) string  { return s.wrap("\033[1;34m", text) } // bold blue
func (s styles) muted(text string) string   { return s.wrap("\033[2m", text) }    // dim
func (s styles) prompt(text string) string  { return s.wrap("\033[1m", text) }    // bold
func (s styles) success(text string) string { return s.wrap("\033[32m", text) }   // green
func (s styles) warning(text string) string { return s.wrap("\033[33m", text) }   // yellow
func (s styles) failure(text string) string { return s.wrap("\033[31m", text) }   // red

// url renders a link so it stands out from the prose around it. Terminals that
// autolink still do; the underline is for the ones that don't.
func (s styles) url(text string) string { return s.wrap("\033[4;34m", text) }

// label renders the key half of an aligned "key: value" report line, so the
// eye lands on the values — which are the part that differs between runs.
func (s styles) label(text string) string { return s.muted(text) }

// value renders a resolved setting, dimming the placeholders that stand for
// "nothing is set here" so a report reads as values with gaps rather than as a
// wall of equally-weighted text.
func (s styles) value(text string) string {
	switch text {
	case "(none)", "(not set)", "(none — environment only)", "environment only (no config file)":
		return s.muted(text)
	}
	return text
}

// Status markers. reportProbe and the doctor/login lines share these so one
// glyph never means two different things across commands.
func (s styles) markOK() string      { return s.success("✓") }
func (s styles) markFail() string    { return s.failure("✗") }
func (s styles) markUnknown() string { return s.warning("?") }
func (s styles) bullet(n int) string { return s.accent(fmt.Sprintf("%d.", n)) }
func (s styles) arrow(text string) string {
	return s.muted("→ ") + s.url(text)
}

// previewBlock colors a staged write's preview for the terminal.
//
// The preview text itself must stay plain: it is a field of WriteResult, which
// MCP serializes to JSON and an agent reads. So the coloring happens here, on
// the way to stderr, by recognizing the lines previewText() writes — the
// header, the destructive warning, and the line carrying the confirm token.
func (s styles) previewBlock(text string) string {
	if !s.enabled {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "PREVIEW"):
			lines[i] = s.header(line)
		case strings.HasPrefix(line, "DESTRUCTIVE"):
			lines[i] = s.failure(line)
		case strings.HasPrefix(line, "To apply,"):
			lines[i] = s.accent(line)
		case strings.HasPrefix(line, "Nothing has been changed yet."):
			lines[i] = s.muted(line)
		}
	}
	return strings.Join(lines, "\n")
}
