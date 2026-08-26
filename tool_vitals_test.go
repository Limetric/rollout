package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// metricRowJSON builds a row in the Reporting API's own shape: dimensions and
// metrics as parallel arrays of tagged values.
func metricRowJSON(date string, dims map[string]string, metrics map[string]string) string {
	var year, month, day int
	fmt.Sscanf(date, "%d-%d-%d", &year, &month, &day)
	row := fmt.Sprintf(`{"startTime":{"year":%d,"month":%d,"day":%d}`, year, month, day)

	var dimParts []string
	for name, value := range dims {
		dimParts = append(dimParts, fmt.Sprintf(`{"dimension":%q,"stringValue":%q}`, name, value))
	}
	if len(dimParts) > 0 {
		row += `,"dimensions":[` + strings.Join(dimParts, ",") + `]`
	}
	var metricParts []string
	for name, value := range metrics {
		metricParts = append(metricParts, fmt.Sprintf(`{"metric":%q,"decimalValue":{"value":%q}}`, name, value))
	}
	if len(metricParts) > 0 {
		row += `,"metrics":[` + strings.Join(metricParts, ",") + `]`
	}
	return row + "}"
}

// vitalsAPI serves metric-set queries, recording the request bodies.
func vitalsAPI(t *testing.T, rows map[string][]string) *fakePlayAPI {
	t.Helper()
	return newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		for set, body := range rows {
			if strings.Contains(r.URL.Path, set) {
				writeJSON(w, http.StatusOK, `{"rows":[`+strings.Join(body, ",")+`]}`)
				return
			}
		}
		writeJSON(w, http.StatusOK, `{"rows":[]}`)
	})
}

func TestRunVitalsDefaults(t *testing.T) {
	api := vitalsAPI(t, map[string][]string{
		"crashRateMetricSet": {metricRowJSON("2026-08-24", nil, map[string]string{"crashRate": "0.004"})},
	})
	client := newTestClient(t, api)

	res, err := runVitals(context.Background(), client, VitalsArgs{Metric: "crashrate"})
	if err != nil {
		t.Fatalf("runVitals: %v", err)
	}
	if res.MetricSet != "crashRateMetricSet" || res.Period != "DAILY" {
		t.Errorf("unexpected result: %+v", res)
	}
	// The defaults are the user-perceived rate, its weighted form, and the
	// user count that makes a rate mean anything.
	want := []string{"crashRate", "crashRate7dUserWeighted", "distinctUsers"}
	if strings.Join(res.Metrics, ",") != strings.Join(want, ",") {
		t.Errorf("default metrics = %v, want %v", res.Metrics, want)
	}

	var sent metricQuery
	if err := json.Unmarshal([]byte(api.seen()[0].Body), &sent); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if sent.Timeline == nil || sent.Timeline.AggregationPeriod != "DAILY" {
		t.Fatalf("unexpected timeline: %+v", sent.Timeline)
	}
	// Daily vitals are aggregated in Play's own zone; sending another silently
	// shifts every bucket.
	if sent.Timeline.StartTime.TimeZone == nil || sent.Timeline.StartTime.TimeZone.ID != vitalsTimeZone {
		t.Errorf("time zone = %+v, want %s", sent.Timeline.StartTime.TimeZone, vitalsTimeZone)
	}
	// The default window ends yesterday: today's data is partial, and a rate
	// over a few hours reads as a spike.
	yesterday := time.Now().AddDate(0, 0, -1)
	if sent.Timeline.EndTime.Day != yesterday.Day() {
		t.Errorf("end day = %d, want yesterday (%d)", sent.Timeline.EndTime.Day, yesterday.Day())
	}
}

