package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Android vitals. The Reporting API models everything as a metric set queried
// over a timeline with dimensions and metrics, and this exposes that model
// rather than hiding it — an agent that can name a dimension can answer a
// question this tool's author never anticipated.
//
// What is *not* passed through is validation: dimension and metric names are
// checked against the table below before the request goes out, because the
// Reporting API allows 10 queries per second and a typo should not cost one.

// vitalsMetricSet describes one metric set: its resource segment, the metrics
// worth returning by default, and the dimensions it accepts.
type vitalsMetricSet struct {
	// Name is what the user types.
	Name string
	// Resource is the API's path segment.
	Resource string
	// DefaultMetrics are returned when the caller names none. They are the
	// user-perceived rate, its seven-day weighted form, and the user count
	// that makes a rate meaningful.
	DefaultMetrics []string
	// Metrics are every metric the set accepts.
	Metrics []string
	// Description is the tool-facing explanation.
	Description string
}

// standardVitalsDimensions are accepted by every metric set.
var standardVitalsDimensions = []string{
	"apiLevel", "versionCode", "countryCode", "deviceModel", "deviceType",
	"deviceRamBucket", "deviceSocMake", "deviceSocModel", "deviceCpuMake",
	"deviceCpuModel", "deviceGpuMake", "deviceGpuModel", "deviceGpuVersion",
	"deviceVulkanVersion", "deviceGlEsVersion", "deviceScreenSize", "deviceScreenDpi",
}

// vitalsMetricSets is the table every vitals query is validated against.
var vitalsMetricSets = []vitalsMetricSet{
	{
		Name: "crashrate", Resource: "crashRateMetricSet",
		DefaultMetrics: []string{"crashRate", "crashRate7dUserWeighted", "distinctUsers"},
		Metrics:        []string{"crashRate", "crashRate7dUserWeighted", "crashRate28dUserWeighted", "distinctUsers"},
		Description:    "user-perceived crash rate",
	},
	{
		Name: "anrrate", Resource: "anrRateMetricSet",
		DefaultMetrics: []string{"anrRate", "anrRate7dUserWeighted", "distinctUsers"},
		Metrics:        []string{"anrRate", "anrRate7dUserWeighted", "anrRate28dUserWeighted", "distinctUsers"},
		Description:    "user-perceived ANR rate",
	},
	{
		Name: "errors", Resource: "errorCountMetricSet",
		DefaultMetrics: []string{"errorReportCount", "distinctUsers"},
		Metrics:        []string{"errorReportCount", "distinctUsers"},
		Description:    "crash and ANR report counts",
	},
	{
		Name: "excessivewakeuprate", Resource: "excessiveWakeupRateMetricSet",
		DefaultMetrics: []string{"excessiveWakeupRate", "distinctUsers"},
		Metrics:        []string{"excessiveWakeupRate", "excessiveWakeupRate7dUserWeighted", "excessiveWakeupRate28dUserWeighted", "distinctUsers"},
		Description:    "excessive background wakeups",
	},
	{
		Name: "stuckbackgroundwakelockrate", Resource: "stuckBackgroundWakelockRateMetricSet",
		DefaultMetrics: []string{"stuckBgWakelockRate", "distinctUsers"},
		Metrics:        []string{"stuckBgWakelockRate", "stuckBgWakelockRate7dUserWeighted", "stuckBgWakelockRate28dUserWeighted", "distinctUsers"},
		Description:    "stuck background wakelocks",
	},
	{
		Name: "slowrenderingrate", Resource: "slowRenderingRateMetricSet",
		DefaultMetrics: []string{"slowRenderingRate20Fps", "distinctUsers"},
		Metrics:        []string{"slowRenderingRate20Fps", "slowRenderingRate20Fps7dUserWeighted", "slowRenderingRate30Fps", "slowRenderingRate30Fps7dUserWeighted", "distinctUsers"},
		Description:    "slow rendering",
	},
	{
		Name: "slowstartrate", Resource: "slowStartRateMetricSet",
		DefaultMetrics: []string{"slowColdStartRate", "distinctUsers"},
		Metrics:        []string{"slowColdStartRate", "slowWarmStartRate", "slowHotStartRate", "distinctUsers"},
		Description:    "slow app starts",
	},
	{
		Name: "lmkrate", Resource: "lmkRateMetricSet",
		DefaultMetrics: []string{"userPerceivedLmkRate", "distinctUsers"},
		Metrics:        []string{"userPerceivedLmkRate", "userPerceivedLmkRate7dUserWeighted", "lmkRate", "distinctUsers"},
		Description:    "low-memory kills",
	},
}

