package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// The Play Developer Reporting API v1beta1 — a different service from the
// Publisher API, with its own scope, its own quota (10 queries/second), and its
// own permission requirement in the Console: "View app information (read-only)"
// grants it, and Release Manager alone does not.
//
// Everything here is read-only, so every call retries transient failures.

// reportingAPIPath is the version-carrying prefix for Reporting calls.
const reportingAPIPath = "v1beta1"

// metricQuery is the body of a `<metricSet>:query` call. The field names are
// the API's; a tool builds one from its own typed args.
type metricQuery struct {
	Timeline   *timelineSpec `json:"timelineSpec,omitempty"`
	Dimensions []string      `json:"dimensions,omitempty"`
	Metrics    []string      `json:"metrics,omitempty"`
	// PageSize caps rows per page. The API's own default is small, and a
	// tool that wants a week of hourly data would otherwise page needlessly.
	PageSize  int    `json:"pageSize,omitempty"`
	PageToken string `json:"pageToken,omitempty"`
	// Filter is the API's filter expression (e.g. `versionCode = 42`).
	Filter string `json:"filter,omitempty"`
}

// timelineSpec is the reporting window: an aggregation period plus start and
// end points expressed in that period's units.
type timelineSpec struct {
	AggregationPeriod string     `json:"aggregationPeriod,omitempty"`
	StartTime         *timePoint `json:"startTime,omitempty"`
	EndTime           *timePoint `json:"endTime,omitempty"`
}

// timePoint is the API's date/time shape. Only the fields relevant to the
// chosen aggregation period are sent — an HOURLY query carries hours, a DAILY
// one does not.
type timePoint struct {
	Year     int `json:"year,omitempty"`
	Month    int `json:"month,omitempty"`
	Day      int `json:"day,omitempty"`
	Hours    int `json:"hours,omitempty"`
	TimeZone *struct {
		ID string `json:"id"`
	} `json:"timeZone,omitempty"`
}

// metricRows is a decoded query result: the raw rows as the API returned them,
// so a tool exposes the wire format 1:1 rather than a lossy re-shaping.
type metricRows struct {
	Rows      []json.RawMessage `json:"rows"`
	Truncated bool              `json:"truncated,omitempty"`
}

// reportingURL builds a fully-qualified Reporting API URL.
func (c *Client) reportingURL(path string) string {
	return c.cfg.ReportingBaseURL + "/" + reportingAPIPath + "/" + path
}

// queryMetricSet runs a metric-set query for one app and returns every row,
// following pagination up to maxPages.
//
// set is the metric set's resource segment, e.g. "crashRateMetricSet" or
// "anrRateMetricSet".
func (c *Client) queryMetricSet(ctx context.Context, pkg, set string, query metricQuery) (*metricRows, error) {
	rows := &metricRows{}
	truncated, err := eachPage(func(token string) (string, bool, error) {
		query.PageToken = token
		var page struct {
			pagedResponse
			Rows []json.RawMessage `json:"rows"`
		}
		url := c.reportingURL(fmt.Sprintf("apps/%s/%s:query", pkg, set))
		if err := c.doAt(ctx, url, http.MethodPost, nil, query, &page, retryIdempotent); err != nil {
			return "", false, fmt.Errorf("query %s for %s: %w", set, pkg, err)
		}
		rows.Rows = append(rows.Rows, page.Rows...)
		return page.next(), true, nil
	})
	if err != nil {
		return nil, err
	}
	rows.Truncated = truncated
	return rows, nil
}

// metricSetInfo is a metric set's metadata. freshnessInfo is the part that
// matters to a caller: vitals lag real time by hours to days, and a tool that
// reports a number without saying how stale it is invites a wrong decision.
type metricSetInfo struct {
	Name          string          `json:"name"`
	FreshnessInfo json.RawMessage `json:"freshnessInfo"`
}

// getMetricSet reads a metric set's metadata, including data freshness.
func (c *Client) getMetricSet(ctx context.Context, pkg, set string) (*metricSetInfo, error) {
	var info metricSetInfo
	url := c.reportingURL(fmt.Sprintf("apps/%s/%s", pkg, set))
	if err := c.doAt(ctx, url, http.MethodGet, nil, nil, &info, retryIdempotent); err != nil {
		return nil, fmt.Errorf("read %s metadata for %s: %w", set, pkg, err)
	}
	return &info, nil
}

// reportingApp is one entry of apps:search — the apps this credential can read
// reporting data for.
type reportingApp struct {
	Name        string `json:"name"`
	PackageName string `json:"packageName"`
	DisplayName string `json:"displayName"`
}

// searchApps lists the apps the credential can reach. It is the only listing
// endpoint either API offers: the Publisher API has no "list my apps" call, so
// this is what `play_apps` and the login wizard's app picker are built on.
func (c *Client) searchApps(ctx context.Context) ([]reportingApp, error) {
	var apps []reportingApp
	_, err := eachPage(func(token string) (string, bool, error) {
		query := url.Values{}
		if token != "" {
			query.Set("pageToken", token)
		}
		var page struct {
			pagedResponse
			Apps []reportingApp `json:"apps"`
		}
		if err := c.doAt(ctx, c.reportingURL("apps:search"), http.MethodGet, query, nil, &page, retryIdempotent); err != nil {
			return "", false, fmt.Errorf("list apps: %w", err)
		}
		apps = append(apps, page.Apps...)
		return page.next(), true, nil
	})
	if err != nil {
		return nil, err
	}
	return apps, nil
}

