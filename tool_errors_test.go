package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestRunErrorIssues(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"errorIssues":[
			{"name":"apps/com.example.app/errorIssues/abc","type":"APPLICATION_NOT_RESPONDING",
			 "cause":"java.lang.NullPointerException","location":"MainActivity.onCreate",
			 "errorReportCount":"120","distinctUsers":"88","lastErrorReportTime":"2026-08-24T10:00:00Z",
			 "firstAppVersion":{"versionCode":"41"},"lastAppVersion":{"versionCode":"42"}}
		]}`)
	})
	client := newTestClient(t, api)

	res, err := runErrorIssues(context.Background(), client, ErrorIssuesArgs{Type: "crash", VersionCode: 42})
	if err != nil {
		t.Fatalf("runErrorIssues: %v", err)
	}
	if len(res.Issues) != 1 {
		t.Fatalf("got %+v", res)
	}

	query := api.seen()[0].Query
	// The interval goes as flattened query keys, and the filters are combined.
	for _, want := range []string{"interval.startTime.year", "errorIssueType+%3D+CRASH", "versionCode+%3D+42", "orderBy=distinctUsers+desc"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q missing %q", query, want)
		}
	}

	// The nested issue object is flattened for the table but kept whole in JSON.
	flat := flattenErrorIssue(res.Issues[0])
	if flat["cause"] != "java.lang.NullPointerException" || flat["distinct_users"] != "88" {
		t.Errorf("flattened issue = %+v", flat)
	}
	if flat["first_version_code"] != "41" || flat["last_version_code"] != "42" {
		t.Errorf("version range was lost: %+v", flat)
	}

	var buf strings.Builder
	if err := printResult(&buf, "table", res); err != nil {
		t.Fatalf("printResult: %v", err)
	}
	if !strings.Contains(buf.String(), "MainActivity.onCreate") {
		t.Errorf("table missing the failure location:\n%s", buf.String())
	}
}

func TestErrorIssueTypeValidation(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, `{}`) })
	client := newTestClient(t, api)

	_, err := runErrorIssues(context.Background(), client, ErrorIssuesArgs{Type: "crashes"})
	if err == nil || !strings.Contains(err.Error(), "CRASH") {
		t.Fatalf("err = %v, want one listing the valid types", err)
	}
	if len(api.calls()) != 0 {
		t.Errorf("a rejected type reached the API: %v", api.calls())
	}
}

func TestErrorIssuesEmptyResultExplainsItself(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"errorIssues":[]}`)
	})
	client := newTestClient(t, api)

	res, err := runErrorIssues(context.Background(), client, ErrorIssuesArgs{})
	if err != nil {
		t.Fatalf("runErrorIssues: %v", err)
	}
	if !strings.Contains(res.Note, "privacy") {
		t.Errorf("note should explain an empty result: %q", res.Note)
	}
}

func TestRunErrorReports(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"errorReports":[
			{"name":"apps/com.example.app/errorReports/r1","type":"CRASH","eventTime":"2026-08-24T10:00:00Z",
			 "deviceModel":{"marketingName":"Pixel 8"},"osVersion":{"apiLevel":"34"},
			 "appVersion":{"versionCode":"42"},"reportText":"java.lang.NullPointerException\n\tat ..."}
		]}`)
	})
	client := newTestClient(t, api)

	res, err := runErrorReports(context.Background(), client, ErrorReportsArgs{
		Issue: "apps/com.example.app/errorIssues/abc",
	})
	if err != nil {
		t.Fatalf("runErrorReports: %v", err)
	}
	if len(res.Reports) != 1 {
		t.Fatalf("got %+v", res)
	}
	// The issue is addressed by filter, not by path, and either a bare id or
	// the full resource name is accepted because both are what a caller has.
	if !strings.Contains(api.seen()[0].Query, "errorIssueId") || !strings.Contains(api.seen()[0].Query, "abc") {
		t.Errorf("issue filter = %q", api.seen()[0].Query)
	}

	flat := flattenErrorReport(res.Reports[0])
	if flat["device_model"] != "Pixel 8" || flat["os_version"] != "34" {
		t.Errorf("flattened report = %+v", flat)
	}
	// The stack trace is the reason to call this at all.
	if !strings.Contains(flat["report_text"].(string), "NullPointerException") {
		t.Errorf("the stack trace was dropped: %+v", flat)
	}

	if _, err := runErrorReports(context.Background(), client, ErrorReportsArgs{}); err == nil {
		t.Error("issue should be required")
	}
}

func TestErrorIssueID(t *testing.T) {
	if got := errorIssueID("apps/com.example.app/errorIssues/abc"); got != "abc" {
		t.Errorf("errorIssueID = %q", got)
	}
	if got := errorIssueID("abc"); got != "abc" {
		t.Errorf("a bare id should pass through: %q", got)
	}
}

func TestRunAnomalies(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"anomalies":[{"name":"apps/com.example.app/anomalies/1","metricSet":"crashRateMetricSet"}]}`)
	})
	client := newTestClient(t, api)

	res, err := runAnomalies(context.Background(), client, AnomaliesArgs{Filter: `metricSet = "crashRateMetricSet"`})
	if err != nil {
		t.Fatalf("runAnomalies: %v", err)
	}
	if len(res.Anomalies) != 1 {
		t.Fatalf("got %+v", res)
	}
	if !strings.Contains(api.seen()[0].Query, "filter=") {
		t.Errorf("the filter was dropped: %q", api.seen()[0].Query)
	}
}

// TestAnomaliesEmptyIsNotHealth: an empty list means nothing crossed Play's own
// detection thresholds, which is a different claim from the app being healthy.
func TestAnomaliesEmptyIsNotHealth(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"anomalies":[]}`)
	})
	client := newTestClient(t, api)

	res, err := runAnomalies(context.Background(), client, AnomaliesArgs{})
	if err != nil {
		t.Fatalf("runAnomalies: %v", err)
	}
	if !strings.Contains(res.Note, "vitals_summary") {
		t.Errorf("note should point at the tool that does answer health: %q", res.Note)
	}
}

func TestReportingResultsRenderAsTables(t *testing.T) {
	results := []any{
		VitalsResult{Metrics: []string{"crashRate"}, Rows: nil},
		VitalsSummaryResult{Metrics: []VitalsMetricSummary{{Metric: "crashRate", Status: "ok"}}},
		ErrorIssuesResult{},
		ErrorReportsResult{},
		AnomaliesResult{},
	}
	for _, res := range results {
		for _, format := range []string{"json", "table", "csv"} {
			var buf strings.Builder
			if err := printResult(&buf, format, res); err != nil {
				t.Errorf("%T as %s: %v", res, format, err)
			}
		}
	}
}