func lookupMetricSet(name string) (vitalsMetricSet, error) {
	wanted := strings.ToLower(strings.TrimSpace(name))
	for _, set := range vitalsMetricSets {
		if set.Name == wanted {
			return set, nil
		}
	}
	names := make([]string, len(vitalsMetricSets))
	for i, set := range vitalsMetricSets {
		names[i] = set.Name
	}
	return vitalsMetricSet{}, fmt.Errorf("unknown metric %q — expected one of: %s", name, strings.Join(names, ", "))
}

// validateNames checks user-supplied metric or dimension names against the set,
// so a typo fails locally instead of spending one of ten queries per second.
func validateNames(kind string, given, allowed []string) error {
	known := map[string]bool{}
	for _, name := range allowed {
		known[name] = true
	}
	for _, name := range given {
		if !known[name] {
			sorted := append([]string(nil), allowed...)
			sort.Strings(sorted)
			return fmt.Errorf("unknown %s %q — this metric set accepts: %s", kind, name, strings.Join(sorted, ", "))
		}
	}
	return nil
}

// VitalsArgs queries one Android vitals metric set.
type VitalsArgs struct {
	PackageName string   `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Metric      string   `json:"metric" jsonschema:"which metric set to query: crashrate, anrrate, errors, excessivewakeuprate, stuckbackgroundwakelockrate, slowrenderingrate, slowstartrate, or lmkrate"`
	Days        int      `json:"days,omitempty" jsonschema:"how many days back to query, ending yesterday; defaults to 7"`
	Start       string   `json:"start,omitempty" jsonschema:"start date as YYYY-MM-DD; overrides days"`
	End         string   `json:"end,omitempty" jsonschema:"end date as YYYY-MM-DD; overrides days"`
	Period      string   `json:"period,omitempty" jsonschema:"aggregation period: daily (default) or hourly"`
	Dimensions  []string `json:"dimensions,omitempty" jsonschema:"break the results down by these dimensions, for example versionCode or countryCode"`
	Metrics     []string `json:"metrics,omitempty" jsonschema:"which metrics to return; omit for this metric set's defaults"`
	Filter      string   `json:"filter,omitempty" jsonschema:"an AIP-160 filter expression, for example versionCode = 42"`
	Freshness   bool     `json:"freshness,omitempty" jsonschema:"also report how far behind real time this metric set's data is"`
}

// VitalsResult is a metric-set query result.
type VitalsResult struct {
	PackageName string            `json:"package_name"`
	Metric      string            `json:"metric"`
	MetricSet   string            `json:"metric_set"`
	Period      string            `json:"aggregation_period"`
	Start       string            `json:"start"`
	End         string            `json:"end"`
	Metrics     []string          `json:"metrics"`
	Dimensions  []string          `json:"dimensions,omitempty"`
	Rows        []json.RawMessage `json:"rows"`
	Truncated   bool              `json:"truncated,omitempty"`
	// Freshness is the metric set's own report of how current its data is.
	// Vitals lag real time by hours to days, and a number without that context
	// invites gating a rollout on data that predates the release.
	Freshness json.RawMessage `json:"freshness,omitempty"`
	Note      string          `json:"note,omitempty"`
}

func (r VitalsResult) tableRows() ([]json.RawMessage, []string) {
	rows := make([]json.RawMessage, 0, len(r.Rows))
	for _, raw := range r.Rows {
		rows = append(rows, jsonRow(flattenMetricRow(raw)))
	}
	fields := []string{"date"}
	fields = append(fields, r.Dimensions...)
	fields = append(fields, r.Metrics...)
	return rows, fields
}

// flattenMetricRow turns the API's `dimensions[]`/`metrics[]` arrays into a
// flat object, so `--format table` has columns to address.
func flattenMetricRow(raw json.RawMessage) map[string]any {
	var row struct {
		StartTime  *timePoint `json:"startTime"`
		Dimensions []struct {
			Dimension   string `json:"dimension"`
			StringValue string `json:"stringValue"`
			Int64Value  string `json:"int64Value"`
			ValueLabel  string `json:"valueLabel"`
		} `json:"dimensions"`
		Metrics []struct {
			Metric       string `json:"metric"`
			DecimalValue *struct {
				Value string `json:"value"`
			} `json:"decimalValue"`
			Int64Value string `json:"int64Value"`
		} `json:"metrics"`
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &row); err != nil {
		return out
	}
	if row.StartTime != nil {
		out["date"] = formatTimePoint(*row.StartTime)
	}
	for _, d := range row.Dimensions {
		switch {
		case d.ValueLabel != "":
			out[d.Dimension] = d.ValueLabel
		case d.StringValue != "":
			out[d.Dimension] = d.StringValue
		default:
			out[d.Dimension] = d.Int64Value
		}
	}
	for _, m := range row.Metrics {
		if m.DecimalValue != nil {
			out[m.Metric] = m.DecimalValue.Value
			continue
		}
		out[m.Metric] = m.Int64Value
	}
	return out
}

func formatTimePoint(t timePoint) string {
	if t.Hours != 0 {
		return fmt.Sprintf("%04d-%02d-%02d %02d:00", t.Year, t.Month, t.Day, t.Hours)
	}
	return fmt.Sprintf("%04d-%02d-%02d", t.Year, t.Month, t.Day)
}

// vitalsTimeZone is the zone Play aggregates daily vitals in. Sending anything
// else silently shifts every bucket by the offset.
const vitalsTimeZone = "America/Los_Angeles"

// runVitals queries a metric set.
func runVitals(ctx context.Context, c *Client, args VitalsArgs) (VitalsResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return VitalsResult{}, err
	}
	set, err := lookupMetricSet(args.Metric)
	if err != nil {
		return VitalsResult{}, err
	}
	period, err := parseAggregationPeriod(args.Period)
	if err != nil {
		return VitalsResult{}, err
	}
	if err := validateNames("dimension", args.Dimensions, standardVitalsDimensions); err != nil {
		return VitalsResult{}, err
	}
	metrics := args.Metrics
	if len(metrics) == 0 {
		metrics = set.DefaultMetrics
	}
	if err := validateNames("metric", metrics, set.Metrics); err != nil {
		return VitalsResult{}, err
	}
	start, end, err := resolveWindow(args.Start, args.End, args.Days, period)
	if err != nil {
		return VitalsResult{}, err
	}

	query := metricQuery{
		Timeline:   &timelineSpec{AggregationPeriod: period, StartTime: &start, EndTime: &end},
		Dimensions: args.Dimensions,
		Metrics:    metrics,
		Filter:     args.Filter,
	}
	rows, err := c.queryMetricSet(ctx, pkg, set.Resource, query)
	if err != nil {
		return VitalsResult{}, toolError("vitals", reportingPermissionHint(err))
	}

	out := VitalsResult{
		PackageName: pkg, Metric: set.Name, MetricSet: set.Resource, Period: period,
		Start: formatTimePoint(start), End: formatTimePoint(end),
		Metrics: metrics, Dimensions: args.Dimensions,
		Rows: rows.Rows, Truncated: rows.Truncated,
	}
	if len(out.Rows) == 0 {
		// The most common cause by far, and the API says nothing about it.
		out.Note = "no rows — vitals lag real time by up to a few days, and a version with too few users is withheld for privacy"
	}
	if args.Freshness {
		if info, err := c.getMetricSet(ctx, pkg, set.Resource); err == nil {
			out.Freshness = info.FreshnessInfo
		}
	}
	return out, nil
}

func parseAggregationPeriod(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "daily":
		return "DAILY", nil
	case "hourly":
		return "HOURLY", nil
	default:
		return "", fmt.Errorf("unknown period %q — expected daily or hourly", s)
	}
}

// resolveWindow turns --days or --start/--end into the API's date structs.
//
// The default window ends *yesterday*, not today: today's data is always
// partial, and a rate computed over a few hours of a day reads as a spike.
func resolveWindow(startStr, endStr string, days int, period string) (start, end timePoint, err error) {
	zone := vitalsTimeZone
	if period == "HOURLY" {
		// Hourly buckets are UTC; sending the daily zone shifts every one.
		zone = "UTC"
	}
	if days <= 0 {
		days = 7
	}

	endDate := time.Now().AddDate(0, 0, -1)
	if endStr != "" {
		if endDate, err = time.Parse("2006-01-02", endStr); err != nil {
			return start, end, fmt.Errorf("invalid --end %q — use YYYY-MM-DD", endStr)
		}
	}
	startDate := endDate.AddDate(0, 0, -(days - 1))
	if startStr != "" {
		if startDate, err = time.Parse("2006-01-02", startStr); err != nil {
			return start, end, fmt.Errorf("invalid --start %q — use YYYY-MM-DD", startStr)
		}
	}
	if startDate.After(endDate) {
		return start, end, fmt.Errorf("--start %s is after --end %s", startDate.Format("2006-01-02"), endDate.Format("2006-01-02"))
	}
	return newTimePoint(startDate, zone), newTimePoint(endDate, zone), nil
}

// newTimePoint builds the API's DateTime. AddDate normalizes month and year
// rollover, so a seven-day window ending on the 3rd starts in the previous
// month without any arithmetic here.
func newTimePoint(t time.Time, zone string) timePoint {
	point := timePoint{Year: t.Year(), Month: int(t.Month()), Day: t.Day()}
	point.TimeZone = &struct {
		ID string `json:"id"`
	}{ID: zone}
	return point
}

// --- the summary ---

// Play's bad-behaviour thresholds. An app above these is at risk of reduced
// discoverability, which is the thing a release decision actually turns on.
const (
	crashRateThreshold = 0.0109 // 1.09% user-perceived crash rate
	anrRateThreshold   = 0.0047 // 0.47% user-perceived ANR rate
)

// VitalsSummaryArgs is the one-shot health check.
type VitalsSummaryArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Days        int    `json:"days,omitempty" jsonschema:"how many days back to summarize, ending yesterday; defaults to 7"`
	VersionCode int64  `json:"version_code,omitempty" jsonschema:"restrict the summary to one app version code"`
}

// VitalsMetricSummary is one metric measured against Play's threshold.
type VitalsMetricSummary struct {
	Metric    string   `json:"metric"`
	Rate      *float64 `json:"rate,omitempty"`
	Percent   *float64 `json:"percent,omitempty"`
	Threshold float64  `json:"threshold"`
	// Status is ok, warn, or unknown. An agent gating a rollout reads this
	// rather than comparing floats itself.
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// VitalsSummaryResult is the health check.
type VitalsSummaryResult struct {
	PackageName string                `json:"package_name"`
	Days        int                   `json:"days"`
	VersionCode int64                 `json:"version_code,omitempty"`
	Metrics     []VitalsMetricSummary `json:"metrics"`
	// OK is false when any metric is over its threshold, so a caller can gate
	// on one field.
	OK   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
}

func (r VitalsSummaryResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Metrics), []string{"metric", "percent", "threshold", "status", "note"}
}

// runVitalsSummary answers "is this app healthy enough to keep rolling out".
//
// It exists because the raw query answers a different question: it returns a
// timeline of numbers, and turning that into a decision means knowing Play's
// thresholds. Encoding them here means an agent can gate a rollout on one
// boolean instead of inventing a threshold of its own.
func runVitalsSummary(ctx context.Context, c *Client, args VitalsSummaryArgs) (VitalsSummaryResult, error) {
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return VitalsSummaryResult{}, err
	}
	days := args.Days
	if days <= 0 {
		days = 7
	}
	filter := ""
	if args.VersionCode > 0 {
		filter = "versionCode = " + strconv.FormatInt(args.VersionCode, 10)
	}

	out := VitalsSummaryResult{PackageName: pkg, Days: days, VersionCode: args.VersionCode, OK: true}
	for _, check := range []struct {
		metric, name string
		threshold    float64
	}{
		{"crashrate", "crashRate", crashRateThreshold},
		{"anrrate", "anrRate", anrRateThreshold},
	} {
		summary := VitalsMetricSummary{Metric: check.name, Threshold: check.threshold, Status: "unknown"}
		res, err := runVitals(ctx, c, VitalsArgs{
			PackageName: pkg, Metric: check.metric, Days: days,
			Metrics: []string{check.name}, Filter: filter,
		})
		if err != nil {
			return VitalsSummaryResult{}, err
		}
		if rate, ok := worstMetricValue(res.Rows, check.name); ok {
			percent := rate * 100
			summary.Rate, summary.Percent = &rate, &percent
			summary.Status = "ok"
			if rate > check.threshold {
				summary.Status = "warn"
				out.OK = false
			}
		} else {
			// Unknown must not read as healthy: it is the state where a caller
			// should look rather than ship.
			summary.Note = "no data in this window — vitals lag real time, and a version with too few users is withheld for privacy"
			out.OK = false
		}
		out.Metrics = append(out.Metrics, summary)
	}
	out.Note = fmt.Sprintf("Play's bad-behaviour thresholds: crash rate above %.2f%% or ANR rate above %.2f%% risks reduced discoverability. The worst day in the window is reported.",
		crashRateThreshold*100, anrRateThreshold*100)
	return out, nil
}

// worstMetricValue returns the highest value of a metric across the window.
// The worst day is what matters: an average smooths exactly the spike a release
// decision needs to see.
func worstMetricValue(rows []json.RawMessage, metric string) (float64, bool) {
	worst, found := 0.0, false
	for _, raw := range rows {
		flat := flattenMetricRow(raw)
		value, ok := flat[metric].(string)
		if !ok || value == "" {
			continue
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			continue
		}
		if !found || parsed > worst {
			worst, found = parsed, true
		}
	}
	return worst, found
}

// --- CLI front-end ---

var (
	vitalsArgs        VitalsArgs
	vitalsFormat      string
	vitalsSummaryArgs VitalsSummaryArgs
	summaryFormat     string
)

var vitalsCmd = &cobra.Command{
	Use:         "vitals",
	Short:       "Query an Android vitals metric set",
	Annotations: mcpTool("vitals"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, vitalsArgs, vitalsFormat, runVitals)
	},
}

var vitalsSummaryCmd = &cobra.Command{
	Use:         "summary",
	Short:       "Crash and ANR rates against Play's bad-behaviour thresholds",
	Annotations: mcpTool("vitals_summary"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, vitalsSummaryArgs, summaryFormat, runVitalsSummary)
	},
}

func init() {
	addPackageFlag(vitalsCmd, &vitalsArgs.PackageName)
	vitalsCmd.Flags().StringVar(&vitalsArgs.Metric, "metric", "crashrate", "metric set to query")
	vitalsCmd.Flags().IntVar(&vitalsArgs.Days, "days", 7, "days back to query, ending yesterday")
	vitalsCmd.Flags().StringVar(&vitalsArgs.Start, "start", "", "start date (YYYY-MM-DD)")
	vitalsCmd.Flags().StringVar(&vitalsArgs.End, "end", "", "end date (YYYY-MM-DD)")
	vitalsCmd.Flags().StringVar(&vitalsArgs.Period, "period", "daily", "aggregation period: daily or hourly")
	vitalsCmd.Flags().StringArrayVar(&vitalsArgs.Dimensions, "dimension", nil, "break results down by this dimension (repeatable)")
	vitalsCmd.Flags().StringArrayVar(&vitalsArgs.Metrics, "metrics", nil, "metrics to return (default: the metric set's own)")
	vitalsCmd.Flags().StringVar(&vitalsArgs.Filter, "filter", "", "AIP-160 filter, e.g. 'versionCode = 42'")
	vitalsCmd.Flags().BoolVar(&vitalsArgs.Freshness, "freshness", false, "also report how current the data is")
	addFormatFlag(vitalsCmd, &vitalsFormat)

	addPackageFlag(vitalsSummaryCmd, &vitalsSummaryArgs.PackageName)
	vitalsSummaryCmd.Flags().IntVar(&vitalsSummaryArgs.Days, "days", 7, "days back to summarize")
	vitalsSummaryCmd.Flags().Int64Var(&vitalsSummaryArgs.VersionCode, "version-code", 0, "restrict to one version code")
	addFormatFlag(vitalsSummaryCmd, &summaryFormat)

	vitalsCmd.AddCommand(vitalsSummaryCmd)
}