func TestVitalsValidatesNamesLocally(t *testing.T) {
	api := vitalsAPI(t, nil)
	client := newTestClient(t, api)
	ctx := context.Background()

	tests := []struct {
		name    string
		args    VitalsArgs
		wantErr string
	}{
		{"unknown metric set", VitalsArgs{Metric: "crashes"}, "crashrate"},
		{"unknown dimension", VitalsArgs{Metric: "crashrate", Dimensions: []string{"version_code"}}, "versionCode"},
		{"unknown metric", VitalsArgs{Metric: "crashrate", Metrics: []string{"crashCount"}}, "crashRate"},
		{"unknown period", VitalsArgs{Metric: "crashrate", Period: "weekly"}, "daily or hourly"},
		{"bad start date", VitalsArgs{Metric: "crashrate", Start: "24-08-2026"}, "YYYY-MM-DD"},
		{"start after end", VitalsArgs{Metric: "crashrate", Start: "2026-08-20", End: "2026-08-10"}, "is after"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runVitals(ctx, client, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
	// The Reporting API allows 10 queries per second; a typo must not spend one.
	if len(api.calls()) != 0 {
		t.Errorf("a rejected query reached the API: %v", api.calls())
	}
}

func TestVitalsHourlyUsesUTC(t *testing.T) {
	api := vitalsAPI(t, nil)
	client := newTestClient(t, api)

	if _, err := runVitals(context.Background(), client, VitalsArgs{Metric: "crashrate", Period: "hourly"}); err != nil {
		t.Fatalf("runVitals: %v", err)
	}
	var sent metricQuery
	if err := json.Unmarshal([]byte(api.seen()[0].Body), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sent.Timeline.AggregationPeriod != "HOURLY" {
		t.Errorf("period = %q", sent.Timeline.AggregationPeriod)
	}
	if sent.Timeline.StartTime.TimeZone.ID != "UTC" {
		t.Errorf("hourly buckets are UTC, got %q", sent.Timeline.StartTime.TimeZone.ID)
	}
}

// TestResolveWindowCrossesMonthBoundaries: a seven-day window ending on the 3rd
// starts in the previous month, and getting that wrong silently queries the
// wrong days.
func TestResolveWindowCrossesMonthBoundaries(t *testing.T) {
	start, end, err := resolveWindow("2026-02-25", "2026-03-03", 0, "DAILY")
	if err != nil {
		t.Fatalf("resolveWindow: %v", err)
	}
	if start.Year != 2026 || start.Month != 2 || start.Day != 25 {
		t.Errorf("start = %+v", start)
	}
	if end.Month != 3 || end.Day != 3 {
		t.Errorf("end = %+v", end)
	}

	// A window given only by --days walks back across the boundary itself.
	start, end, err = resolveWindow("", "2026-03-03", 7, "DAILY")
	if err != nil {
		t.Fatalf("resolveWindow: %v", err)
	}
	if start.Month != 2 || start.Day != 25 {
		t.Errorf("a 7-day window ending 2026-03-03 should start 2026-02-25, got %+v", start)
	}
	if end.Day != 3 {
		t.Errorf("end = %+v", end)
	}
}

func TestVitalsEmptyResultExplainsItself(t *testing.T) {
	api := vitalsAPI(t, nil)
	client := newTestClient(t, api)

	res, err := runVitals(context.Background(), client, VitalsArgs{Metric: "anrrate"})
	if err != nil {
		t.Fatalf("runVitals: %v", err)
	}
	// The two causes that actually produce an empty result, neither of which
	// the API mentions.
	for _, want := range []string{"lag", "too few users"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("note should explain an empty result (%q): %q", want, res.Note)
		}
	}
}

func TestVitalsFreshness(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":query") {
			writeJSON(w, http.StatusOK, `{"rows":[]}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"name":"apps/com.example.app/crashRateMetricSet","freshnessInfo":{"freshnesses":[{"aggregationPeriod":"DAILY","latestEndTime":{"year":2026,"month":8,"day":24}}]}}`)
	})
	client := newTestClient(t, api)

	res, err := runVitals(context.Background(), client, VitalsArgs{Metric: "crashrate", Freshness: true})
	if err != nil {
		t.Fatalf("runVitals: %v", err)
	}
	if !strings.Contains(string(res.Freshness), "latestEndTime") {
		t.Errorf("freshness was not reported: %s", res.Freshness)
	}
}

func TestVitalsTableFlattensRows(t *testing.T) {
	api := vitalsAPI(t, map[string][]string{
		"crashRateMetricSet": {
			metricRowJSON("2026-08-23", map[string]string{"versionCode": "42"}, map[string]string{"crashRate": "0.004"}),
			metricRowJSON("2026-08-24", map[string]string{"versionCode": "42"}, map[string]string{"crashRate": "0.02"}),
		},
	})
	client := newTestClient(t, api)

	res, err := runVitals(context.Background(), client, VitalsArgs{
		Metric: "crashrate", Dimensions: []string{"versionCode"}, Metrics: []string{"crashRate"},
	})
	if err != nil {
		t.Fatalf("runVitals: %v", err)
	}
	var buf strings.Builder
	if err := printResult(&buf, "table", res); err != nil {
		t.Fatalf("printResult: %v", err)
	}
	// The API returns dimensions and metrics as arrays; a table needs columns.
	for _, want := range []string{"date", "versionCode", "crashRate", "2026-08-24", "0.02"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("table missing %q:\n%s", want, buf.String())
		}
	}
}

