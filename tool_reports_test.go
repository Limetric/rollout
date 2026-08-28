package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The exports are monthly files; a window that crosses a month boundary has to
// read both, which is what these fixtures are shaped to prove.
const (
	juneInstalls = "Date,Package Name,Daily Device Installs,Daily Device Uninstalls,Active Device Installs\n" +
		"2026-06-28,com.example.app,90,9,4900\n" +
		"2026-06-29,com.example.app,100,10,5000\n" +
		"2026-06-30,com.example.app,110,11,5100\n"
	julyInstalls = "Date,Package Name,Daily Device Installs,Daily Device Uninstalls,Active Device Installs\n" +
		"2026-07-01,com.example.app,120,12,5200\n" +
		"2026-07-02,com.example.app,130,13,5300\n"
	julyRatings = "Date,Package Name,Daily Average Rating,Total Average Rating\n" +
		"2026-07-01,com.example.app,4.5,4.3\n" +
		"2026-07-02,com.example.app,4.6,4.3\n"
	julyCountryInstalls = "Date,Package Name,Country,Daily Device Installs\n" +
		"2026-07-01,com.example.app,NL,12\n" +
		"2026-07-01,com.example.app,US,60\n" +
		"2026-07-02,com.example.app,NL,14\n"
)

// reportsFixture is a bucket holding one month of most kinds, plus the two
// awkward ones: a zipped financial report and a subscriptions month with two
// product ids in it.
func reportsFixture(t *testing.T) fakeBucket {
	t.Helper()
	return fakeBucket{
		"stats/installs/installs_com.example.app_202606_overview.csv":                                utf16LE(juneInstalls),
		"stats/installs/installs_com.example.app_202607_overview.csv":                                utf16LE(julyInstalls),
		"stats/installs/installs_com.example.app_202607_country.csv":                                 utf16LE(julyCountryInstalls),
		"stats/ratings/ratings_com.example.app_202607_overview.csv":                                  utf16LE(julyRatings),
		"reviews/reviews_com.example.app_202607.csv":                                                 utf16LE("Review Submit Date and Time,Star Rating\n2026-07-01T10:00:00Z,5\n"),
		"financial-stats/subscriptions/subscriptions_com.example.app_sku.monthly_202607_country.csv": utf16LE("Date,Country\n2026-07-01,NL\n"),
		"financial-stats/subscriptions/subscriptions_com.example.app_sku.yearly_202607_country.csv":  utf16LE("Date,Country\n2026-07-01,US\n"),
		"sales/salesreport_202607.zip": zipWith(t, map[string][]byte{
			"salesreport_202607.csv": utf16LE("Order Number,Amount\nGPA.1,4.99\n"),
		}),
	}
}

func TestRunReportsList(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	t.Run("labels every object it lists", func(t *testing.T) {
		res, err := runReportsList(context.Background(), client, ReportsListArgs{})
		if err != nil {
			t.Fatalf("runReportsList: %v", err)
		}
		if res.Bucket != testReportsBucket {
			t.Errorf("bucket = %q", res.Bucket)
		}
		if len(res.Objects) != 8 {
			t.Fatalf("objects = %d, want the whole fixture: %+v", len(res.Objects), res.Objects)
		}
		byName := map[string]ReportObject{}
		for _, obj := range res.Objects {
			byName[obj.Name] = obj
		}
		installs := byName["stats/installs/installs_com.example.app_202607_country.csv"]
		if installs.Kind != "installs" || installs.Month != "2026-07" || installs.Dimension != "country" {
			t.Errorf("installs object = %+v", installs)
		}
		// A reviews file's trailing segment is the month, not a breakdown —
		// labelling one would invite a --dimension the kind does not have.
		reviews := byName["reviews/reviews_com.example.app_202607.csv"]
		if reviews.Kind != "reviews" || reviews.Dimension != "" {
			t.Errorf("reviews object = %+v", reviews)
		}
		sales := byName["sales/salesreport_202607.zip"]
		if sales.Kind != "sales" || sales.Month != "2026-07" || sales.Dimension != "" {
			t.Errorf("sales object = %+v", sales)
		}
		if !strings.Contains(res.Note, "arrears") {
			t.Errorf("note should carry the export lag: %q", res.Note)
		}
	})

	t.Run("narrows to one kind and month", func(t *testing.T) {
		res, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "installs", Month: "2026-07"})
		if err != nil {
			t.Fatalf("runReportsList: %v", err)
		}
		if len(res.Objects) != 2 {
			t.Fatalf("objects = %+v, want the two July installs files", res.Objects)
		}
		for _, obj := range res.Objects {
			if obj.Month != "2026-07" || obj.Kind != "installs" {
				t.Errorf("unexpected object %+v", obj)
			}
		}
	})

	t.Run("caps the listing and says so", func(t *testing.T) {
		res, err := runReportsList(context.Background(), client, ReportsListArgs{Max: 2})
		if err != nil {
			t.Fatalf("runReportsList: %v", err)
		}
		if len(res.Objects) != 2 || !res.Truncated {
			t.Fatalf("objects = %d truncated = %v, want a reported cap", len(res.Objects), res.Truncated)
		}
		if !strings.Contains(res.Note, "--kind") {
			t.Errorf("a capped listing should say how to narrow it: %q", res.Note)
		}
	})

	t.Run("an empty bucket is an answer, not a failure", func(t *testing.T) {
		empty, _ := newBucketClient(t, fakeBucket{})
		res, err := runReportsList(context.Background(), empty, ReportsListArgs{})
		if err != nil {
			t.Fatalf("runReportsList: %v", err)
		}
		if len(res.Objects) != 0 || res.Note == "" {
			t.Errorf("empty listing = %+v", res)
		}
	})

	t.Run("rejects an unknown kind locally", func(t *testing.T) {
		_, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "downloads"})
		if err == nil || !strings.Contains(err.Error(), "installs") {
			t.Errorf("error should name the kinds that exist: %v", err)
		}
	})

	t.Run("rejects an unparseable month locally", func(t *testing.T) {
		_, err := runReportsList(context.Background(), client, ReportsListArgs{Month: "July 2026"})
		if err == nil || !strings.Contains(err.Error(), "YYYY-MM") {
			t.Errorf("error should name the month format: %v", err)
		}
	})
}

