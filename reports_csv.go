package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Turning a Play CSV export into rows.
//
// Two things about these files trip up every first attempt: they are UTF-16
// (little-endian, with a BOM), and the financial ones are zipped. Neither is
// mentioned anywhere the file itself; a naive read yields a header full of NUL
// bytes, or a zip archive presented as text.

// decodeReportText converts a report's bytes to UTF-8 text.
//
// The BOM is authoritative when present. Without one, a second byte of NUL is
// the giveaway for UTF-16LE: every ASCII character in a UTF-16LE stream is
// followed by a zero byte, and no valid UTF-8 report contains a NUL at all.
func decodeReportText(data []byte) (string, error) {
	switch {
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE:
		return decodeUTF16(data[2:], false)
	case len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF:
		return decodeUTF16(data[2:], true)
	case len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF:
		return string(data[3:]), nil
	case len(data) >= 2 && data[0] != 0 && data[1] == 0:
		return decodeUTF16(data, false)
	}
	if !utf8.Valid(data) {
		return "", errors.New("report is neither UTF-16 nor valid UTF-8 — it may not be a CSV export")
	}
	return string(data), nil
}

// decodeUTF16 converts a UTF-16 byte stream (BOM already stripped) to UTF-8.
func decodeUTF16(data []byte, bigEndian bool) (string, error) {
	if len(data)%2 != 0 {
		return "", fmt.Errorf("truncated UTF-16 report: %d bytes is not a whole number of code units", len(data))
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		hi, lo := data[2*i+1], data[2*i]
		if bigEndian {
			hi, lo = data[2*i], data[2*i+1]
		}
		units[i] = uint16(hi)<<8 | uint16(lo)
	}
	return string(utf16.Decode(units)), nil
}

// reportTable is a parsed CSV export: the header order, kept because JSON
// objects have none and both `--format table` and `--format csv` need columns
// in the order the report published them, plus one string map per row.
//
// Values stay strings. A Play report mixes dates, package names, integers and
// rates, and coercing them here would turn a version code into a float and a
// leading-zero country code into a number.
type reportTable struct {
	Columns []string            `json:"columns"`
	Rows    []map[string]string `json:"rows"`
	// Total is how many rows the file held, which is not len(Rows) when a
	// limit was applied — a capped result still has to say how much there was.
	Total int `json:"total"`
}

// parseReportCSV parses decoded report text into columns and rows.
//
// Header names are normalized to snake_case so a JSON consumer addresses
// `daily_device_installs` rather than `"Daily Device Installs"`, matching how
// every other rollout result names its fields.
//
// limit caps how many rows are *retained*; 0 keeps them all. Rows past it are
// still read and counted, so Total is the file's own size — the point is that a
// million-row sales export does not have a million maps built for it when the
// caller asked for two thousand.
func parseReportCSV(text string, limit int) (*reportTable, error) {
	reader := csv.NewReader(strings.NewReader(text))
	// Play has changed the column set of these reports over time, and a month
	// that gained a column mid-file is still worth reading.
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err == io.EOF {
		return &reportTable{Columns: []string{}, Rows: []map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("parse report header: %w", err)
	}
	columns := normalizeHeader(header)

	table := &reportTable{Columns: columns, Rows: []map[string]string{}}
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse report row %d: %w", table.Total+2, err)
		}
		if len(record) > len(columns) {
			// The file disagrees with its own header. Keeping the row would
			// drop its trailing fields and present the rest as complete, which
			// for a financial export is a wrong number rather than a gap.
			return nil, fmt.Errorf("report row %d has %d fields but the header names %d — the file does not match its own header; read it with --out and inspect it", table.Total+2, len(record), len(columns))
		}
		table.Total++
		if limit > 0 && len(table.Rows) >= limit {
			continue
		}
		row := make(map[string]string, len(columns))
		for i, name := range columns {
			if i < len(record) {
				// Verbatim. A review's text is user-authored and may open or
				// close with whitespace; trimming it here would make the parsed
				// rows disagree with the file --out writes.
				row[name] = record[i]
			}
		}
		table.Rows = append(table.Rows, row)
	}
	return table, nil
}

// normalizeHeader snake_cases each column name and makes the set unique. A
// duplicate header would otherwise silently drop a column: two keys of the same
// name cannot coexist in one row map.
func normalizeHeader(header []string) []string {
	columns := make([]string, len(header))
	taken := map[string]bool{}
	for i, name := range header {
		base := snakeCase(name)
		if base == "" {
			base = fmt.Sprintf("column_%d", i+1)
		}
		// Probe for a free suffix rather than counting: with three columns of
		// the same name, a counter kept against the base would hand out
		// `_2` twice and one of the values would be overwritten.
		column := base
		for n := 2; taken[column]; n++ {
			column = fmt.Sprintf("%s_%d", base, n)
		}
		taken[column] = true
		columns[i] = column
	}
	return columns
}

