package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// printJSON writes v as indented JSON followed by a newline. This is the
// default CLI output so results pipe cleanly into jq.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// rowSource is implemented by read-tool results whose rows can render as a
// table or CSV. fields gives the column order.
type rowSource interface {
	tableRows() (rows []json.RawMessage, fields []string)
}

// printResult renders a read-tool result for the CLI: json (the default,
// printing the full structured result) or table/csv over its rows. This is
// CLI-only shaping — MCP always returns the structured result.
func printResult(w io.Writer, format string, res any) error {
	f := strings.ToLower(strings.TrimSpace(format))
	switch f {
	case "", "json":
		return printJSON(w, res)
	case "table", "csv":
		rs, ok := res.(rowSource)
		if !ok {
			return fmt.Errorf("this command cannot render %s output", f)
		}
		rows, fields := rs.tableRows()
		// CSV is data — it never gets color, even on a terminal, because the
		// usual next step is a redirect into a file a spreadsheet will read.
		rendered := formatTable(newStyles(w), rows, fields)
		if f == "csv" {
			rendered = formatCSV(rows, fields)
		}
		_, err := fmt.Fprint(w, rendered)
		return err
	default:
		return fmt.Errorf("unknown format %q — use json, table, or csv", format)
	}
}

// addFormatFlag registers the shared --format flag on a read command.
func addFormatFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "format", "json", "output format: json, table, or csv")
}

// formatTable renders rows as an aligned text table over the named fields.
//
// Only the header and the rule under it are styled, and only after every width
// has been measured from the plain text — an escape code has no display width,
// so coloring a cell before padding it would push every later column out of
// alignment.
func formatTable(s styles, rows []json.RawMessage, fields []string) string {
	if len(rows) == 0 {
		return s.muted("No results found.") + "\n"
	}
	widths := make([]int, len(fields))
	for i, f := range fields {
		widths[i] = displayWidth(f)
	}
	cells := make([][]string, len(rows))
	for ri, r := range rows {
		v, _ := decodeRow(r)
		row := make([]string, len(fields))
		for ci, f := range fields {
			val := resolveField(v, f)
			row[ci] = val
			if w := displayWidth(val); w > widths[ci] {
				widths[ci] = w
			}
		}
		cells[ri] = row
	}

	var b strings.Builder
	header := make([]string, len(fields))
	for i, f := range fields {
		header[i] = padRight(f, widths[i])
	}
	b.WriteString(s.header(strings.Join(header, " | ")))
	b.WriteByte('\n')

	sep := make([]string, len(widths))
	for i, w := range widths {
		sep[i] = strings.Repeat("-", w)
	}
	b.WriteString(s.muted(strings.Join(sep, "-+-")))
	b.WriteByte('\n')

	for _, row := range cells {
		out := make([]string, len(row))
		for i, c := range row {
			out[i] = padRight(c, widths[i])
		}
		b.WriteString(strings.TrimRight(strings.Join(out, " | "), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// formatCSV renders rows as RFC 4180 CSV over the named fields.
func formatCSV(rows []json.RawMessage, fields []string) string {
	var b strings.Builder
	b.WriteString(strings.Join(fields, ","))
	b.WriteByte('\n')
	for _, r := range rows {
		v, _ := decodeRow(r)
		vals := make([]string, len(fields))
		for i, f := range fields {
			val := resolveField(v, f)
			if strings.ContainsAny(val, ",\"\n") {
				val = `"` + strings.ReplaceAll(val, `"`, `""`) + `"`
			}
			vals[i] = val
		}
		b.WriteString(strings.Join(vals, ","))
		b.WriteByte('\n')
	}
	return b.String()
}

// decodeRow parses one row into a generic value, keeping numbers as
// json.Number so large IDs (version codes, developer IDs) never round-trip
// through float64.
func decodeRow(r json.RawMessage) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(r))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// resolveField reads a dotted path out of a decoded row and renders it as a
// display string. A missing path renders empty rather than failing the row:
// the Play API omits fields that do not apply (a track with no in-progress
// rollout has no userFraction), and a table should show a blank cell for that.
func resolveField(row any, field string) string {
	cur := row
	for _, part := range strings.Split(field, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		v, ok := m[part]
		if !ok {
			if v, ok = m[snakeToCamel(part)]; !ok {
				return ""
			}
		}
		cur = v
	}
	switch v := cur.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// snakeToCamel converts release_notes to releaseNotes, so a --format table
// column can be named the way the CLI flag is even though the Play API returns
// camelCase JSON.
func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	if len(parts) == 1 {
		return s
	}
	var b strings.Builder
	b.WriteString(parts[0])
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

func displayWidth(s string) int { return utf8.RuneCountInString(s) }

func padRight(s string, w int) string {
	if d := displayWidth(s); d < w {
		return s + strings.Repeat(" ", w-d)
	}
	return s
}
