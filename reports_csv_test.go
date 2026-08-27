package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

// utf16LE encodes text the way Play writes its CSV exports: UTF-16
// little-endian behind a byte-order mark.
func utf16LE(text string) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xFE})
	for _, unit := range utf16.Encode([]rune(text)) {
		b.WriteByte(byte(unit))
		b.WriteByte(byte(unit >> 8))
	}
	return b.Bytes()
}

// utf16BE is the same in big-endian, which some older exports used.
func utf16BE(text string) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFE, 0xFF})
	for _, unit := range utf16.Encode([]rune(text)) {
		b.WriteByte(byte(unit >> 8))
		b.WriteByte(byte(unit))
	}
	return b.Bytes()
}

func TestDecodeReportText(t *testing.T) {
	const text = "Date,Daily Average Rating\n2026-07-01,4.5\n"

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"utf-16 little-endian with BOM", utf16LE(text), text},
		{"utf-16 big-endian with BOM", utf16BE(text), text},
		{"utf-8 with BOM", append([]byte{0xEF, 0xBB, 0xBF}, text...), text},
		{"plain utf-8", []byte(text), text},
		// Play has shipped BOM-less UTF-16 files. Every ASCII character is
		// followed by a zero byte, which no valid UTF-8 report contains.
		{"utf-16 little-endian without BOM", utf16LE(text)[2:], text},
		{"empty", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeReportText(tc.data)
			if err != nil {
				t.Fatalf("decodeReportText: %v", err)
			}
			if got != tc.want {
				t.Errorf("decoded %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeReportTextRejectsGarbage(t *testing.T) {
	// A truncated UTF-16 stream is half a character short; decoding it as if it
	// were whole would silently drop or invent one.
	if _, err := decodeReportText([]byte{0xFF, 0xFE, 0x41}); err == nil {
		t.Error("expected an error for a truncated UTF-16 report")
	}
	// Invalid UTF-8 with no BOM and no NUL pattern is not a CSV export at all.
	if _, err := decodeReportText([]byte{0x41, 0x42, 0xC3, 0x28}); err == nil {
		t.Error("expected an error for bytes that are neither UTF-16 nor UTF-8")
	}
}

func TestParseReportCSV(t *testing.T) {
	const text = "Date,Package Name,Daily Device Installs,Store Listing Acquisitions (Unique Users)\n" +
		"2026-07-01,com.example.app,120,340\n" +
		"2026-07-02,com.example.app,131,352\n"

	table, err := parseReportCSV(text, 0)
	if err != nil {
		t.Fatalf("parseReportCSV: %v", err)
	}
	want := []string{"date", "package_name", "daily_device_installs", "store_listing_acquisitions_unique_users"}
	if !reflect.DeepEqual(table.Columns, want) {
		t.Errorf("columns = %v, want %v", table.Columns, want)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(table.Rows))
	}
	if table.Rows[0]["daily_device_installs"] != "120" {
		t.Errorf("row 1 = %v", table.Rows[0])
	}
	// Values stay strings: a version code or a leading-zero country code that
	// went through a float would come back wrong.
	if table.Rows[1]["date"] != "2026-07-02" {
		t.Errorf("row 2 date = %q", table.Rows[1]["date"])
	}
}

func TestParseReportCSVKeepsDuplicateColumns(t *testing.T) {
	// Two headers with the same normalized name would collide in one row map,
	// silently dropping a column.
	table, err := parseReportCSV("Date,Total,Total\n2026-07-01,1,2\n", 0)
	if err != nil {
		t.Fatalf("parseReportCSV: %v", err)
	}
	if !reflect.DeepEqual(table.Columns, []string{"date", "total", "total_2"}) {
		t.Fatalf("columns = %v", table.Columns)
	}
	if table.Rows[0]["total"] != "1" || table.Rows[0]["total_2"] != "2" {
		t.Errorf("row = %v", table.Rows[0])
	}
}

func TestParseReportCSVEmptyFile(t *testing.T) {
	table, err := parseReportCSV("", 0)
	if err != nil {
		t.Fatalf("parseReportCSV: %v", err)
	}
	if len(table.Columns) != 0 || len(table.Rows) != 0 {
		t.Errorf("empty report parsed to %+v", table)
	}
}

func TestSnakeCase(t *testing.T) {
	tests := map[string]string{
		"Date":                        "date",
		"Daily Average Rating":        "daily_average_rating",
		"Country/Region":              "country_region",
		"  Total   User  Installs  ":  "total_user_installs",
		"Store Listing Visitors (7d)": "store_listing_visitors_7d",
		"":                            "",
	}
	for in, want := range tests {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

// zipWith builds an archive holding the named members.
func zipWith(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range members {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := f.Write(body); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestExtractZippedCSV(t *testing.T) {
	body := utf16LE("Order Number,Amount\n1,2\n")
	archive, err := extractZippedCSV(zipWith(t, map[string][]byte{"salesreport_202607.csv": body}))
	if err != nil {
		t.Fatalf("extractZippedCSV: %v", err)
	}
	if archive.Member != "salesreport_202607.csv" {
		t.Errorf("member = %q", archive.Member)
	}
	if !bytes.Equal(archive.Data, body) {
		t.Error("archive data does not round-trip")
	}
}

// TestExtractZippedCSVJoinsEveryMember: an earnings archive can hold several
// CSVs. Reading only the first would report a slice of the payout as the whole
// of it — partial success is not success.
func TestExtractZippedCSVJoinsEveryMember(t *testing.T) {
	archive, err := extractZippedCSV(zipWith(t, map[string][]byte{
		"earnings_202607_b.csv": utf16LE("Order,Amount\nGPA.2,2.00\n"),
		"earnings_202607_a.csv": utf16LE("Order,Amount\nGPA.1,1.00\n"),
		"readme.txt":            []byte("ignore me"),
	}))
	if err != nil {
		t.Fatalf("extractZippedCSV: %v", err)
	}
	if len(archive.Members) != 3 {
		t.Errorf("members = %v, want all three entries", archive.Members)
	}
	table, err := parseReportCSV(mustDecode(t, archive.Data), 0)
	if err != nil {
		t.Fatalf("parseReportCSV: %v", err)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("rows = %+v, want both members' rows", table.Rows)
	}
	// Members are joined in name order, and the header appears once.
	if table.Rows[0]["order"] != "GPA.1" || table.Rows[1]["order"] != "GPA.2" {
		t.Errorf("rows = %+v", table.Rows)
	}
	if !strings.Contains(archive.Member, "earnings_202607_a.csv") || !strings.Contains(archive.Member, "earnings_202607_b.csv") {
		t.Errorf("member = %q, want both named", archive.Member)
	}
}

// TestExtractZippedCSVRefusesToJoinDifferentShapes: CSVs that do not share a
// header are not one report, and stacking them would invent rows.
func TestExtractZippedCSVRefusesToJoinDifferentShapes(t *testing.T) {
	_, err := extractZippedCSV(zipWith(t, map[string][]byte{
		"a.csv": utf16LE("Order,Amount\nGPA.1,1.00\n"),
		"b.csv": utf16LE("Payout,Currency\n1.00,EUR\n"),
	}))
	if err == nil {
		t.Fatal("expected an error joining CSVs with different columns")
	}
	for _, want := range []string{"a.csv", "b.csv", "different columns"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

func mustDecode(t *testing.T, data []byte) string {
	t.Helper()
	text, err := decodeReportText(data)
	if err != nil {
		t.Fatalf("decodeReportText: %v", err)
	}
	return text
}

func TestExtractZippedCSVWithoutCSV(t *testing.T) {
	_, err := extractZippedCSV(zipWith(t, map[string][]byte{"notes.txt": []byte("nothing")}))
	if err == nil {
		t.Fatal("expected an error for an archive with no CSV")
	}
	if !strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("error should name what the archive held: %v", err)
	}
}

func TestExtractZippedCSVRejectsNonArchive(t *testing.T) {
	if _, err := extractZippedCSV([]byte("Date,Installs\n")); err == nil {
		t.Error("expected an error when the bytes are not a zip")
	}
}

// TestParseReportCSVDisambiguatesThreeDuplicates: a counter kept against the
// base name hands out `_2` twice, and the row map then loses a value outright.
func TestParseReportCSVDisambiguatesThreeDuplicates(t *testing.T) {
	table, err := parseReportCSV("Date,Total,Total,Total\n2026-07-01,1,2,3\n", 0)
	if err != nil {
		t.Fatalf("parseReportCSV: %v", err)
	}
	want := []string{"date", "total", "total_2", "total_3"}
	if !reflect.DeepEqual(table.Columns, want) {
		t.Fatalf("columns = %v, want %v", table.Columns, want)
	}
	row := table.Rows[0]
	if row["total"] != "1" || row["total_2"] != "2" || row["total_3"] != "3" {
		t.Errorf("row = %v, want every column preserved", row)
	}
}

// TestParseReportCSVRejectsAWiderRow: a row with more fields than the header
// means the file disagrees with itself. Keeping it would drop the trailing
// values and present the rest as complete — for a financial export that is a
// wrong number, not a gap.
func TestParseReportCSVRejectsAWiderRow(t *testing.T) {
	_, err := parseReportCSV("Date,Amount\n2026-07-01,1.00,SURPRISE\n", 0)
	if err == nil {
		t.Fatal("expected an error for a row wider than the header")
	}
	if !strings.Contains(err.Error(), "row 2") {
		t.Errorf("error should name the row: %v", err)
	}
	// A short row is a different thing: the export simply omits a value, and
	// the missing key is the honest representation of that.
	table, err := parseReportCSV("Date,Amount\n2026-07-01\n", 0)
	if err != nil {
		t.Fatalf("parseReportCSV: %v", err)
	}
	if _, ok := table.Rows[0]["amount"]; ok {
		t.Errorf("a missing value should be absent, not empty: %v", table.Rows[0])
	}
}

// TestParseReportCSVRetainsOnlyTheLimit: the row cap has to bound memory, not
// just the answer — a million-row sales export should not build a million maps
// to hand back two thousand.
func TestParseReportCSVRetainsOnlyTheLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("Date,Amount\n")
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "2026-07-%02d,%d.00\n", i, i)
	}

	table, err := parseReportCSV(b.String(), 3)
	if err != nil {
		t.Fatalf("parseReportCSV: %v", err)
	}
	if len(table.Rows) != 3 {
		t.Errorf("retained %d rows, want 3", len(table.Rows))
	}
	// The file's own size still has to be reported, or a capped answer cannot
	// say how much it left behind.
	if table.Total != 10 {
		t.Errorf("total = %d, want the file's 10", table.Total)
	}
	if table.Rows[0]["amount"] != "1.00" {
		t.Errorf("kept the wrong end: %+v", table.Rows)
	}
	// A row past the limit that disagrees with the header is still caught.
	if _, err := parseReportCSV("Date,Amount\n2026-07-01,1.00\n2026-07-02,2.00,SURPRISE\n", 1); err == nil {
		t.Error("expected a malformed row past the limit to be reported")
	}
}

// TestExtractZippedCSVRefusesAnOversizedArchive: the archive is compressed, so
// its own size says little about what extracting it costs. The declared size
// does, and it is worth trusting far enough to refuse rather than to allocate.
func TestExtractZippedCSVRefusesAnOversizedArchive(t *testing.T) {
	// Shrink the bound rather than building a half-gigabyte fixture: what is
	// under test is that the declared size is checked before extraction, not
	// the particular number it is checked against.
	original := maxReportBytes
	maxReportBytes = 64
	t.Cleanup(func() { maxReportBytes = original })

	// Highly compressible, so it is small on the wire and past the bound once
	// extracted — the shape a size check has to catch.
	big := bytes.Repeat([]byte("a,b\n"), 64)
	_, err := extractZippedCSV(zipWith(t, map[string][]byte{"huge.csv": big}))
	if err == nil {
		t.Fatal("expected an oversized archive to be refused")
	}
	if !strings.Contains(err.Error(), "gsutil cp") {
		t.Errorf("error should name a tool that can handle it: %v", err)
	}
}