// snakeCase lowercases a CSV header and reduces every run of non-alphanumeric
// characters to a single underscore: "Daily Average Rating" becomes
// daily_average_rating, "Store Listing Acquisitions (Unique Users)" becomes
// store_listing_acquisitions_unique_users.
func snakeCase(s string) string {
	var b strings.Builder
	pendingSeparator := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if pendingSeparator && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSeparator = false
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			if pendingSeparator && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSeparator = false
			b.WriteRune(r - 'A' + 'a')
		default:
			pendingSeparator = true
		}
	}
	return b.String()
}

// maxReportBytes bounds a report rollout will read into memory.
//
// These files are read whole: `--out` writes the entire document, and UTF-16
// decoding has no incremental form here. That is fine for every export Play
// actually produces — the largest are tens of megabytes — but "read whatever
// arrives" is not a promise a CLI can keep. Past this, the honest answer is to
// name a tool built for the job rather than to fail with an allocation error.
// It is a var so tests can shrink it; nothing else reassigns it.
var maxReportBytes int64 = 512 << 20

// zipArchive is what a financial report actually is: a zip holding its CSV.
type zipArchive struct {
	// Member names the CSV the rows came from, or all of them joined when an
	// archive held several.
	Member string
	// Members lists every entry in the archive.
	Members []string
	// Data is the CSV bytes, still in the archive's own encoding.
	Data []byte
}

// zipMember is one file read out of an archive.
type zipMember struct {
	name string
	data []byte
}

// extractZippedCSV pulls the CSV out of a sales or earnings archive.
//
// An archive can hold more than one CSV — an earnings report split by region,
// say — and reading just the first would report a slice of the payout as the
// whole of it. Partial success is not success, so every CSV member is read and
// the rows are joined. Members that do not share a header cannot be joined, and
// that is an error naming them rather than a number that is quietly wrong.
func extractZippedCSV(data []byte) (*zipArchive, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open report archive: %w", err)
	}
	archive := &zipArchive{}
	var csvs []zipMember
	var uncompressed uint64
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		archive.Members = append(archive.Members, f.Name)
		if !strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
			continue
		}
		// The archive is compressed, so its own size says little about what
		// extracting it costs. The declared size does, and it is worth trusting
		// far enough to refuse rather than to allocate.
		uncompressed += f.UncompressedSize64
		if uncompressed > uint64(maxReportBytes) {
			return nil, fmt.Errorf("report archive expands to more than %d MB — fetch it with `gsutil cp` and process it outside rollout", maxReportBytes>>20)
		}
		body, err := readZipEntry(f)
		if err != nil {
			return nil, err
		}
		csvs = append(csvs, zipMember{name: f.Name, data: body})
	}
	if len(csvs) == 0 {
		return nil, fmt.Errorf("report archive holds no CSV (members: %s)", strings.Join(archive.Members, ", "))
	}
	sort.Slice(csvs, func(i, j int) bool { return csvs[i].name < csvs[j].name })

	if len(csvs) == 1 {
		archive.Member, archive.Data = csvs[0].name, csvs[0].data
		return archive, nil
	}
	joined, names, err := joinCSVMembers(csvs)
	if err != nil {
		return nil, err
	}
	archive.Member, archive.Data = names, []byte(joined)
	return archive, nil
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s in report archive: %w", f.Name, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s from report archive: %w", f.Name, err)
	}
	return body, nil
}

// joinCSVMembers concatenates several CSV members into one document, keeping a
// single header. The members are decoded first because they can each carry
// their own byte-order mark, and splicing two UTF-16 files byte-wise would
// leave a BOM in the middle of the result.
func joinCSVMembers(csvs []zipMember) (joined, names string, err error) {
	var body strings.Builder
	var header string
	labels := make([]string, len(csvs))
	for i, member := range csvs {
		labels[i] = member.name
		text, err := decodeReportText(member.data)
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", member.name, err)
		}
		head, rows, _ := strings.Cut(text, "\n")
		head = strings.TrimSuffix(head, "\r")
		if i == 0 {
			header = head
			body.WriteString(head)
			body.WriteString("\n")
		} else if head != header {
			return "", "", fmt.Errorf("report archive holds CSVs with different columns (%s), which cannot be read as one report — extract them by hand", strings.Join(labels, ", "))
		}
		rows = strings.TrimLeft(rows, "\n")
		if rows == "" {
			continue
		}
		body.WriteString(rows)
		if !strings.HasSuffix(rows, "\n") {
			body.WriteString("\n")
		}
	}
	return body.String(), strings.Join(labels, ", "), nil
}
