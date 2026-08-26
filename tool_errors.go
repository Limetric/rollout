package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Crash and ANR issue clusters, the individual reports behind them, and the
// anomalies Play itself flags. All read-only, all on the Reporting API.

// errorIssueTypes are the values the API's errorIssueType filter accepts.
var errorIssueTypes = []string{"CRASH", "ANR", "NON_FATAL"}

// ErrorIssuesArgs lists crash and ANR clusters.
type ErrorIssuesArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Days        int    `json:"days,omitempty" jsonschema:"how many days back to search, ending today; defaults to 7"`
	Type        string `json:"type,omitempty" jsonschema:"restrict to crash, anr, or non_fatal issues"`
	VersionCode int64  `json:"version_code,omitempty" jsonschema:"restrict to one app version code"`
	Max         int    `json:"max,omitempty" jsonschema:"maximum number of issues to return; defaults to 50"`
	OrderBy     string `json:"order_by,omitempty" jsonschema:"sort expression, for example 'distinctUsers desc' (the default) or 'errorReportCount desc'"`
}

// ErrorIssuesResult lists the clusters.
type ErrorIssuesResult struct {
	PackageName string            `json:"package_name"`
	Issues      []json.RawMessage `json:"issues"`
	Truncated   bool              `json:"truncated,omitempty"`
	Note        string            `json:"note,omitempty"`
}

func (r ErrorIssuesResult) tableRows() ([]json.RawMessage, []string) {
	rows := make([]json.RawMessage, 0, len(r.Issues))
	for _, raw := range r.Issues {
		rows = append(rows, jsonRow(flattenErrorIssue(raw)))
	}
	return rows, []string{"name", "type", "distinct_users", "error_report_count", "last_error_report_time", "cause", "location"}
}

// flattenErrorIssue lifts the fields a human scans out of the API's nested
// issue object, without discarding the object itself.
func flattenErrorIssue(raw json.RawMessage) map[string]any {
	var issue struct {
		Name                string `json:"name"`
		Type                string `json:"type"`
		Cause               string `json:"cause"`
		Location            string `json:"location"`
		ErrorReportCount    string `json:"errorReportCount"`
		DistinctUsers       string `json:"distinctUsers"`
		LastErrorReportTime string `json:"lastErrorReportTime"`
		FirstAppVersion     *struct {
			VersionCode string `json:"versionCode"`
		} `json:"firstAppVersion"`
		LastAppVersion *struct {
			VersionCode string `json:"versionCode"`
		} `json:"lastAppVersion"`
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &issue); err != nil {
		return out
	}
	out["name"] = issue.Name
	out["type"] = issue.Type
	out["cause"] = issue.Cause
	out["location"] = issue.Location
	out["error_report_count"] = issue.ErrorReportCount
	out["distinct_users"] = issue.DistinctUsers
	out["last_error_report_time"] = issue.LastErrorReportTime
	if issue.FirstAppVersion != nil {
		out["first_version_code"] = issue.FirstAppVersion.VersionCode
	}
	if issue.LastAppVersion != nil {
		out["last_version_code"] = issue.LastAppVersion.VersionCode
	}
	return out
}

// runErrorIssues searches the crash and ANR clusters.
func runErrorIssues(ctx context.Context, c *Client, args ErrorIssuesArgs) (ErrorIssuesResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ErrorIssuesResult{}, err
	}
	query, err := buildErrorQuery(args.Days, args.Type, args.VersionCode, args.Max, args.OrderBy)
	if err != nil {
		return ErrorIssuesResult{}, err
	}

	issues, truncated, err := c.searchErrorIssues(ctx, pkg, query)
	if err != nil {
		return ErrorIssuesResult{}, toolError("error_issues", reportingPermissionHint(err))
	}
	if len(issues) > query.PageSize && query.PageSize > 0 {
		issues = issues[:query.PageSize]
	}
	out := ErrorIssuesResult{PackageName: pkg, Issues: issues, Truncated: truncated}
	if len(issues) == 0 {
		out.Note = "no issues in this window — crash clustering lags real time, and issues affecting too few users are withheld for privacy"
	}
	return out, nil
}