func TestVitalsSummaryThresholds(t *testing.T) {
	tests := []struct {
		name       string
		crash, anr string
		wantOK     bool
		wantStatus []string
	}{
		{"healthy", "0.004", "0.001", true, []string{"ok", "ok"}},
		{"crash rate over the threshold", "0.02", "0.001", false, []string{"warn", "ok"}},
		{"ANR rate over the threshold", "0.004", "0.006", false, []string{"ok", "warn"}},
		// The threshold is 1.09%; 1.09% exactly is not over it.
		{"exactly at the threshold", "0.0109", "0.0047", true, []string{"ok", "ok"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := vitalsAPI(t, map[string][]string{
				"crashRateMetricSet": {metricRowJSON("2026-08-24", nil, map[string]string{"crashRate": tc.crash})},
				"anrRateMetricSet":   {metricRowJSON("2026-08-24", nil, map[string]string{"anrRate": tc.anr})},
			})
			client := newTestClient(t, api)

			res, err := runVitalsSummary(context.Background(), client, VitalsSummaryArgs{})
			if err != nil {
				t.Fatalf("runVitalsSummary: %v", err)
			}
			if res.OK != tc.wantOK {
				t.Errorf("ok = %v, want %v (%+v)", res.OK, tc.wantOK, res.Metrics)
			}
			for i, want := range tc.wantStatus {
				if res.Metrics[i].Status != want {
					t.Errorf("%s status = %q, want %q", res.Metrics[i].Metric, res.Metrics[i].Status, want)
				}
			}
		})
	}
}

// TestVitalsSummaryReportsTheWorstDay: an average smooths exactly the spike a
// release decision needs to see.
func TestVitalsSummaryReportsTheWorstDay(t *testing.T) {
	api := vitalsAPI(t, map[string][]string{
		"crashRateMetricSet": {
			metricRowJSON("2026-08-22", nil, map[string]string{"crashRate": "0.001"}),
			metricRowJSON("2026-08-23", nil, map[string]string{"crashRate": "0.05"}),
			metricRowJSON("2026-08-24", nil, map[string]string{"crashRate": "0.001"}),
		},
		"anrRateMetricSet": {metricRowJSON("2026-08-24", nil, map[string]string{"anrRate": "0.001"})},
	})
	client := newTestClient(t, api)

	res, err := runVitalsSummary(context.Background(), client, VitalsSummaryArgs{})
	if err != nil {
		t.Fatalf("runVitalsSummary: %v", err)
	}
	if res.OK {
		t.Error("a spike day should fail the summary")
	}
	if res.Metrics[0].Rate == nil || *res.Metrics[0].Rate != 0.05 {
		t.Errorf("crash rate = %v, want the worst day", res.Metrics[0].Rate)
	}
	if res.Metrics[0].Percent == nil || *res.Metrics[0].Percent != 5 {
		t.Errorf("percent = %v, want 5", res.Metrics[0].Percent)
	}
}

// TestVitalsSummaryUnknownIsNotHealthy: no data is a reason to look, not to
// ship.
func TestVitalsSummaryUnknownIsNotHealthy(t *testing.T) {
	api := vitalsAPI(t, nil)
	client := newTestClient(t, api)

	res, err := runVitalsSummary(context.Background(), client, VitalsSummaryArgs{})
	if err != nil {
		t.Fatalf("runVitalsSummary: %v", err)
	}
	if res.OK {
		t.Fatal("an unknown rate must not report as healthy")
	}
	for _, metric := range res.Metrics {
		if metric.Status != "unknown" {
			t.Errorf("%s status = %q", metric.Metric, metric.Status)
		}
	}
}

func TestVitalsSummaryFiltersByVersion(t *testing.T) {
	api := vitalsAPI(t, map[string][]string{
		"crashRateMetricSet": {metricRowJSON("2026-08-24", nil, map[string]string{"crashRate": "0.001"})},
		"anrRateMetricSet":   {metricRowJSON("2026-08-24", nil, map[string]string{"anrRate": "0.001"})},
	})
	client := newTestClient(t, api)

	if _, err := runVitalsSummary(context.Background(), client, VitalsSummaryArgs{VersionCode: 42}); err != nil {
		t.Fatalf("runVitalsSummary: %v", err)
	}
	var sent metricQuery
	if err := json.Unmarshal([]byte(api.seen()[0].Body), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sent.Filter != "versionCode = 42" {
		t.Errorf("filter = %q", sent.Filter)
	}
}

// TestVitalsPermissionErrorNamesThePermission: a service account with full
// release permissions still cannot read vitals.
func TestVitalsPermissionErrorNamesThePermission(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)
	})
	client := newTestClient(t, api)

	_, err := runVitals(context.Background(), client, VitalsArgs{Metric: "crashrate"})
	if err == nil || !strings.Contains(err.Error(), "View app information") {
		t.Fatalf("err = %v, want one naming the missing permission", err)
	}
}

func TestMetricSetTableIsSelfConsistent(t *testing.T) {
	for _, set := range vitalsMetricSets {
		// A default the set does not accept would fail every call to it.
		if err := validateNames("metric", set.DefaultMetrics, set.Metrics); err != nil {
			t.Errorf("%s: %v", set.Name, err)
		}
		if set.Resource == "" || len(set.DefaultMetrics) == 0 {
			t.Errorf("%s is incompletely described: %+v", set.Name, set)
		}
	}
}