func TestRunReport(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	t.Run("resolves and parses the default dimension", func(t *testing.T) {
		res, err := runReport(context.Background(), client, ReportArgs{Kind: "installs", Month: "2026-07"})
		if err != nil {
			t.Fatalf("runReport: %v", err)
		}
		if res.Object != "stats/installs/installs_com.example.app_202607_overview.csv" {
			t.Errorf("object = %q", res.Object)
		}
		if res.Kind != "installs" || res.Month != "2026-07" || res.Dimension != "overview" {
			t.Errorf("result = %+v", res)
		}
		want := []string{"date", "package_name", "daily_device_installs", "daily_device_uninstalls", "active_device_installs"}
		if !reflect.DeepEqual(res.Columns, want) {
			t.Errorf("columns = %v, want %v", res.Columns, want)
		}
		if res.RowCount != 2 || res.Rows[0]["daily_device_installs"] != "120" {
			t.Errorf("rows = %+v", res.Rows)
		}
	})

	t.Run("reads a named dimension", func(t *testing.T) {
		res, err := runReport(context.Background(), client, ReportArgs{Kind: "installs", Month: "2026-07", Dimension: "country"})
		if err != nil {
			t.Fatalf("runReport: %v", err)
		}
		if res.Object != "stats/installs/installs_com.example.app_202607_country.csv" || res.RowCount != 3 {
			t.Errorf("result = %+v", res)
		}
	})

	t.Run("rejects a dimension the kind does not publish", func(t *testing.T) {
		_, err := runReport(context.Background(), client, ReportArgs{Kind: "installs", Month: "2026-07", Dimension: "traffic_source"})
		if err == nil || !strings.Contains(err.Error(), "os_version") {
			// A wrong dimension must not surface as "no such report", which
			// reads as missing data rather than a wrong argument.
			t.Errorf("error should list the accepted dimensions: %v", err)
		}
	})

	t.Run("a missing month names the months that exist", func(t *testing.T) {
		_, err := runReport(context.Background(), client, ReportArgs{Kind: "installs", Month: "2026-05"})
		if err == nil {
			t.Fatal("expected an error for a month with no export")
		}
		if !strings.Contains(err.Error(), "2026-06") || !strings.Contains(err.Error(), "2026-07") {
			t.Errorf("error should list the exported months: %v", err)
		}
	})

	t.Run("an ambiguous month refuses to guess", func(t *testing.T) {
		// A subscriptions month holds one file per product id. Reading the
		// first would report one product's numbers as the app's.
		_, err := runReport(context.Background(), client, ReportArgs{Kind: "subscriptions", Month: "2026-07", Dimension: "country"})
		if err == nil {
			t.Fatal("expected an error when several files match")
		}
		if !strings.Contains(err.Error(), "--object") || !strings.Contains(err.Error(), "sku.monthly") {
			t.Errorf("error should name the candidates and the way out: %v", err)
		}
	})

	t.Run("reads an exact object name", func(t *testing.T) {
		res, err := runReport(context.Background(), client, ReportArgs{
			Object: "financial-stats/subscriptions/subscriptions_com.example.app_sku.yearly_202607_country.csv",
		})
		if err != nil {
			t.Fatalf("runReport: %v", err)
		}
		if res.Kind != "subscriptions" || res.Month != "2026-07" || res.RowCount != 1 {
			t.Errorf("result = %+v", res)
		}
	})

	t.Run("unzips a financial report", func(t *testing.T) {
		res, err := runReport(context.Background(), client, ReportArgs{Kind: "sales", Month: "2026-07"})
		if err != nil {
			t.Fatalf("runReport: %v", err)
		}
		if res.ArchiveMember != "salesreport_202607.csv" {
			t.Errorf("archive member = %q", res.ArchiveMember)
		}
		if res.RowCount != 1 || res.Rows[0]["order_number"] != "GPA.1" {
			t.Errorf("rows = %+v", res.Rows)
		}
		// A financial report covers the developer account, not one app; naming
		// an app on it would suggest a filter that was never applied.
		if res.PackageName != "" {
			t.Errorf("package name = %q, want none on an account-wide report", res.PackageName)
		}
	})

	t.Run("needs a kind or an object", func(t *testing.T) {
		_, err := runReport(context.Background(), client, ReportArgs{Month: "2026-07"})
		if err == nil || !strings.Contains(err.Error(), "--object") {
			t.Errorf("error should offer both ways in: %v", err)
		}
	})
}

func TestRunReportCapsRowsButWritesTheWholeFile(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))
	out := filepath.Join(t.TempDir(), "installs.csv")

	res, err := runReport(context.Background(), client, ReportArgs{
		Kind: "installs", Month: "2026-07", Dimension: "country", MaxRows: 1, Out: out,
	})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if len(res.Rows) != 1 || !res.Truncated {
		t.Fatalf("rows = %d truncated = %v, want a reported cap", len(res.Rows), res.Truncated)
	}
	// RowCount is the file's real size, so a truncated answer still says how
	// much there was.
	if res.RowCount != 3 {
		t.Errorf("row count = %d, want the file's own 3", res.RowCount)
	}
	if res.OutPath != out {
		t.Errorf("out path = %q", res.OutPath)
	}

	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read written report: %v", err)
	}
	// The row cap is a context guard on the JSON result, never on the file.
	if string(written) != julyCountryInstalls {
		t.Errorf("written file = %q, want the complete UTF-8 CSV", written)
	}
	if len(written) > 1 && written[1] == 0 {
		t.Error("the written file is still UTF-16; it should be transcoded")
	}
}

func TestRunReportOutPathFailure(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))
	_, err := runReport(context.Background(), client, ReportArgs{
		Kind: "installs", Month: "2026-07", Out: filepath.Join(t.TempDir(), "missing-dir", "x.csv"),
	})
	if err == nil || !strings.Contains(err.Error(), "write report") {
		t.Errorf("error should name the write that failed: %v", err)
	}
}

func TestRunInstallsSpansMonths(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	res, err := runInstalls(context.Background(), client, DailyReportArgs{Days: 6, End: "2026-07-04"})
	if err != nil {
		t.Fatalf("runInstalls: %v", err)
	}
	if res.Start != "2026-06-29" || res.End != "2026-07-04" {
		t.Fatalf("window = %s..%s", res.Start, res.End)
	}
	if !reflect.DeepEqual(res.Months, []string{"2026-06", "2026-07"}) {
		t.Errorf("months = %v, want both files read", res.Months)
	}
	// 2026-06-28 is in the June file but outside the window: --days 6 has to
	// mean six days, not "everything in the months it touches".
	dates := make([]string, len(res.Rows))
	for i, row := range res.Rows {
		dates[i] = row["date"]
	}
	want := []string{"2026-06-29", "2026-06-30", "2026-07-01", "2026-07-02"}
	if !reflect.DeepEqual(dates, want) {
		t.Errorf("dates = %v, want %v", dates, want)
	}
	// The trailing days have not been exported yet. Naming them is the point:
	// a chart that fills them with zero shows a cliff that never happened.
	if !reflect.DeepEqual(res.MissingDays, []string{"2026-07-03", "2026-07-04"}) {
		t.Errorf("missing days = %v", res.MissingDays)
	}
	if res.DataThrough != "2026-07-02" {
		t.Errorf("data through = %q", res.DataThrough)
	}
	if !strings.Contains(res.Note, "2 of 6 days") {
		t.Errorf("note should quantify the gap: %q", res.Note)
	}
	wantColumns := []string{"date", "daily_device_installs", "daily_device_uninstalls", "active_device_installs"}
	if !reflect.DeepEqual(res.Columns, wantColumns) {
		t.Errorf("columns = %v, want %v", res.Columns, wantColumns)
	}
}

func TestRunInstallsUnexportedMonthIsNotAFailure(t *testing.T) {
	bucket := reportsFixture(t)
	delete(bucket, "stats/installs/installs_com.example.app_202606_overview.csv")
	client, _ := newBucketClient(t, bucket)

	res, err := runInstalls(context.Background(), client, DailyReportArgs{Days: 6, End: "2026-07-04"})
	if err != nil {
		t.Fatalf("runInstalls: %v", err)
	}
	if !reflect.DeepEqual(res.MissingMonths, []string{"2026-06"}) {
		t.Errorf("missing months = %v", res.MissingMonths)
	}
	if len(res.Rows) != 2 {
		t.Errorf("rows = %+v, want July's two", res.Rows)
	}
	if len(res.MissingDays) != 4 {
		t.Errorf("missing days = %v, want the whole June half of the window", res.MissingDays)
	}
}