// buildErrorQuery assembles the interval, filter, and ordering both error
// searches share.
func buildErrorQuery(days int, issueType string, versionCode int64, max int, orderBy string) (errorIssueQuery, error) {
	if days <= 0 {
		days = 7
	}
	if max <= 0 {
		max = 50
	}
	if orderBy == "" {
		// Most-users-affected first is the order a triage decision wants; the
		// API's own default is unspecified.
		orderBy = "distinctUsers desc"
	}

	var filters []string
	if issueType != "" {
		normalized := strings.ToUpper(strings.TrimSpace(issueType))
		if !slicesContainsString(errorIssueTypes, normalized) {
			return errorIssueQuery{}, fmt.Errorf("unknown issue type %q — expected one of: %s", issueType, strings.Join(errorIssueTypes, ", "))
		}
		filters = append(filters, "errorIssueType = "+normalized)
	}
	if versionCode > 0 {
		filters = append(filters, "versionCode = "+strconv.FormatInt(versionCode, 10))
	}

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -days)
	return errorIssueQuery{
		Interval: &reportingInterval{
			Start: &timePoint{Year: start.Year(), Month: int(start.Month()), Day: start.Day()},
			End:   &timePoint{Year: now.Year(), Month: int(now.Month()), Day: now.Day()},
		},
		Filter:   strings.Join(filters, " AND "),
		OrderBy:  orderBy,
		PageSize: max,
	}, nil
}

// --- individual reports ---

// ErrorReportsArgs lists the reports behind one issue.
type ErrorReportsArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Issue       string `json:"issue" jsonschema:"the issue resource name from play_error_issues, for example apps/com.example.app/errorIssues/abc"`
	Days        int    `json:"days,omitempty" jsonschema:"how many days back to search; defaults to 7"`
	Max         int    `json:"max,omitempty" jsonschema:"maximum number of reports to return; defaults to 20"`
}

// ErrorReportsResult lists the reports.
type ErrorReportsResult struct {
	PackageName string            `json:"package_name"`
	Issue       string            `json:"issue"`
	Reports     []json.RawMessage `json:"reports"`
	Truncated   bool              `json:"truncated,omitempty"`
}

func (r ErrorReportsResult) tableRows() ([]json.RawMessage, []string) {
	rows := make([]json.RawMessage, 0, len(r.Reports))
	for _, raw := range r.Reports {
		rows = append(rows, jsonRow(flattenErrorReport(raw)))
	}
	return rows, []string{"name", "type", "event_time", "device_model", "os_version", "version_code"}
}

func flattenErrorReport(raw json.RawMessage) map[string]any {
	var report struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		EventTime   string `json:"eventTime"`
		DeviceModel *struct {
			MarketingName string `json:"marketingName"`
		} `json:"deviceModel"`
		OSVersion *struct {
			APILevel string `json:"apiLevel"`
		} `json:"osVersion"`
		AppVersion *struct {
			VersionCode string `json:"versionCode"`
		} `json:"appVersion"`
		ReportText string `json:"reportText"`
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &report); err != nil {
		return out
	}
	out["name"], out["type"], out["event_time"] = report.Name, report.Type, report.EventTime
	out["report_text"] = report.ReportText
	if report.DeviceModel != nil {
		out["device_model"] = report.DeviceModel.MarketingName
	}
	if report.OSVersion != nil {
		out["os_version"] = report.OSVersion.APILevel
	}
	if report.AppVersion != nil {
		out["version_code"] = report.AppVersion.VersionCode
	}
	return out
}

// runErrorReports lists the individual reports behind an issue cluster, which
// is where the stack traces are.
func runErrorReports(ctx context.Context, c *Client, args ErrorReportsArgs) (ErrorReportsResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return ErrorReportsResult{}, err
	}
	if args.Issue == "" {
		return ErrorReportsResult{}, fmt.Errorf("issue is required — pass --issue with a name from `rollout play errors`")
	}
	max := args.Max
	if max <= 0 {
		max = 20
	}
	query, err := buildErrorQuery(args.Days, "", 0, max, "")
	if err != nil {
		return ErrorReportsResult{}, err
	}
	// The issue name is the filter; there is no path-scoped reports endpoint.
	query.Filter = "errorIssueId = " + strconv.Quote(errorIssueID(args.Issue))
	query.OrderBy = ""

	reports, truncated, err := c.searchErrorReports(ctx, pkg, query)
	if err != nil {
		return ErrorReportsResult{}, toolError("error_reports", reportingPermissionHint(err))
	}
	if len(reports) > max {
		reports = reports[:max]
	}
	return ErrorReportsResult{PackageName: pkg, Issue: args.Issue, Reports: reports, Truncated: truncated}, nil
}

