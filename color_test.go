package main

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// ansiRE matches the SGR escapes styles emits, so a test can assert on what a
// user would actually read on screen.
var ansiRE = regexp.MustCompile("\033\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// withColorMode sets the --color mode for one test and restores it after, so a
// test that forces color cannot leak into the next one.
func withColorMode(t *testing.T, mode string) {
	t.Helper()
	prev := colorMode
	colorMode = mode
	t.Cleanup(func() { colorMode = prev })
}

func TestValidateColorMode(t *testing.T) {
	for _, mode := range []string{"auto", "always", "never"} {
		if err := validateColorMode(mode); err != nil {
			t.Errorf("--color %s should be accepted: %v", mode, err)
		}
	}
	if err := validateColorMode("yes"); err == nil {
		t.Error("an unknown --color value should be rejected, not silently treated as auto")
	}
}

// TestNewStylesEnablement is the whole safety story of this package: color is
// on only where it can be displayed, and any of three signals turns it off.
func TestNewStylesEnablement(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		noColor string
		term    string
		out     any // "buffer" or "devnull" or "tty-ish file"
		want    bool
	}{
		{name: "a buffer never gets color", mode: "auto", out: "buffer", want: false},
		{name: "--color never wins over everything", mode: "never", out: "tty", want: false},
		{name: "--color always wins over a buffer", mode: "always", out: "buffer", want: true},
		{name: "--color always wins over NO_COLOR", mode: "always", noColor: "1", out: "buffer", want: true},
		{name: "NO_COLOR disables auto", mode: "auto", noColor: "1", out: "tty", want: false},
		{name: "an empty NO_COLOR does not disable", mode: "auto", noColor: "", out: "buffer", want: false},
		{name: "TERM=dumb disables auto", mode: "auto", term: "dumb", out: "tty", want: false},
		{name: "a regular file is not a terminal", mode: "auto", out: "file", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withColorMode(t, tc.mode)
			t.Setenv("NO_COLOR", tc.noColor)
			t.Setenv("TERM", tc.term)

			var out interface{ Write([]byte) (int, error) }
			switch tc.out {
			case "buffer", "tty":
				// There is no portable way to hand a test a real terminal, so
				// the "tty" cases are the ones whose answer must not depend on
				// the writer at all: --color and the environment decide first.
				out = &bytes.Buffer{}
			case "file":
				f, err := os.CreateTemp(t.TempDir(), "out")
				if err != nil {
					t.Fatalf("temp file: %v", err)
				}
				defer f.Close()
				out = f
			}
			if got := newStyles(out).enabled; got != tc.want {
				t.Errorf("newStyles(...).enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStylesWrap(t *testing.T) {
	plain := styles{}
	if got := plain.success("READY"); got != "READY" {
		t.Errorf("a disabled styles must not add escapes, got %q", got)
	}
	colored := styles{enabled: true}
	if got := colored.success("READY"); got != "\033[32mREADY\033[0m" {
		t.Errorf("success(READY) = %q", got)
	}
	if got := colored.success(""); got != "" {
		t.Errorf("empty text should stay empty, got %q — a bare escape pair is not a value", got)
	}
}

// TestStylesValueDimsPlaceholders keeps "(none)" from reading with the same
// weight as a real setting in `config show` and `doctor`.
func TestStylesValueDimsPlaceholders(t *testing.T) {
	s := styles{enabled: true}
	if got := s.value("(none)"); !strings.Contains(got, "\033[2m") {
		t.Errorf("(none) should render dim, got %q", got)
	}
	if got := s.value("com.example.app"); got != "com.example.app" {
		t.Errorf("a real value should render plain, got %q", got)
	}
}

// TestStatusLineColorsTheVerdict checks both halves at once: the verdict word
// carries the color, and the text a user reads is unchanged by it.
func TestStatusLineColorsTheVerdict(t *testing.T) {
	tests := []struct {
		res  liveResult
		err  error
		code string
	}{
		{liveOK, nil, "\033[32m"},           // green
		{liveOffline, nil, "\033[32m"},      // green
		{liveInconclusive, nil, "\033[33m"}, // yellow
		{liveFailed, nil, "\033[31m"},       // red
		{liveUnconfigured, errors.New("no credentials"), "\033[31m"},
	}
	for _, tc := range tests {
		colored := statusLine(styles{enabled: true}, tc.res, tc.err)
		if !strings.Contains(colored, tc.code) {
			t.Errorf("statusLine(%v) = %q, want escape %q", tc.res, colored, tc.code)
		}
		if got, want := stripANSI(colored), statusLine(styles{}, tc.res, tc.err); got != want {
			t.Errorf("color changed the text: %q vs %q", got, want)
		}
	}
}

// TestReportProbeStaysReadableInColor guards the same property for the probe
// lines: the marker is colored, the message is not rewritten.
func TestReportProbeStaysReadableInColor(t *testing.T) {
	withColorMode(t, "always")
	var buf bytes.Buffer
	reportProbe(&buf, "edit probe:         ", errors.New("connection refused"))
	if !strings.Contains(buf.String(), "\033[") {
		t.Fatal("--color always should have colored the probe line")
	}
	if got := stripANSI(buf.String()); !strings.Contains(got, "? could not reach the API: connection refused") {
		t.Errorf("probe line reads as %q", got)
	}
}

// TestFormatTableColorPreservesAlignment is the bug this design exists to
// avoid: an escape code has no display width, so a colored header must still
// pad to exactly the same columns as a plain one.
func TestFormatTableColorPreservesAlignment(t *testing.T) {
	rows := rawRows(t,
		map[string]any{"track": "production", "status": "inProgress"},
		map[string]any{"track": "internal", "status": "completed"},
	)
	fields := []string{"track", "status"}
	plain := formatTable(styles{}, rows, fields)
	colored := formatTable(styles{enabled: true}, rows, fields)
	if !strings.Contains(colored, "\033[") {
		t.Fatal("an enabled styles should have colored the header")
	}
	if got := stripANSI(colored); got != plain {
		t.Errorf("colored table does not match the plain one once escapes are stripped:\n%q\nvs\n%q", got, plain)
	}
}

// TestMachineOutputIsNeverColored is the contract the whole file rests on:
// anything a pipeline parses stays byte-identical even with --color always.
func TestMachineOutputIsNeverColored(t *testing.T) {
	withColorMode(t, "always")
	res := tracksResult{Tracks: rawRows(t, map[string]any{"track": "production"})}
	for _, format := range []string{"json", "csv"} {
		var buf bytes.Buffer
		if err := printResult(&buf, format, res); err != nil {
			t.Fatalf("printResult(%s): %v", format, err)
		}
		if strings.Contains(buf.String(), "\033[") {
			t.Errorf("--format %s leaked escape codes: %q", format, buf.String())
		}
	}
}

// TestPreviewBlockLeavesTheStagedTextAlone checks that coloring a preview is
// purely presentational: the WriteResult field MCP serializes is untouched,
// and the terminal rendering says the same words.
func TestPreviewBlockLeavesTheStagedTextAlone(t *testing.T) {
	p := &PendingMutation{Tool: "play_halt_release", PackageName: "com.example.app", Summary: "halt production", Token: "abc123"}
	plain := p.previewText()

	if strings.Contains(plain, "\033[") {
		t.Fatal("previewText must stay plain — MCP serializes it into JSON")
	}
	colored := styles{enabled: true}.previewBlock(plain)
	if !strings.Contains(colored, "\033[") {
		t.Fatal("previewBlock should have colored the preview")
	}
	if got := stripANSI(colored); got != plain {
		t.Errorf("previewBlock rewrote the text:\n%q\nvs\n%q", got, plain)
	}
	if got := (styles{}).previewBlock(plain); got != plain {
		t.Errorf("a disabled styles must return the preview untouched, got %q", got)
	}
}

// TestPreviewBlockMarksTheDestructiveLine is the one line whose color carries
// meaning rather than decoration.
func TestPreviewBlockMarksTheDestructiveLine(t *testing.T) {
	text := "PREVIEW — play_halt_release on com.example.app\nDESTRUCTIVE — a second confirmation is required."
	colored := styles{enabled: true}.previewBlock(text)
	for _, line := range strings.Split(colored, "\n") {
		if strings.Contains(stripANSI(line), "DESTRUCTIVE") && !strings.HasPrefix(line, "\033[31m") {
			t.Errorf("the destructive warning should be red, got %q", line)
		}
	}
}

// TestColorFlagIsRegistered keeps the escape hatch discoverable: a user who
// pipes rollout somewhere it cannot detect needs a documented way to turn
// color off (and CI a way to force it on).
func TestColorFlagIsRegistered(t *testing.T) {
	flag := rootCmd.PersistentFlags().Lookup("color")
	if flag == nil {
		t.Fatal("--color was not registered on the root command")
	}
	if flag.DefValue != "auto" {
		t.Errorf("--color default = %q, want auto", flag.DefValue)
	}
}

// TestConfigShowKeepsItsColumnInColor renders the settings report with color
// forced on and checks that every value still starts in the same column once
// the escapes are stripped — the report is read as a column, and a dimmed key
// must not shift it.
func TestConfigShowKeepsItsColumnInColor(t *testing.T) {
	clearPlayEnv(t)
	t.Setenv("PLAY_SERVICE_ACCOUNT_JSON", testServiceAccountJSON)
	original := configPath
	configPath = writeConfig(t, "")
	t.Cleanup(func() { configPath = original })

	withColorMode(t, "always")
	var colored bytes.Buffer
	if err := playShowConfig(&colored); err != nil {
		t.Fatalf("playShowConfig: %v", err)
	}
	if !strings.Contains(colored.String(), "\033[") {
		t.Fatal("--color always should have colored the settings report")
	}

	withColorMode(t, "never")
	var plain bytes.Buffer
	if err := playShowConfig(&plain); err != nil {
		t.Fatalf("playShowConfig: %v", err)
	}
	if got := stripANSI(colored.String()); got != plain.String() {
		t.Errorf("color changed the report:\n%s\nvs\n%s", got, plain.String())
	}

	column := -1
	for _, line := range strings.Split(strings.TrimRight(plain.String(), "\n"), "\n") {
		at := strings.Index(line, ": ")
		if at < 0 {
			continue
		}
		// The value starts after the padding that follows the colon.
		rest := line[at+1:]
		start := at + 1 + (len(rest) - len(strings.TrimLeft(rest, " ")))
		if column == -1 {
			column = start
			continue
		}
		if start != column {
			t.Errorf("value on %q starts at column %d, want %d", line, start, column)
		}
	}
	if column == -1 {
		t.Fatal("the settings report printed no key: value lines")
	}
}