func TestRunRatings(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	res, err := runRatings(context.Background(), client, DailyReportArgs{Days: 2, End: "2026-07-02"})
	if err != nil {
		t.Fatalf("runRatings: %v", err)
	}
	if !reflect.DeepEqual(res.Columns, []string{"date", "daily_average_rating", "total_average_rating"}) {
		t.Errorf("columns = %v", res.Columns)
	}
	if len(res.Rows) != 2 || res.Rows[1]["daily_average_rating"] != "4.6" {
		t.Errorf("rows = %+v", res.Rows)
	}
	if len(res.MissingDays) != 0 {
		t.Errorf("missing days = %v, want none in a fully exported window", res.MissingDays)
	}
}

func TestRunInstallsWithoutABucket(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))
	client.cfg.ReportsBucket = ""
	if _, err := runInstalls(context.Background(), client, DailyReportArgs{}); err == nil {
		t.Error("expected an error with no reports bucket configured")
	}
}

func TestReportToolsExplainARefusedBucket(t *testing.T) {
	// A 403 here has nothing to do with release permissions, and the generic
	// publishing hint would send the user to the wrong checkbox.
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"error":{"code":403,"message":"does not have storage.objects.list access","status":"PERMISSION_DENIED"}}`)
	})
	client := newTestClient(t, api)
	client.cfg.ReportsBucket = testReportsBucket

	_, err := runReportsList(context.Background(), client, ReportsListArgs{})
	if err == nil {
		t.Fatal("expected the refusal to surface")
	}
	if !strings.Contains(err.Error(), "Play Console") {
		t.Errorf("error should name where report access is granted: %v", err)
	}
}

func TestResolveDayWindowDefaults(t *testing.T) {
	start, end, days, err := resolveDayWindow(0, "")
	if err != nil {
		t.Fatalf("resolveDayWindow: %v", err)
	}
	if days != 30 {
		t.Errorf("days = %d, want the 30-day default", days)
	}
	if got := end.Sub(start).Hours() / 24; got != 29 {
		t.Errorf("window spans %v days between its ends, want 29", got)
	}
	if _, _, _, err := resolveDayWindow(7, "2026/07/01"); err == nil {
		t.Error("expected an error for a malformed end date")
	}
}

func TestMonthsInWindow(t *testing.T) {
	start, end, _, err := resolveDayWindow(70, "2026-08-05")
	if err != nil {
		t.Fatalf("resolveDayWindow: %v", err)
	}
	want := []string{"202605", "202606", "202607", "202608"}
	if got := monthsInWindow(start, end); !reflect.DeepEqual(got, want) {
		t.Errorf("months = %v, want %v", got, want)
	}
}

func TestObjectNameParsing(t *testing.T) {
	tests := []struct {
		name             string
		month, dimension string
	}{
		{"installs_com.example.app_202607_overview.csv", "202607", "overview"},
		{"reviews_com.example.app_202607.csv", "202607", ""},
		{"salesreport_202607.zip", "202607", ""},
		{"earnings_202607_1234567890.zip", "202607", "1234567890"},
		{"subscriptions_com.example.app_sku.monthly_202607_country.csv", "202607", "country"},
		{"something_without_a_stamp.csv", "", ""},
	}
	for _, tc := range tests {
		if got := objectMonth(tc.name); got != tc.month {
			t.Errorf("objectMonth(%q) = %q, want %q", tc.name, got, tc.month)
		}
		if got := objectDimension(tc.name); got != tc.dimension {
			t.Errorf("objectDimension(%q) = %q, want %q", tc.name, got, tc.dimension)
		}
	}
}

func TestParseReportMonth(t *testing.T) {
	for _, in := range []string{"2026-07", "202607", " 2026-07 "} {
		got, err := parseReportMonth(in)
		if err != nil || got != "202607" {
			t.Errorf("parseReportMonth(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "2026-13", "26-07", "July"} {
		if _, err := parseReportMonth(in); err == nil {
			t.Errorf("parseReportMonth(%q) should have failed", in)
		}
	}
}

func TestNormalizeBucketName(t *testing.T) {
	tests := map[string]string{
		"pubsite_prod_rev_1234":               "pubsite_prod_rev_1234",
		"gs://pubsite_prod_rev_1234":          "pubsite_prod_rev_1234",
		"gs://pubsite_prod_rev_1234/":         "pubsite_prod_rev_1234",
		" gs://pubsite_prod_rev_1234/stats/ ": "pubsite_prod_rev_1234",
	}
	for in, want := range tests {
		got, err := normalizeBucketName(in)
		if err != nil || got != want {
			t.Errorf("normalizeBucketName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "gs://", "my bucket"} {
		if _, err := normalizeBucketName(in); err == nil {
			t.Errorf("normalizeBucketName(%q) should have failed", in)
		}
	}
}

// TestRunReportFiltersByPackage is the one that matters most: two apps share
// every stats folder, and resolving the wrong file would report another app's
// numbers under this app's name.
func TestRunReportFiltersByPackage(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"stats/installs/installs_com.example.app_202607_overview.csv": utf16LE(julyInstalls),
		"stats/installs/installs_com.other.thing_202607_overview.csv": utf16LE("Date,Package Name,Daily Device Installs\n2026-07-01,com.other.thing,9999\n"),
	})

	res, err := runReport(context.Background(), client, ReportArgs{Kind: "installs", Month: "2026-07"})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if res.Object != "stats/installs/installs_com.example.app_202607_overview.csv" {
		t.Fatalf("object = %q, want the configured app's file", res.Object)
	}

	other, err := runReport(context.Background(), client, ReportArgs{
		PackageName: "com.other.thing", Kind: "installs", Month: "2026-07",
	})
	if err != nil {
		t.Fatalf("runReport for the other app: %v", err)
	}
	if other.Rows[0]["daily_device_installs"] != "9999" {
		t.Errorf("--package did not switch apps: %+v", other.Rows)
	}
}

// TestRunReportUnzipsByExtension: --object can address an archive under a
// folder rollout has no kind for, so the name has to decide, not the table.
func TestRunReportUnzipsByExtension(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"somewhere/else/custom_202607.zip": zipWith(t, map[string][]byte{
			"custom.csv": utf16LE("Date,Amount\n2026-07-01,1.00\n"),
		}),
	})

	res, err := runReport(context.Background(), client, ReportArgs{Object: "somewhere/else/custom_202607.zip"})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if res.ArchiveMember != "custom.csv" || res.RowCount != 1 {
		t.Errorf("result = %+v", res)
	}
}

// TestRunInstallsSurvivesAChangedColumnSet: Play has renamed and dropped these
// columns over the years, and a month whose header no longer matches should
// still come back readable rather than empty.
func TestRunInstallsSurvivesAChangedColumnSet(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"stats/installs/installs_com.example.app_202607_overview.csv": utf16LE(
			"Date,Package Name,Something Play Renamed\n" +
				"2026-07-01,com.example.app,7\n"),
	})

	res, err := runInstalls(context.Background(), client, DailyReportArgs{Days: 1, End: "2026-07-01"})
	if err != nil {
		t.Fatalf("runInstalls: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	// None of the preferred columns except `date` survive, so the report's own
	// header stands in — dropping to an empty column list would render a table
	// with no columns at all.
	want := []string{"date", "package_name", "something_play_renamed"}
	if !reflect.DeepEqual(res.Columns, want) {
		t.Errorf("columns = %v, want the report's own %v", res.Columns, want)
	}
}

// TestRunInstallsSkipsRowsWithoutADate: some months carry a trailing summary
// row. Counting it as a day would double one and shift every missing-day claim.
func TestRunInstallsSkipsRowsWithoutADate(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"stats/installs/installs_com.example.app_202607_overview.csv": utf16LE(
			"Date,Package Name,Daily Device Installs\n" +
				"2026-07-01,com.example.app,120\n" +
				"Total,com.example.app,120\n"),
	})

	res, err := runInstalls(context.Background(), client, DailyReportArgs{Days: 2, End: "2026-07-02"})
	if err != nil {
		t.Fatalf("runInstalls: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["date"] != "2026-07-01" {
		t.Errorf("rows = %+v, want only the dated one", res.Rows)
	}
	if !reflect.DeepEqual(res.MissingDays, []string{"2026-07-02"}) {
		t.Errorf("missing days = %v", res.MissingDays)
	}
}

// TestReportResultsRenderAsTables: the docs promise --format table and --format
// csv on these commands, which needs the rowSource contract to hold.
func TestReportResultsRenderAsTables(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	report, err := runReport(context.Background(), client, ReportArgs{Kind: "ratings", Month: "2026-07"})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	var buf bytes.Buffer
	if err := printResult(&buf, "csv", report); err != nil {
		t.Fatalf("printResult: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "date,package_name,daily_average_rating,total_average_rating\n") {
		t.Errorf("csv header = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "2026-07-02,com.example.app,4.6,4.3") {
		t.Errorf("csv body = %q", buf.String())
	}

	for name, res := range map[string]any{
		"reports list": mustReportsList(t, client),
		"installs":     mustInstalls(t, client),
	} {
		buf.Reset()
		if err := printResult(&buf, "table", res); err != nil {
			t.Errorf("%s: printResult table: %v", name, err)
		}
		if buf.Len() == 0 {
			t.Errorf("%s rendered nothing", name)
		}
	}
}

func mustReportsList(t *testing.T, client *Client) ReportsListResult {
	t.Helper()
	res, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "installs"})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	return res
}

func mustInstalls(t *testing.T, client *Client) DailyReportResult {
	t.Helper()
	res, err := runInstalls(context.Background(), client, DailyReportArgs{Days: 2, End: "2026-07-02"})
	if err != nil {
		t.Fatalf("runInstalls: %v", err)
	}
	return res
}

func TestSetReportsBucketCommand(t *testing.T) {
	clearPlayEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	original := configPath
	configPath = path
	t.Cleanup(func() { configPath = original })

	var out bytes.Buffer
	playSetReportsBucketCmd.SetOut(&out)
	t.Cleanup(func() { playSetReportsBucketCmd.SetOut(nil) })

	// The Console shows a gs:// URI; pasting it should just work.
	if err := playSetReportsBucketCmd.RunE(playSetReportsBucketCmd, []string{"gs://pubsite_prod_rev_1234567890/"}); err != nil {
		t.Fatalf("set-reports-bucket: %v", err)
	}
	cfg, err := loadPlayConfig(path)
	if err != nil {
		t.Fatalf("loadPlayConfig: %v", err)
	}
	if cfg.ReportsBucket != "pubsite_prod_rev_1234567890" {
		t.Errorf("reports bucket = %q", cfg.ReportsBucket)
	}
	// Setting a bucket is what adds the storage scope; without it the report
	// tools would authenticate and then be refused.
	if !slices.Contains(cfg.scopes(), "https://www.googleapis.com/auth/devstorage.read_only") {
		t.Errorf("scopes = %v, want the read-only storage scope", cfg.scopes())
	}

	out.Reset()
	if err := playSetReportsBucketCmd.RunE(playSetReportsBucketCmd, []string{"my-own-bucket"}); err != nil {
		t.Fatalf("set-reports-bucket: %v", err)
	}
	// Accepted, but a name that is not Play's is worth flagging: the likeliest
	// cause is someone naming their own bucket rather than the export one.
	if !strings.Contains(out.String(), "pubsite_prod") {
		t.Errorf("an unexpected bucket name should be flagged: %q", out.String())
	}

	if err := playSetReportsBucketCmd.RunE(playSetReportsBucketCmd, []string{"  "}); err == nil {
		t.Error("expected an error for an empty bucket name")
	}
}

// TestRunReportsListScopesPerAppKinds: the bucket holds every app in the
// developer account. Listing another app's exports under this app's
// package_name is exactly how a caller ends up reading the wrong numbers.
func TestRunReportsListScopesPerAppKinds(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"stats/installs/installs_com.example.app_202607_overview.csv": utf16LE(julyInstalls),
		"stats/installs/installs_com.other.thing_202607_overview.csv": utf16LE(julyInstalls),
		"sales/salesreport_202607.zip":                                zipWith(t, map[string][]byte{"s.csv": utf16LE("A\n1\n")}),
	})

	res, err := runReportsList(context.Background(), client, ReportsListArgs{})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	var names []string
	for _, obj := range res.Objects {
		names = append(names, obj.Name)
	}
	// The financial report is account-wide and stays; the other app's installs
	// file goes.
	want := []string{
		"sales/salesreport_202607.zip",
		"stats/installs/installs_com.example.app_202607_overview.csv",
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, want) {
		t.Errorf("objects = %v, want %v", names, want)
	}
	if !strings.Contains(res.Note, "com.example.app") {
		t.Errorf("note should say the listing is app-scoped: %q", res.Note)
	}

	other, err := runReportsList(context.Background(), client, ReportsListArgs{PackageName: "com.other.thing", Kind: "installs"})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	if len(other.Objects) != 1 || other.Objects[0].Name != "stats/installs/installs_com.other.thing_202607_overview.csv" {
		t.Errorf("--package did not switch apps: %+v", other.Objects)
	}
}

// TestMonthStampIsTheLastOne: a subscription product id can itself look like a
// month (`sku_202401`). Taking the first stamp would label the file with the
// SKU's digits and leave its real month unresolvable.
func TestMonthStampIsTheLastOne(t *testing.T) {
	const name = "financial-stats/subscriptions/subscriptions_com.example.app_sku_202401_202607_country.csv"
	client, _ := newBucketClient(t, fakeBucket{name: utf16LE("Date,Country\n2026-07-01,NL\n")})

	if got := objectMonth("subscriptions_com.example.app_sku_202401_202607_country.csv"); got != "202607" {
		t.Errorf("objectMonth = %q, want the trailing stamp 202607", got)
	}
	// A six-digit run that is not a real month must not stand in for one.
	if got := objectMonth("earnings_999999_1234.zip"); got != "" {
		t.Errorf("objectMonth = %q, want no month for a 99th month", got)
	}

	res, err := runReport(context.Background(), client, ReportArgs{Kind: "subscriptions", Month: "2026-07", Dimension: "country"})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if res.Object != name {
		t.Errorf("object = %q", res.Object)
	}
}

// TestRunReportDoesNotCallAMonthMissingOffAPartialListing: a listing that
// stopped at the page cap is not evidence of absence, and saying "exported
// months are …" off one would be a guess presented as a fact.
func TestRunReportDoesNotCallAMonthMissingOffAPartialListing(t *testing.T) {
	bucket := fakeBucket{}
	// The fake serves one object per page, so more months than maxPages puts
	// the last one past the cutoff.
	month := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var last string
	for i := 0; i <= maxPages; i++ {
		last = month.AddDate(0, i, 0).Format("200601")
		bucket["stats/installs/installs_com.example.app_"+last+"_overview.csv"] = utf16LE(julyInstalls)
	}
	client, _ := newBucketClient(t, bucket)

	_, err := runReport(context.Background(), client, ReportArgs{Kind: "installs", Month: last[:4] + "-" + last[4:]})
	if err == nil {
		t.Fatal("expected the unread tail to surface as an error")
	}
	if !strings.Contains(err.Error(), "cut short") {
		t.Errorf("error should admit the listing was partial, not claim absence: %v", err)
	}
	if strings.Contains(err.Error(), "exported months are") {
		t.Errorf("a partial listing must not present its months as the full set: %v", err)
	}
}

// TestRunInstallsUnionsColumnsAcrossMonths: Play has changed this column set
// between months. A field a later file added is in the rows either way, and
// dropping it from the column list would hide it from every table and CSV.
func TestRunInstallsUnionsColumnsAcrossMonths(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"stats/installs/installs_com.example.app_202606_overview.csv": utf16LE(
			"Date,Package Name,Daily Device Installs\n2026-06-30,com.example.app,110\n"),
		"stats/installs/installs_com.example.app_202607_overview.csv": utf16LE(
			"Date,Package Name,Daily Device Installs,Active Device Installs\n2026-07-01,com.example.app,120,5200\n"),
	})

	res, err := runInstalls(context.Background(), client, DailyReportArgs{Days: 2, End: "2026-07-01"})
	if err != nil {
		t.Fatalf("runInstalls: %v", err)
	}
	want := []string{"date", "daily_device_installs", "active_device_installs"}
	if !reflect.DeepEqual(res.Columns, want) {
		t.Errorf("columns = %v, want the union %v", res.Columns, want)
	}
}

// TestRunReportsListKeepsTheNewestUnderTheCap: capping in the bucket's own
// lexicographic order would return the oldest months and hide the current
// export — the one thing someone runs this command to find.
func TestRunReportsListKeepsTheNewestUnderTheCap(t *testing.T) {
	bucket := fakeBucket{}
	month := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var newest string
	for i := 0; i < 12; i++ {
		newest = month.AddDate(0, i, 0).Format("200601")
		bucket["stats/installs/installs_com.example.app_"+newest+"_overview.csv"] = utf16LE(julyInstalls)
	}
	client, _ := newBucketClient(t, bucket)

	res, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "installs", Max: 2})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	if len(res.Objects) != 2 || !res.Truncated {
		t.Fatalf("objects = %d truncated = %v", len(res.Objects), res.Truncated)
	}
	want := newest[:4] + "-" + newest[4:]
	if res.Objects[0].Month != want {
		t.Errorf("first object month = %q, want the newest %q", res.Objects[0].Month, want)
	}
	if !strings.Contains(res.Note, "newest") {
		t.Errorf("note should say which end was kept: %q", res.Note)
	}
}

// TestReportResolutionMatchesThePackageExactly: underscores are legal in an
// application ID, so `installs_com.foo_bar_…` contains `_com.foo_`. A substring
// test would hand com.foo_bar's numbers back as com.foo's.
func TestReportResolutionMatchesThePackageExactly(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"stats/installs/installs_com.foo_bar_202607_overview.csv": utf16LE(
			"Date,Package Name,Daily Device Installs\n2026-07-01,com.foo_bar,9999\n"),
	})

	// com.foo has no export at all; the neighbour's must not stand in for it.
	_, err := runReport(context.Background(), client, ReportArgs{
		PackageName: "com.foo", Kind: "installs", Month: "2026-07",
	})
	if err == nil {
		t.Fatal("expected no report for com.foo, not com.foo_bar's")
	}
	if !strings.Contains(err.Error(), "no installs report") {
		t.Errorf("error = %v", err)
	}

	res, err := runReportsList(context.Background(), client, ReportsListArgs{PackageName: "com.foo", Kind: "installs"})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	if len(res.Objects) != 0 {
		t.Errorf("listing for com.foo returned com.foo_bar's files: %+v", res.Objects)
	}

	owner, err := runReport(context.Background(), client, ReportArgs{
		PackageName: "com.foo_bar", Kind: "installs", Month: "2026-07",
	})
	if err != nil {
		t.Fatalf("runReport for com.foo_bar: %v", err)
	}
	if owner.Rows[0]["daily_device_installs"] != "9999" {
		t.Errorf("the owning app cannot read its own report: %+v", owner.Rows)
	}
}

// TestAccountWideListingsCarryNoPackage: sales and earnings cover the whole
// developer account, and stamping an app on them invites a consumer to file the
// account's revenue under one product.
func TestAccountWideListingsCarryNoPackage(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	sales, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "sales"})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	if sales.PackageName != "" {
		t.Errorf("package name = %q on an account-wide listing", sales.PackageName)
	}
	if strings.Contains(sales.Note, "scoped to") {
		t.Errorf("note claims an app scope that was not applied: %q", sales.Note)
	}

	installs, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "installs"})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	if installs.PackageName != "com.example.app" {
		t.Errorf("a per-app listing should name the app it is scoped to, got %q", installs.PackageName)
	}
}

// TestRunReportRejectsAnotherAppsData: Play's file names cannot always tell
// `com.foo` from `com.foo_bar`, so the report's own package column is the
// backstop against returning one app's numbers under the other's name.
func TestRunReportRejectsAnotherAppsData(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		// A subscriptions file for com.foo_bar also satisfies com.foo's name
		// pattern, because a product id sits between the package and the month.
		"financial-stats/subscriptions/subscriptions_com.foo_bar_202607_country.csv": utf16LE(
			"Date,Package Name,Country\n2026-07-01,com.foo_bar,NL\n"),
	})

	_, err := runReport(context.Background(), client, ReportArgs{
		PackageName: "com.foo", Kind: "subscriptions", Month: "2026-07", Dimension: "country",
	})
	if err == nil {
		t.Fatal("expected com.foo_bar's subscription data to be refused for com.foo")
	}
	for _, want := range []string{"com.foo_bar", "--object"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}

	// The owning app reads it fine.
	owner, err := runReport(context.Background(), client, ReportArgs{
		PackageName: "com.foo_bar", Kind: "subscriptions", Month: "2026-07", Dimension: "country",
	})
	if err != nil {
		t.Fatalf("runReport for com.foo_bar: %v", err)
	}
	if owner.RowCount != 1 {
		t.Errorf("result = %+v", owner)
	}
}

// TestVerifyReportPackageIsSilentWithoutTheColumn: the check exists to catch a
// wrong file, not to reject a shape Play has changed.
func TestVerifyReportPackageIsSilentWithoutTheColumn(t *testing.T) {
	kind, err := lookupReportKind("installs")
	if err != nil {
		t.Fatalf("lookupReportKind: %v", err)
	}
	table := &reportTable{Columns: []string{"date"}, Rows: []map[string]string{{"date": "2026-07-01"}}}
	if err := verifyReportPackage(table, kind, "com.example.app", "x.csv"); err != nil {
		t.Errorf("a report with no package column should pass: %v", err)
	}
	sales, err := lookupReportKind("sales")
	if err != nil {
		t.Fatalf("lookupReportKind: %v", err)
	}
	// An account-wide report is nobody's app, so it is never checked.
	wrong := &reportTable{Rows: []map[string]string{{"package_name": "com.other"}}}
	if err := verifyReportPackage(wrong, sales, "com.example.app", "x.csv"); err != nil {
		t.Errorf("an account-wide report should not be package-checked: %v", err)
	}
}

// TestRunReportsListAdmitsAnUnreadTail: sorting a partial listing newest-first
// and calling it "the newest objects" would hide the very months the command
// exists to surface.
func TestRunReportsListAdmitsAnUnreadTail(t *testing.T) {
	bucket := fakeBucket{}
	month := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= maxPages; i++ {
		stamp := month.AddDate(0, i, 0).Format("200601")
		bucket["stats/installs/installs_com.example.app_"+stamp+"_overview.csv"] = utf16LE(julyInstalls)
	}
	client, _ := newBucketClient(t, bucket)

	res, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "installs"})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	if !res.Truncated {
		t.Error("a listing that hit the page cap should report truncation")
	}
	if !strings.Contains(res.Note, "page cap") {
		t.Errorf("note should admit the tail was never read: %q", res.Note)
	}
	if strings.Contains(res.Note, "newest objects") {
		t.Errorf("a partial listing must not claim to show the newest: %q", res.Note)
	}
}

// TestRunReportsListFlagsSubscriptionAmbiguity: a subscriptions name puts a
// product id between the package and the month, so a sibling package whose name
// extends this one satisfies the same pattern. The listing cannot tell, and
// presenting the scope as certain would be the misleading part.
func TestRunReportsListFlagsSubscriptionAmbiguity(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	subs, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "subscriptions"})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	if !strings.Contains(subs.Note, "cannot tell two packages apart") {
		t.Errorf("note should flag the ambiguity: %q", subs.Note)
	}

	installs, err := runReportsList(context.Background(), client, ReportsListArgs{Kind: "installs"})
	if err != nil {
		t.Fatalf("runReportsList: %v", err)
	}
	// Installs names the month straight after the package, so there is nothing
	// ambiguous to warn about — a caveat on every listing is noise.
	if strings.Contains(installs.Note, "cannot tell two packages apart") {
		t.Errorf("an unambiguous listing should not carry the caveat: %q", installs.Note)
	}
}

// TestRunReportObjectIgnoresTheDefaultPackage: --object is the way out of an
// ambiguous file name, so resolving the configured default there would make it
// refuse the very read it exists to allow.
func TestRunReportObjectIgnoresTheDefaultPackage(t *testing.T) {
	const name = "financial-stats/subscriptions/subscriptions_com.other.thing_sku_202607_country.csv"
	client, _ := newBucketClient(t, fakeBucket{
		name: utf16LE("Date,Package Name,Country\n2026-07-01,com.other.thing,NL\n"),
	})

	// The client's default app is com.example.app; naming the file wins.
	res, err := runReport(context.Background(), client, ReportArgs{Object: name})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if res.RowCount != 1 {
		t.Fatalf("result = %+v", res)
	}
	// Nothing claims the rows belong to the configured app.
	if res.PackageName != "" {
		t.Errorf("package name = %q, want no claim on a named object", res.PackageName)
	}

	// Passing a package alongside --object is an assertion, and is checked.
	_, err = runReport(context.Background(), client, ReportArgs{Object: name, PackageName: "com.example.app"})
	if err == nil || !strings.Contains(err.Error(), "com.other.thing") {
		t.Errorf("an asserted package should be verified: %v", err)
	}
}

// TestRunReportOutNeverClobbers: this is the only tool that writes to the local
// filesystem, and over MCP the path comes from an agent. Destroying a config or
// a source file because a path was guessed is not a read tool's failure mode.
func TestRunReportOutNeverClobbers(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))
	out := filepath.Join(t.TempDir(), "precious.csv")
	if err := os.WriteFile(out, []byte("do not lose me"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, err := runReport(context.Background(), client, ReportArgs{Kind: "ratings", Month: "2026-07", Out: out})
	if err == nil {
		t.Fatal("expected the existing file to be protected")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should name the way through: %v", err)
	}
	kept, _ := os.ReadFile(out)
	if string(kept) != "do not lose me" {
		t.Errorf("the existing file was modified: %q", kept)
	}

	res, err := runReport(context.Background(), client, ReportArgs{Kind: "ratings", Month: "2026-07", Out: out, Force: true})
	if err != nil {
		t.Fatalf("runReport --force: %v", err)
	}
	written, _ := os.ReadFile(out)
	if string(written) != julyRatings {
		t.Errorf("--force did not replace the file: %q", written)
	}
	if res.OutPath != out {
		t.Errorf("out path = %q", res.OutPath)
	}
}

// TestRunReportObjectDropsANonDimensionSuffix: an earnings archive's trailing
// segment is an account id, and reporting it as a dimension invites a
// --dimension the kind does not have.
func TestRunReportObjectDropsANonDimensionSuffix(t *testing.T) {
	const name = "earnings/earnings_202607_1234567890.zip"
	client, _ := newBucketClient(t, fakeBucket{
		name: zipWith(t, map[string][]byte{"earnings.csv": utf16LE("Description,Amount\nPayout,1.00\n")}),
	})

	res, err := runReport(context.Background(), client, ReportArgs{Object: name})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if res.Dimension != "" {
		t.Errorf("dimension = %q, want none on a kind that has no dimensions", res.Dimension)
	}
	if res.Month != "2026-07" || res.Kind != "earnings" {
		t.Errorf("result = %+v", res)
	}
}

// TestRunInstallsDoesNotCallAnUnreadMonthMissing: a trailing window may absorb
// one resolution failure — this month has no export. An ambiguous match or a
// listing that stopped short is a reason the answer would be wrong, and
// reporting it as "not exported" asserts an absence nobody established.
func TestRunInstallsDoesNotCallAnUnreadMonthMissing(t *testing.T) {
	bucket := fakeBucket{}
	month := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var last string
	for i := 0; i <= maxPages; i++ {
		last = month.AddDate(0, i, 0).Format("200601")
		bucket["stats/installs/installs_com.example.app_"+last+"_overview.csv"] = utf16LE(julyInstalls)
	}
	client, _ := newBucketClient(t, bucket)

	end := last[:4] + "-" + last[4:] + "-15"
	_, err := runInstalls(context.Background(), client, DailyReportArgs{Days: 5, End: end})
	if err == nil {
		t.Fatal("expected the unread tail to surface rather than become a missing month")
	}
	if !strings.Contains(err.Error(), "cut short") {
		t.Errorf("error = %v", err)
	}
}

// TestPackageBoundaryUsesTheReportsOwnMonth: a sibling package can itself end
// in something month-shaped (`com.foo_202401`), and its file would satisfy a
// looser "a month follows the package" test for `com.foo`.
func TestPackageBoundaryUsesTheReportsOwnMonth(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"stats/installs/installs_com.foo_202401_202607_overview.csv": utf16LE(
			"Date,Package Name,Daily Device Installs\n2026-07-01,com.foo_202401,42\n"),
	})

	_, err := runReport(context.Background(), client, ReportArgs{
		PackageName: "com.foo", Kind: "installs", Month: "2026-07",
	})
	if err == nil {
		t.Fatal("com.foo_202401's report was accepted as com.foo's")
	}

	owner, err := runReport(context.Background(), client, ReportArgs{
		PackageName: "com.foo_202401", Kind: "installs", Month: "2026-07",
	})
	if err != nil {
		t.Fatalf("runReport for com.foo_202401: %v", err)
	}
	if owner.Rows[0]["daily_device_installs"] != "42" {
		t.Errorf("the owning app cannot read its own report: %+v", owner.Rows)
	}
}

// TestRunReportWritesOutBeforeParsing: the parser's advice on a malformed
// export is to read it with --out and look. That advice has to be followed
// already by the time it is given.
func TestRunReportWritesOutBeforeParsing(t *testing.T) {
	const malformed = "Date,Amount\n2026-07-01,1.00,SURPRISE\n"
	client, _ := newBucketClient(t, fakeBucket{
		"sales/salesreport_202607.zip": zipWith(t, map[string][]byte{
			"salesreport_202607.csv": utf16LE(malformed),
		}),
	})
	out := filepath.Join(t.TempDir(), "sales.csv")

	_, err := runReport(context.Background(), client, ReportArgs{Kind: "sales", Month: "2026-07", Out: out})
	if err == nil {
		t.Fatal("expected the malformed report to be reported")
	}
	written, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("the file the error tells the user to inspect was never written: %v", readErr)
	}
	// Unzipped and transcoded, so what lands is readable.
	if string(written) != malformed {
		t.Errorf("written file = %q", written)
	}
}

// TestResolveDayWindowIsBounded: the series is stitched from monthly files, so
// days translate directly into downloads — a mistyped --days must not ask for a
// century of them.
func TestResolveDayWindowIsBounded(t *testing.T) {
	if _, _, _, err := resolveDayWindow(maxReportDays, "2026-07-01"); err != nil {
		t.Errorf("the maximum window should be accepted: %v", err)
	}
	_, _, _, err := resolveDayWindow(maxReportDays+1, "2026-07-01")
	if err == nil {
		t.Fatal("expected an over-long window to be refused")
	}
	if !strings.Contains(err.Error(), "reports get") {
		t.Errorf("error should name the way to read a longer span: %v", err)
	}
}

// TestCompletenessNotesReachRowOnlyFormats: --format table and --format csv
// render rows and nothing else, so a capped report or a window with unexported
// days would otherwise look complete.
func TestCompletenessNotesReachRowOnlyFormats(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	report, err := runReport(context.Background(), client, ReportArgs{
		Kind: "installs", Month: "2026-07", Dimension: "country", MaxRows: 1,
	})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if note := report.completeness(); !strings.Contains(note, "1 of 3 rows") {
		t.Errorf("truncated report note = %q", note)
	}

	daily, err := runInstalls(context.Background(), client, DailyReportArgs{Days: 6, End: "2026-07-04"})
	if err != nil {
		t.Fatalf("runInstalls: %v", err)
	}
	note := daily.completeness()
	for _, want := range []string{"2 of 6 days", "2026-07-03", "2026-07-04"} {
		if !strings.Contains(note, want) {
			t.Errorf("daily note should mention %q: %q", want, note)
		}
	}

	// A complete result says nothing — a warning on every run is noise nobody
	// reads by the time it matters.
	full, err := runRatings(context.Background(), client, DailyReportArgs{Days: 2, End: "2026-07-02"})
	if err != nil {
		t.Fatalf("runRatings: %v", err)
	}
	if got := full.completeness(); got != "" {
		t.Errorf("complete result should be quiet, got %q", got)
	}
}

// TestRunReportKeepsValuesVerbatim: a review's text is user-authored and may
// open or close with whitespace; trimming would make the parsed rows disagree
// with the file --out writes.
func TestRunReportKeepsValuesVerbatim(t *testing.T) {
	client, _ := newBucketClient(t, fakeBucket{
		"reviews/reviews_com.example.app_202607.csv": utf16LE(
			"Review Submit Date and Time,Review Text\n" +
				"2026-07-01T10:00:00Z,\"  spaced out  \"\n"),
	})

	res, err := runReport(context.Background(), client, ReportArgs{Kind: "reviews", Month: "2026-07"})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if got := res.Rows[0]["review_text"]; got != "  spaced out  " {
		t.Errorf("review text = %q, want it untouched", got)
	}
}

// TestRunReportDoesNotLeaveAnotherAppsDataOnDisk: a report that turned out to
// be another app's must not be written under the name this call asked for.
func TestRunReportDoesNotLeaveAnotherAppsDataOnDisk(t *testing.T) {
	const name = "financial-stats/subscriptions/subscriptions_com.foo_bar_202607_country.csv"
	client, _ := newBucketClient(t, fakeBucket{
		name: utf16LE("Date,Package Name,Country\n2026-07-01,com.foo_bar,NL\n"),
	})
	out := filepath.Join(t.TempDir(), "subs.csv")

	_, err := runReport(context.Background(), client, ReportArgs{
		PackageName: "com.foo", Kind: "subscriptions", Month: "2026-07", Dimension: "country", Out: out,
	})
	if err == nil {
		t.Fatal("expected the wrong app's report to be refused")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		body, _ := os.ReadFile(out)
		t.Errorf("the refused report was written anyway: %q", body)
	}
}

// TestWriteReportFileIsAtomic: a failed write must leave the destination as it
// was. Half a CSV is worse than none — the next run refuses it as an existing
// file, with nothing usable in it.
func TestWriteReportFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.csv")

	if err := writeReportFile(path, "Date,Amount\n2026-07-01,1.00\n", false); err != nil {
		t.Fatalf("writeReportFile: %v", err)
	}
	// No temporary file is left next to it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "report.csv" {
		t.Errorf("directory holds %v, want only the report", entries)
	}

	if err := writeReportFile(path, "x", false); err == nil {
		t.Error("expected the second write to refuse an existing file")
	}
	if err := writeReportFile(path, "Date,Amount\n2026-07-02,2.00\n", true); err != nil {
		t.Fatalf("writeReportFile --force: %v", err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "2026-07-02") {
		t.Errorf("--force did not replace the file: %q", body)
	}
	// An unwritable directory is an error, not a partial file.
	if err := writeReportFile(filepath.Join(dir, "no-such-dir", "x.csv"), "x", false); err == nil {
		t.Error("expected a write into a missing directory to fail")
	}
}

// TestSubscriptionUniquenessNeedsACompleteListing: one match proves uniqueness
// only against a listing read to the end, and a subscriptions month holds one
// file per product — returning the first would present one product as the app.
func TestSubscriptionUniquenessNeedsACompleteListing(t *testing.T) {
	bucket := fakeBucket{}
	// Enough objects that the listing stops at the page cap, with the wanted
	// month early enough to be read.
	month := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= maxPages; i++ {
		stamp := month.AddDate(0, i, 0).Format("200601")
		bucket["financial-stats/subscriptions/subscriptions_com.example.app_sku_"+stamp+"_country.csv"] =
			utf16LE("Date,Package Name,Country\n2020-01-01,com.example.app,NL\n")
	}
	client, _ := newBucketClient(t, bucket)

	_, err := runReport(context.Background(), client, ReportArgs{
		Kind: "subscriptions", Month: "2020-01", Dimension: "country",
	})
	if err == nil {
		t.Fatal("expected uniqueness to be unprovable off a truncated listing")
	}
	if !strings.Contains(err.Error(), "--object") {
		t.Errorf("error should offer the way out: %v", err)
	}

	// A kind whose package, month and dimension pin exactly one object is not
	// affected — refusing there would be caution with nothing behind it.
	installs := fakeBucket{}
	for i := 0; i <= maxPages; i++ {
		stamp := month.AddDate(0, i, 0).Format("200601")
		installs["stats/installs/installs_com.example.app_"+stamp+"_overview.csv"] = utf16LE(julyInstalls)
	}
	client2, _ := newBucketClient(t, installs)
	if _, err := runReport(context.Background(), client2, ReportArgs{Kind: "installs", Month: "2020-01"}); err != nil {
		t.Errorf("an unambiguous kind should still resolve: %v", err)
	}
}

// TestWriteReportFileDoesNotReplaceAConcurrentWinner: publishing with a rename
// after an existence check would let two runs writing the same new path both
// pass, and the second would silently replace the first's report.
func TestWriteReportFileDoesNotReplaceAConcurrentWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.csv")
	if err := os.WriteFile(path, []byte("first writer"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeReportFile(path, "second writer", false); err == nil {
		t.Fatal("expected the existing report to win")
	}
	body, _ := os.ReadFile(path)
	if string(body) != "first writer" {
		t.Errorf("file = %q, want the first writer's content", body)
	}
	// And no temporary file is left behind by the refusal.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %v, want only the report", entries)
	}
}

// TestWriteReportFileIsOwnerOnly: sales, earnings, subscription and review
// exports are commercial data, and a shared directory is not the place to leave
// them world-readable.
func TestWriteReportFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		force bool
	}{{"created", false}, {"forced", true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".csv")
			if err := writeReportFile(path, "Date,Amount\n2026-07-01,1.00\n", tc.force); err != nil {
				t.Fatalf("writeReportFile: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("mode = %v, want 0600", perm)
			}
		})
	}
}

// TestObjectOutsideAKnownFolderStillHonoursAPackageAssertion: --object can name
// a path outside the folders rollout knows, and a package passed with it is
// still an assertion the caller is entitled to have checked.
func TestObjectOutsideAKnownFolderStillHonoursAPackageAssertion(t *testing.T) {
	const name = "custom/exports/whatever_202607.csv"
	client, _ := newBucketClient(t, fakeBucket{
		name: utf16LE("Date,Package Name,Daily Device Installs\n2026-07-01,com.other.thing,7\n"),
	})

	_, err := runReport(context.Background(), client, ReportArgs{Object: name, PackageName: "com.example.app"})
	if err == nil {
		t.Fatal("expected the asserted package to be checked")
	}
	if !strings.Contains(err.Error(), "com.other.thing") {
		t.Errorf("error should name what the file actually holds: %v", err)
	}

	// Without an assertion the file is read as named.
	res, err := runReport(context.Background(), client, ReportArgs{Object: name})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if res.RowCount != 1 {
		t.Errorf("result = %+v", res)
	}
}

// TestFetchReportTextRefusesAnOversizedObject: these files are read whole, so
// "read whatever arrives" is not a promise a CLI can keep. Past the bound the
// honest answer names a tool built for the job.
func TestFetchReportTextRefusesAnOversizedObject(t *testing.T) {
	client, api := newBucketClient(t, reportsFixture(t))
	huge := storageObject{
		Name: "sales/salesreport_202607.zip",
		Size: fmt.Sprint(maxReportBytes + 1),
	}

	_, _, err := client.fetchReportText(context.Background(), testReportsBucket, huge, true)
	if err == nil {
		t.Fatal("expected an oversized report to be refused")
	}
	if !strings.Contains(err.Error(), "gsutil cp") {
		t.Errorf("error should name a tool that can handle it: %v", err)
	}
	// And refused before the transfer, not after.
	for _, call := range api.seen() {
		if strings.Contains(call.Query, "alt=media") {
			t.Error("the object was downloaded despite being over the bound")
		}
	}
}

// TestRunReportStatsANamedObject: a mistyped --object should 404 before any
// transfer starts, and the size it reports is what the bound is checked against.
func TestRunReportStatsANamedObject(t *testing.T) {
	client, _ := newBucketClient(t, reportsFixture(t))

	_, err := runReport(context.Background(), client, ReportArgs{Object: "stats/installs/typo.csv"})
	if err == nil {
		t.Fatal("expected a missing object to fail")
	}
	if !strings.Contains(err.Error(), "typo.csv") {
		t.Errorf("error should name the object asked for: %v", err)
	}
}

// TestUnparseableReportIsNotWrittenWhenOwnershipWasAsserted: --object plus
// --package asks rollout to establish which app a file belongs to. Unparseable
// data cannot answer that, so the file must not land on disk as if it had.
func TestUnparseableReportIsNotWrittenWhenOwnershipWasAsserted(t *testing.T) {
	const name = "financial-stats/subscriptions/subscriptions_com.other.thing_sku_202607_country.csv"
	client, _ := newBucketClient(t, fakeBucket{
		name: utf16LE("Date,Country\n2026-07-01,NL,SURPRISE\n"),
	})
	dir := t.TempDir()

	asserted := filepath.Join(dir, "asserted.csv")
	_, err := runReport(context.Background(), client, ReportArgs{
		Object: name, PackageName: "com.example.app", Out: asserted,
	})
	if err == nil {
		t.Fatal("expected the unparseable report to fail")
	}
	if !strings.Contains(err.Error(), "not written") {
		t.Errorf("error should say the file was withheld: %v", err)
	}
	if _, statErr := os.Stat(asserted); statErr == nil {
		t.Error("the unverifiable report was written anyway")
	}

	// Without the assertion there is nothing to establish, and the file someone
	// needs to inspect still lands.
	plain := filepath.Join(dir, "plain.csv")
	if _, err := runReport(context.Background(), client, ReportArgs{Object: name, Out: plain}); err == nil {
		t.Fatal("expected the unparseable report to fail")
	}
	if _, statErr := os.Stat(plain); statErr != nil {
		t.Errorf("the file the error tells the user to inspect was not written: %v", statErr)
	}
}

// TestResolveDayWindowUsesTheLocalCalendarDay: "yesterday" means the day before
// the caller's own date. Reading it off UTC would, just after local midnight
// east of Greenwich, hand back the day before that.
func TestResolveDayWindowUsesTheLocalCalendarDay(t *testing.T) {
	// Amsterdam is ahead of UTC year-round, so just after local midnight the
	// two calendars disagree.
	t.Setenv("TZ", "Europe/Amsterdam")
	time.Local = time.FixedZone("CEST", 2*60*60)
	t.Cleanup(func() { time.Local = time.UTC })

	_, end, _, err := resolveDayWindow(7, "")
	if err != nil {
		t.Fatalf("resolveDayWindow: %v", err)
	}
	now := time.Now()
	want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	if !end.Equal(want) {
		t.Errorf("window ends %s, want the local yesterday %s", end.Format(dateLayout), want.Format(dateLayout))
	}
}