// errorIssueID accepts either a bare id or the full resource name the issue
// search returns, because both are what a caller has in hand.
func errorIssueID(name string) string {
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

// --- anomalies ---

// AnomaliesArgs lists the anomalies Play has detected.
type AnomaliesArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Filter      string `json:"filter,omitempty" jsonschema:"an AIP-160 filter, for example 'activeBetween.startTime.year >= 2026'"`
}

// AnomaliesResult lists them.
type AnomaliesResult struct {
	PackageName string            `json:"package_name"`
	Anomalies   []json.RawMessage `json:"anomalies"`
	Truncated   bool              `json:"truncated,omitempty"`
	Note        string            `json:"note,omitempty"`
}

func (r AnomaliesResult) tableRows() ([]json.RawMessage, []string) {
	return r.Anomalies, []string{"name", "metricSet", "timelineSpec.aggregationPeriod"}
}

// runAnomalies lists detected anomalies — Play's own "something changed"
// signal, as opposed to the raw numbers a vitals query returns.
func runAnomalies(ctx context.Context, c *Client, args AnomaliesArgs) (AnomaliesResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return AnomaliesResult{}, err
	}
	anomalies, truncated, err := c.listAnomalies(ctx, pkg, args.Filter)
	if err != nil {
		return AnomaliesResult{}, toolError("anomalies", reportingPermissionHint(err))
	}
	out := AnomaliesResult{PackageName: pkg, Anomalies: anomalies, Truncated: truncated}
	if len(anomalies) == 0 {
		out.Note = "no anomalies detected — Play flags these itself, so an empty list means nothing crossed its own thresholds, not that vitals are healthy (see play_vitals_summary for that)"
	}
	return out, nil
}

// --- CLI front-end ---

var (
	errorIssuesArgs    ErrorIssuesArgs
	errorIssuesFormat  string
	errorReportsArgs   ErrorReportsArgs
	errorReportsFormat string
	anomaliesArgs      AnomaliesArgs
	anomaliesFormat    string
)

var errorsCmd = &cobra.Command{
	Use:         "errors",
	Short:       "List crash and ANR issue clusters",
	Annotations: mcpTool("error_issues"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, errorIssuesArgs, errorIssuesFormat, runErrorIssues)
	},
}

var errorCmd = &cobra.Command{
	Use:   "error",
	Short: "Inspect a single crash or ANR issue",
}

var errorReportsCmd = &cobra.Command{
	Use:         "reports",
	Short:       "List the individual reports behind an issue",
	Annotations: mcpTool("error_reports"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, errorReportsArgs, errorReportsFormat, runErrorReports)
	},
}

var anomaliesCmd = &cobra.Command{
	Use:         "anomalies",
	Short:       "List the metric anomalies Play has detected",
	Annotations: mcpTool("anomalies"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, anomaliesArgs, anomaliesFormat, runAnomalies)
	},
}

func init() {
	addPackageFlag(errorsCmd, &errorIssuesArgs.PackageName)
	errorsCmd.Flags().IntVar(&errorIssuesArgs.Days, "days", 7, "days back to search")
	errorsCmd.Flags().StringVar(&errorIssuesArgs.Type, "type", "", "crash, anr, or non_fatal")
	errorsCmd.Flags().Int64Var(&errorIssuesArgs.VersionCode, "version-code", 0, "restrict to one version code")
	errorsCmd.Flags().IntVar(&errorIssuesArgs.Max, "max", 50, "maximum issues to return")
	errorsCmd.Flags().StringVar(&errorIssuesArgs.OrderBy, "order-by", "", "sort expression (default: distinctUsers desc)")
	addFormatFlag(errorsCmd, &errorIssuesFormat)

	addPackageFlag(errorReportsCmd, &errorReportsArgs.PackageName)
	errorReportsCmd.Flags().StringVar(&errorReportsArgs.Issue, "issue", "", "issue name from `rollout play errors` (required)")
	errorReportsCmd.Flags().IntVar(&errorReportsArgs.Days, "days", 7, "days back to search")
	errorReportsCmd.Flags().IntVar(&errorReportsArgs.Max, "max", 20, "maximum reports to return")
	addFormatFlag(errorReportsCmd, &errorReportsFormat)

	addPackageFlag(anomaliesCmd, &anomaliesArgs.PackageName)
	anomaliesCmd.Flags().StringVar(&anomaliesArgs.Filter, "filter", "", "AIP-160 filter expression")
	addFormatFlag(anomaliesCmd, &anomaliesFormat)

	errorCmd.AddCommand(errorReportsCmd)
}