// errorIssueQuery narrows a crash/ANR issue search.
type errorIssueQuery struct {
	// Interval is the reporting window; the API rejects a search without one.
	Interval *reportingInterval
	// Filter is the API's filter expression (e.g. `errorIssueType = CRASH`).
	Filter string
	// OrderBy sorts the results, e.g. "distinctUsers desc".
	OrderBy string
	// PageSize caps rows per page.
	PageSize int
}

// reportingInterval is the half-open window a search covers.
type reportingInterval struct {
	Start *timePoint
	End   *timePoint
}

// searchErrorIssues lists crash and ANR issue clusters.
func (c *Client) searchErrorIssues(ctx context.Context, pkg string, q errorIssueQuery) ([]json.RawMessage, bool, error) {
	var issues []json.RawMessage
	truncated, err := eachPage(func(token string) (string, bool, error) {
		query := errorSearchValues(q, token)
		var page struct {
			pagedResponse
			ErrorIssues []json.RawMessage `json:"errorIssues"`
		}
		url := c.reportingURL(fmt.Sprintf("apps/%s/errorIssues:search", pkg))
		if err := c.doAt(ctx, url, http.MethodGet, query, nil, &page, retryIdempotent); err != nil {
			return "", false, fmt.Errorf("search error issues for %s: %w", pkg, err)
		}
		issues = append(issues, page.ErrorIssues...)
		return page.next(), true, nil
	})
	return issues, truncated, err
}

// errorSearchValues renders an issue search as query parameters. The Reporting
// API takes its interval as flattened `interval.startTime.year`-style keys
// rather than a JSON body, which is why this is spelled out here.
func errorSearchValues(q errorIssueQuery, token string) url.Values {
	v := url.Values{}
	if q.Interval != nil {
		addTimePoint(v, "interval.startTime", q.Interval.Start)
		addTimePoint(v, "interval.endTime", q.Interval.End)
	}
	if q.Filter != "" {
		v.Set("filter", q.Filter)
	}
	if q.OrderBy != "" {
		v.Set("orderBy", q.OrderBy)
	}
	if q.PageSize > 0 {
		v.Set("pageSize", fmt.Sprint(q.PageSize))
	}
	if token != "" {
		v.Set("pageToken", token)
	}
	return v
}

func addTimePoint(v url.Values, prefix string, t *timePoint) {
	if t == nil {
		return
	}
	for suffix, value := range map[string]int{"year": t.Year, "month": t.Month, "day": t.Day, "hours": t.Hours} {
		if value != 0 {
			v.Set(prefix+"."+suffix, fmt.Sprint(value))
		}
	}
	if t.TimeZone != nil && t.TimeZone.ID != "" {
		v.Set(prefix+".timeZone.id", t.TimeZone.ID)
	}
}

// searchErrorReports lists the individual reports behind an issue cluster.
func (c *Client) searchErrorReports(ctx context.Context, pkg string, q errorIssueQuery) ([]json.RawMessage, bool, error) {
	var reports []json.RawMessage
	truncated, err := eachPage(func(token string) (string, bool, error) {
		query := errorSearchValues(q, token)
		var page struct {
			pagedResponse
			ErrorReports []json.RawMessage `json:"errorReports"`
		}
		url := c.reportingURL(fmt.Sprintf("apps/%s/errorReports:search", pkg))
		if err := c.doAt(ctx, url, http.MethodGet, query, nil, &page, retryIdempotent); err != nil {
			return "", false, fmt.Errorf("search error reports for %s: %w", pkg, err)
		}
		reports = append(reports, page.ErrorReports...)
		return page.next(), true, nil
	})
	return reports, truncated, err
}

// listAnomalies lists the metric anomalies Play has detected — the "something
// changed" signal, as opposed to the raw metric values a query returns.
func (c *Client) listAnomalies(ctx context.Context, pkg, filter string) ([]json.RawMessage, bool, error) {
	var anomalies []json.RawMessage
	truncated, err := eachPage(func(token string) (string, bool, error) {
		query := url.Values{}
		if filter != "" {
			query.Set("filter", filter)
		}
		if token != "" {
			query.Set("pageToken", token)
		}
		var page struct {
			pagedResponse
			Anomalies []json.RawMessage `json:"anomalies"`
		}
		url := c.reportingURL(fmt.Sprintf("apps/%s/anomalies", pkg))
		if err := c.doAt(ctx, url, http.MethodGet, query, nil, &page, retryIdempotent); err != nil {
			return "", false, fmt.Errorf("list anomalies for %s: %w", pkg, err)
		}
		anomalies = append(anomalies, page.Anomalies...)
		return page.next(), true, nil
	})
	return anomalies, truncated, err
}

// reportingPermissionHint explains the failure that catches everyone: a
// service account with full release permissions still cannot read vitals.
func reportingPermissionHint(err error) error {
	var apiErr *apiError
	if !errors.As(err, &apiErr) || (apiErr.Status != http.StatusForbidden && apiErr.Status != http.StatusUnauthorized) {
		return err
	}
	return fmt.Errorf("%w — the Reporting API needs \"View app information (read-only)\" in Play Console → Users & permissions; Release Manager alone does not grant it, and the Google Play Developer Reporting API must be enabled in the Cloud project", err)
}
