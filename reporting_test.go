package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestQueryMetricSetFollowsPagination(t *testing.T) {
	pages := []string{
		`{"rows":[{"startTime":{"year":2026,"month":8,"day":1}}],"nextPageToken":"p2"}`,
		`{"rows":[{"startTime":{"year":2026,"month":8,"day":2}}]}`,
	}
	var served int
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		body := pages[min(served, len(pages)-1)]
		served++
		writeJSON(w, http.StatusOK, body)
	})
	client := newTestClient(t, api)

	rows, err := client.queryMetricSet(context.Background(), "com.example.app", "crashRateMetricSet", metricQuery{
		Timeline: &timelineSpec{AggregationPeriod: "DAILY"},
		Metrics:  []string{"crashRate"},
	})
	if err != nil {
		t.Fatalf("queryMetricSet: %v", err)
	}
	if len(rows.Rows) != 2 {
		t.Fatalf("got %d rows, want both pages concatenated", len(rows.Rows))
	}
	if rows.Truncated {
		t.Error("a completed walk must not report truncation")
	}

	seen := api.seen()
	if len(seen) != 2 {
		t.Fatalf("made %d calls, want one per page", len(seen))
	}
	if seen[0].Path != "/v1beta1/apps/com.example.app/crashRateMetricSet:query" {
		t.Errorf("query path = %q", seen[0].Path)
	}
	// The second request has to carry the token, or the walk repeats page one
	// forever.
	var second metricQuery
	if err := json.Unmarshal([]byte(seen[1].Body), &second); err != nil {
		t.Fatalf("decode second request: %v", err)
	}
	if second.PageToken != "p2" {
		t.Errorf("second request pageToken = %q", second.PageToken)
	}
	// The query itself has to survive paging.
	if second.Timeline == nil || second.Timeline.AggregationPeriod != "DAILY" {
		t.Errorf("second request lost the timeline: %+v", second)
	}
}

// TestQueryMetricSetReportsTruncation: presenting a capped walk as complete
// would have someone conclude a crash spike ended when the pages just ran out.
func TestQueryMetricSetReportsTruncation(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"rows":[{}],"nextPageToken":"more"}`)
	})
	client := newTestClient(t, api)

	rows, err := client.queryMetricSet(context.Background(), "com.example.app", "crashRateMetricSet", metricQuery{})
	if err != nil {
		t.Fatalf("queryMetricSet: %v", err)
	}
	if !rows.Truncated {
		t.Error("a capped walk must report truncation")
	}
	if len(rows.Rows) != maxPages {
		t.Errorf("collected %d rows, want the page cap of %d", len(rows.Rows), maxPages)
	}
}

func TestGetMetricSetReadsFreshness(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"name":"apps/com.example.app/crashRateMetricSet","freshnessInfo":{"freshnesses":[{"aggregationPeriod":"DAILY","latestEndTime":{"year":2026,"month":8,"day":24}}]}}`)
	})
	client := newTestClient(t, api)

	info, err := client.getMetricSet(context.Background(), "com.example.app", "crashRateMetricSet")
	if err != nil {
		t.Fatalf("getMetricSet: %v", err)
	}
	// Vitals lag real time by hours to days; a number without its freshness
	// invites a wrong decision, so the raw payload is carried through.
	if !strings.Contains(string(info.FreshnessInfo), "latestEndTime") {
		t.Errorf("freshness was dropped: %s", info.FreshnessInfo)
	}
}

func TestSearchApps(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"apps":[{"name":"apps/com.example.app","packageName":"com.example.app","displayName":"Example"}]}`)
	})
	client := newTestClient(t, api)

	apps, err := client.searchApps(context.Background())
	if err != nil {
		t.Fatalf("searchApps: %v", err)
	}
	if len(apps) != 1 || apps[0].PackageName != "com.example.app" {
		t.Fatalf("got %+v", apps)
	}
	if api.seen()[0].Path != "/v1beta1/apps:search" {
		t.Errorf("path = %q", api.seen()[0].Path)
	}
}

func TestSearchErrorIssuesFlattensTheInterval(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"errorIssues":[{"name":"issue-1","type":"CRASH"}]}`)
	})
	client := newTestClient(t, api)

	issues, truncated, err := client.searchErrorIssues(context.Background(), "com.example.app", errorIssueQuery{
		Interval: &reportingInterval{
			Start: &timePoint{Year: 2026, Month: 8, Day: 1},
			End:   &timePoint{Year: 2026, Month: 8, Day: 8},
		},
		Filter:   `errorIssueType = CRASH`,
		OrderBy:  "distinctUsers desc",
		PageSize: 25,
	})
	if err != nil || truncated {
		t.Fatalf("searchErrorIssues: %v (truncated=%v)", err, truncated)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues", len(issues))
	}

	// The Reporting API takes the interval as flattened query keys, not a JSON
	// body — the single easiest thing to get wrong here.
	query := api.seen()[0].Query
	for _, want := range []string{
		"interval.startTime.year=2026", "interval.startTime.month=8", "interval.startTime.day=1",
		"interval.endTime.day=8", "orderBy=distinctUsers+desc", "pageSize=25",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
}

func TestListAnomalies(t *testing.T) {
	api := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"anomalies":[{"name":"anomalies/1"}]}`)
	})
	client := newTestClient(t, api)

	anomalies, _, err := client.listAnomalies(context.Background(), "com.example.app", `metricSet = "crashRateMetricSet"`)
	if err != nil {
		t.Fatalf("listAnomalies: %v", err)
	}
	if len(anomalies) != 1 {
		t.Fatalf("got %d anomalies", len(anomalies))
	}
	if !strings.Contains(api.seen()[0].Query, "filter=") {
		t.Errorf("filter was dropped: %q", api.seen()[0].Query)
	}
}

// TestReportingPermissionHintExplainsTheRealCause: a service account with full
// release permissions still cannot read vitals, and Google's 403 does not say so.
func TestReportingPermissionHintExplainsTheRealCause(t *testing.T) {
	err := reportingPermissionHint(parseAPIError(http.StatusForbidden,
		[]byte(`{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)))
	if !strings.Contains(err.Error(), "View app information") {
		t.Errorf("hint should name the permission: %v", err)
	}
	if !strings.Contains(err.Error(), "Reporting API") {
		t.Errorf("hint should name the API that must be enabled: %v", err)
	}

	// Anything that is not a permission failure is passed through untouched.
	other := parseAPIError(http.StatusInternalServerError, []byte(`{"error":{"message":"boom"}}`))
	if got := reportingPermissionHint(other); got != other {
		t.Errorf("a non-permission error should be unchanged: %v", got)
	}
}

// TestReportingCallsUseTheReportingBaseURL: the two APIs are separate services,
// and pointing a vitals call at the Publisher host fails with a 404 that says
// nothing useful.
func TestReportingCallsUseTheReportingBaseURL(t *testing.T) {
	clearPlayEnv(t)
	publisher := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a reporting call reached the Publisher API host")
		writeJSON(w, http.StatusOK, `{}`)
	})
	reporting := newFakePlayAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"apps":[]}`)
	})

	cfg := &PlayConfig{PackageName: "com.example.app"}
	cfg.BaseURL = publisher.URL
	cfg.ReportingBaseURL = reporting.URL
	client, err := NewClient(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.searchApps(context.Background()); err != nil {
		t.Fatalf("searchApps: %v", err)
	}
	if len(reporting.seen()) != 1 {
		t.Errorf("the reporting host saw %d calls", len(reporting.seen()))
	}
}
