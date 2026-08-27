package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// tracksResult stands in for a real read-tool result while the tool issues are
// still landing: printResult only cares that a result can produce rows.
type tracksResult struct {
	Tracks []json.RawMessage `json:"tracks"`
}

func (r tracksResult) tableRows() ([]json.RawMessage, []string) {
	return r.Tracks, []string{"track", "releases.status", "release_notes"}
}

func rawRows(t *testing.T, values ...any) []json.RawMessage {
	t.Helper()
	rows := make([]json.RawMessage, len(values))
	for i, v := range values {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal row %d: %v", i, err)
		}
		rows[i] = b
	}
	return rows
}

func TestPrintResultFormats(t *testing.T) {
	res := tracksResult{Tracks: rawRows(t,
		map[string]any{
			"track":        "production",
			"releases":     map[string]any{"status": "inProgress"},
			"releaseNotes": "Bug fixes, and a comma",
		},
		map[string]any{"track": "internal"},
	)}

	tests := []struct {
		name    string
		format  string
		want    []string
		notWant []string
		wantErr bool
	}{
		{
			name:   "json is the default",
			format: "",
			want:   []string{`"tracks"`, `"production"`},
		},
		{
			name:   "table renders declared columns",
			format: "table",
			// release_notes resolves against the camelCase key the API returns,
			// and the missing cell on the second row stays blank rather than
			// dropping the row.
			want: []string{"track", "releases.status", "release_notes", "inProgress", "Bug fixes, and a comma", "internal"},
		},
		{
			name: "csv quotes embedded commas",
			// A release note with a comma must not silently become two columns.
			format: "csv",
			want:   []string{"track,releases.status,release_notes", `"Bug fixes, and a comma"`, "internal,,"},
		},
		{
			name:    "unknown format is rejected",
			format:  "yaml",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := printResult(&buf, tc.format, res)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for format %q", tc.format)
				}
				return
			}
			if err != nil {
				t.Fatalf("printResult: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output missing %q:\n%s", want, buf.String())
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(buf.String(), notWant) {
					t.Errorf("output should not contain %q:\n%s", notWant, buf.String())
				}
			}
		})
	}
}

// TestPrintResultRejectsRowlessTable is the honest-failure case: a result that
// has no rows cannot be tabulated, and saying so beats printing an empty table.
func TestPrintResultRejectsRowlessTable(t *testing.T) {
	var buf bytes.Buffer
	if err := printResult(&buf, "table", struct{ A int }{1}); err == nil {
		t.Fatal("expected an error rendering a non-row result as a table")
	}
}

func TestFormatTableEmpty(t *testing.T) {
	if got := formatTable(styles{}, nil, []string{"track"}); !strings.Contains(got, "No results found") {
		t.Errorf("empty table should say so, got %q", got)
	}
}

// TestResolveFieldLargeNumbers guards version codes: decoding through float64
// would round a large int64 and print the wrong artifact.
func TestResolveFieldLargeNumbers(t *testing.T) {
	row := rawRows(t, map[string]any{"versionCode": json.Number("9007199254740993")})[0]
	v, ok := decodeRow(row)
	if !ok {
		t.Fatal("decodeRow failed")
	}
	if got := resolveField(v, "version_code"); got != "9007199254740993" {
		t.Errorf("version code round-tripped as %q", got)
	}
}

func TestAddFormatFlagDefaultsToJSON(t *testing.T) {
	cmd := &cobra.Command{Use: "tracks"}
	var format string
	addFormatFlag(cmd, &format)

	flag := cmd.Flags().Lookup("format")
	if flag == nil {
		t.Fatal("--format was not registered")
	}
	if flag.DefValue != "json" {
		t.Errorf("--format default = %q, want json", flag.DefValue)
	}
	if err := cmd.Flags().Parse([]string{"--format", "table"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if format != "table" {
		t.Errorf("--format bound to %q", format)
	}
}
