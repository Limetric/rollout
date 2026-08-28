package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The CSV report exports.
//
// Installs, ratings, crash statistics, store performance, reviews and the
// financial reports are not in any Play API. Play writes them as monthly CSVs
// into a Cloud Storage bucket (`pubsite_prod_rev_<developer id>`, the one
// Play Console → Download reports names), and these tools read that bucket.
//
// The four tools share one resolution step, which is why they share a file: an
// object name is *listed*, never constructed. Two of the eight kinds carry
// something rollout cannot know in the file name — a subscription report embeds
// the product id, an earnings archive a per-account suffix — so building the
// name from a template would fail on exactly the reports people most want. A
// listing also lets a miss say which months do exist instead of "404".

// reportKind is one export family: where its objects live, how their names are
// built, and which breakdowns Play publishes for it.
type reportKind struct {
	// Name is what the user types.
	Name string
	// Prefix is the bucket folder, ending in a slash.
	Prefix string
	// FilePrefix begins every object's file name.
	FilePrefix string
	// Dimensions are the breakdowns Play exports for this kind; empty means
	// the kind has exactly one file per month.
	Dimensions []string
	// DefaultDimension is used when the caller names none. It is empty for a
	// kind whose files cannot be told apart without one (subscriptions), where
	// the right answer is to ask rather than to guess.
	DefaultDimension string
	// PerPackage marks a kind whose file name carries the package name. The
	// financial reports cover the whole developer account and do not.
	PerPackage bool
	// Zipped marks a kind delivered as a zip holding the CSV.
	Zipped bool
	// ProductInName marks a kind whose file name carries a product id between
	// the package and the month. Every other per-app kind names the month
	// immediately after the package, which is what makes `com.foo` and
	// `com.foo_bar` tellable apart.
	ProductInName bool
	// Description is the tool-facing explanation.
	Description string
}

// statsDimensions are the breakdowns the app-statistics reports share.
var statsDimensions = []string{"overview", "country", "language", "app_version", "carrier", "device", "os_version"}

// reportKinds is the table every report request is resolved against.
var reportKinds = []reportKind{
	{
		Name: "installs", Prefix: "stats/installs/", FilePrefix: "installs_",
		Dimensions: statsDimensions, DefaultDimension: "overview", PerPackage: true,
		Description: "device and user installs, uninstalls, upgrades, and active devices",
	},
	{
		Name: "ratings", Prefix: "stats/ratings/", FilePrefix: "ratings_",
		Dimensions: statsDimensions, DefaultDimension: "overview", PerPackage: true,
		Description: "daily and cumulative average star rating",
	},
	{
		Name: "crashes", Prefix: "stats/crashes/", FilePrefix: "crashes_",
		Dimensions:       []string{"overview", "app_version", "device", "os_version"},
		DefaultDimension: "overview", PerPackage: true,
		Description: "daily crash and ANR counts (the CSV export, not Android vitals)",
	},
	{
		Name: "store_performance", Prefix: "stats/store_performance/", FilePrefix: "store_performance_",
		Dimensions: []string{"country", "traffic_source"}, DefaultDimension: "country", PerPackage: true,
		Description: "store listing visitors and acquisitions",
	},
	{
		Name: "subscriptions", Prefix: "financial-stats/subscriptions/", FilePrefix: "subscriptions_",
		Dimensions: []string{"country"}, PerPackage: true, ProductInName: true,
		Description: "subscription acquisitions, cancellations and renewals; one file per product id",
	},
	{
		Name: "reviews", Prefix: "reviews/", FilePrefix: "reviews_", PerPackage: true,
		Description: "every review with its text and reply, going back further than the reviews API's one week",
	},
	{
		Name: "sales", Prefix: "sales/", FilePrefix: "salesreport_", Zipped: true,
		Description: "order-level sales for the whole developer account (zipped)",
	},
	{
		Name: "earnings", Prefix: "earnings/", FilePrefix: "earnings_", Zipped: true,
		Description: "payouts and fees for the whole developer account (zipped)",
	},
}

func reportKindNames() []string {
	names := make([]string, len(reportKinds))
	for i, k := range reportKinds {
		names[i] = k.Name
	}
	return names
}

// lookupReportKind resolves a kind name, naming the alternatives on a miss.
func lookupReportKind(name string) (reportKind, error) {
	wanted := strings.ToLower(strings.TrimSpace(name))
	for _, kind := range reportKinds {
		if kind.Name == wanted {
			return kind, nil
		}
	}
	return reportKind{}, fmt.Errorf("unknown report kind %q — expected one of: %s", name, strings.Join(reportKindNames(), ", "))
}

// kindForObject reports which kind an object name belongs to, by longest
// matching folder prefix, so `reports list` can label a bare listing.
func kindForObject(name string) (reportKind, bool) {
	var best reportKind
	var found bool
	for _, kind := range reportKinds {
		if strings.HasPrefix(name, kind.Prefix) && len(kind.Prefix) > len(best.Prefix) {
			best, found = kind, true
		}
	}
	return best, found
}

// resolveDimension picks the breakdown to read and rejects one this kind does
// not publish — a wrong dimension otherwise surfaces as "no such report",
// which reads like the data is missing rather than the argument being wrong.
func (k reportKind) resolveDimension(explicit string) (string, error) {
	dimension := strings.ToLower(strings.TrimSpace(explicit))
	if dimension == "" {
		return k.DefaultDimension, nil
	}
	if len(k.Dimensions) == 0 {
		return "", fmt.Errorf("the %s report has no dimensions — drop --dimension", k.Name)
	}
	for _, d := range k.Dimensions {
		if d == dimension {
			return dimension, nil
		}
	}
	return "", fmt.Errorf("unknown dimension %q for the %s report — expected one of: %s", explicit, k.Name, strings.Join(k.Dimensions, ", "))
}

// --- object naming ---

// parseReportMonth normalizes a month argument to the yyyyMM form the file
// names use. Both `2026-07` and `202607` are accepted.
func parseReportMonth(s string) (string, error) {
	month := strings.TrimSpace(s)
	if month == "" {
		return "", fmt.Errorf("no month — pass --month 2026-07")
	}
	for _, layout := range []string{"2006-01", "200601"} {
		if t, err := time.Parse(layout, month); err == nil {
			return t.Format("200601"), nil
		}
	}
	return "", fmt.Errorf("invalid month %q — use YYYY-MM, e.g. 2026-07", s)
}

// monthStamp finds the yyyyMM stamp an export carries, returning it and the
// offset just past it.
//
// It scans from the right, because the stamp is not the only six-digit run a
// name can hold: a subscription product id (`sku_202401`) or a package segment
// can carry one too, and the report's own month is always the last. The stamp
// must be underscore-bounded and name a real month, so an account id of the
// right length cannot stand in for one.
func monthStamp(base string) (month string, end int) {
	for i := len(base) - 7; i >= 0; i-- {
		if base[i] != '_' {
			continue
		}
		candidate := base[i+1 : i+7]
		if !isSixDigitMonth(candidate) {
			continue
		}
		if stop := i + 7; stop == len(base) || base[stop] == '_' || base[stop] == '.' {
			return candidate, stop
		}
	}
	return "", -1
}

func isSixDigitMonth(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	month := (int(s[4]-'0'))*10 + int(s[5]-'0')
	return month >= 1 && month <= 12
}

// objectMonth reads the yyyyMM stamp out of an object name.
func objectMonth(base string) string {
	month, _ := monthStamp(base)
	return month
}

// objectDimension reads the breakdown suffix out of an object name: the part
// between the month stamp and the extension.
func objectDimension(base string) string {
	_, end := monthStamp(base)
	if end < 0 {
		return ""
	}
	rest := strings.TrimPrefix(base[end:], "_")
	if i := strings.LastIndex(rest, "."); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" || strings.Contains(rest, ".") {
		return ""
	}
	return rest
}

// objectPrefix is the narrowest bucket prefix that still holds every object a
// request could match: the kind's folder, its file prefix, and — for a
// per-app kind — the package.
//
// Narrowing matters beyond politeness. A listing stops at maxPages, and a
// developer account with a few hundred apps and a few years of history can
// exceed that under one report folder; a truncated listing would resolve a
// present month as missing.
func objectPrefix(kind reportKind, pkg string) string {
	prefix := kind.Prefix + kind.FilePrefix
	if kind.PerPackage && pkg != "" {
		prefix += pkg + "_"
	}
	return prefix
}

// objectBelongsToPackage reports whether an object's file name names this app.
//
// Underscores are legal in an application ID, so this cannot be a substring — or
// even a prefix — test alone: `installs_com.foo_bar_202607_overview.csv` starts
// with `installs_com.foo_`, and accepting it would hand com.foo_bar's numbers
// back as com.foo's. What settles it is that the month follows the package
// directly, so the name has to continue with a month stamp and not with the
// rest of a longer package.
//
// Subscriptions are the exception the file name cannot resolve: a product id
// sits in between and may itself contain underscores, so the prefix is as exact
// as it gets there.
func objectBelongsToPackage(base string, kind reportKind, pkg string) bool {
	if !kind.PerPackage || pkg == "" {
		return true
	}
	prefix := kind.FilePrefix + pkg + "_"
	if !strings.HasPrefix(base, prefix) {
		return false
	}
	if kind.ProductInName {
		return true
	}
	// Not merely "a month follows" — *the* month, the one that identifies the
	// report. A sibling package can itself end in something month-shaped
	// (`com.foo_202401`), and its file would satisfy a looser test for
	// `com.foo`; requiring the report's own stamp to start here rules that out.
	_, end := monthStamp(base)
	return end == len(prefix)+6
}

// matchReportObjects narrows a prefix listing to the objects for one month,
// package, and dimension.
func matchReportObjects(objects []storageObject, kind reportKind, pkg, month, dimension string) []storageObject {
	var matched []storageObject
	for _, obj := range objects {
		base := obj.base()
		if !strings.HasPrefix(base, kind.FilePrefix) || objectMonth(base) != month {
			continue
		}
		if !objectBelongsToPackage(base, kind, pkg) {
			continue
		}
		if dimension != "" && objectDimension(base) != dimension {
			continue
		}
		matched = append(matched, obj)
	}
	return matched
}

// availableMonths lists the months a kind has exported for this app, so a miss
// answers the question the user is about to ask next.
func availableMonths(objects []storageObject, kind reportKind, pkg string) []string {
	seen := map[string]bool{}
	for _, obj := range objects {
		base := obj.base()
		if !strings.HasPrefix(base, kind.FilePrefix) || !objectBelongsToPackage(base, kind, pkg) {
			continue
		}
		if month := objectMonth(base); month != "" {
			seen[month] = true
		}
	}
	months := make([]string, 0, len(seen))
	for month := range seen {
		months = append(months, month[:4]+"-"+month[4:])
	}
	sort.Strings(months)
	return months
}

// reportNotExportedError marks the one resolution failure a trailing window can
// absorb: this month has no export, proven against a complete listing. An
// ambiguous match or a listing that stopped short is not this.
type reportNotExportedError struct{ err error }

func (e *reportNotExportedError) Error() string { return e.err.Error() }
func (e *reportNotExportedError) Unwrap() error { return e.err }

// reportListing is a prefix listing plus whether it was complete. The flag
// travels with the objects because every "no such report" answer depends on it:
// off a partial listing, absence is not evidence.
type reportListing struct {
	objects   []storageObject
	truncated bool
}

// resolveReportObject picks the single object a request names, or explains why
// it cannot. Ambiguity is an error rather than a guess: a subscriptions month
// holds one file per product id, and silently reading the first would report
// one product's numbers as the app's.
func resolveReportObject(listing reportListing, kind reportKind, pkg, month, dimension string) (storageObject, error) {
	objects := listing.objects
	matched := matchReportObjects(objects, kind, pkg, month, dimension)
	switch len(matched) {
	case 1:
		// One match proves uniqueness only against a listing that was read to
		// the end. It matters just where a month can legitimately hold several
		// files: for every other kind the package, month and dimension pin
		// exactly one object, but a subscriptions month has one per product,
		// and returning the first would present one product as the whole app.
		if listing.truncated && kind.ProductInName {
			return storageObject{}, fmt.Errorf("the object listing was cut short, so %s cannot be shown to be the only %s report for %s-%s — name the file you want with --object", matched[0].Name, kind.Name, month[:4], month[4:])
		}
		return matched[0], nil
	case 0:
		msg := fmt.Sprintf("no %s report for %s-%s", kind.Name, month[:4], month[4:])
		if kind.PerPackage && pkg != "" {
			msg += " for " + pkg
		}
		if dimension != "" {
			msg += " broken down by " + dimension
		}
		if listing.truncated {
			// Saying "this month was never exported" off a listing that stopped
			// early would be a guess presented as a fact.
			return storageObject{}, fmt.Errorf("%s — but the object listing was cut short, so this may exist and simply not have been read", msg)
		}
		if months := availableMonths(objects, kind, pkg); len(months) > 0 {
			return storageObject{}, &reportNotExportedError{fmt.Errorf("%s — exported months are: %s", msg, strings.Join(months, ", "))}
		}
		return storageObject{}, &reportNotExportedError{fmt.Errorf("%s — nothing under gs://…/%s matches; exports start the month after the app's first install", msg, kind.Prefix)}
	default:
		names := make([]string, len(matched))
		for i, obj := range matched {
			names[i] = obj.Name
		}
		return storageObject{}, fmt.Errorf("%d %s reports match that month — name one with --object: %s", len(matched), kind.Name, strings.Join(names, ", "))
	}
}

// --- shared reading ---

// fetchReportText downloads one object and returns it as UTF-8 CSV text,
// unzipping on the way.
//
// Decoding stops here, before parsing, because `--out` has to be writable even
// when the parse fails — a file rollout cannot read is exactly the one someone
// needs on disk to look at.
func (c *Client) fetchReportText(ctx context.Context, bucket string, obj storageObject, zipped bool) (string, *zipArchive, error) {
	if size := obj.sizeBytes(); size > maxReportBytes {
		return "", nil, fmt.Errorf("%s is %d MB, past the %d MB rollout will read into memory — fetch it with `gsutil cp gs://%s/%s .` and process it outside rollout",
			obj.Name, size>>20, maxReportBytes>>20, bucket, obj.Name)
	}
	data, err := c.downloadStorageObject(ctx, bucket, obj.Name)
	if err != nil {
		return "", nil, err
	}
	var archive *zipArchive
	// The name decides, not the kind: `--object` can address an archive under a
	// kind whose usual export is a plain CSV.
	if zipped || strings.HasSuffix(strings.ToLower(obj.Name), ".zip") {
		archive, err = extractZippedCSV(data)
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", obj.Name, err)
		}
		data = archive.Data
	}
	text, err := decodeReportText(data)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", obj.Name, err)
	}
	return text, archive, nil
}

// readReportObject downloads one object and parses it into rows.
func (c *Client) readReportObject(ctx context.Context, bucket string, obj storageObject, zipped bool, rowLimit int) (*reportTable, string, *zipArchive, error) {
	text, archive, err := c.fetchReportText(ctx, bucket, obj, zipped)
	if err != nil {
		return nil, "", nil, err
	}
	table, err := parseReportCSV(text, rowLimit)
	if err != nil {
		return nil, "", nil, fmt.Errorf("%s: %w", obj.Name, err)
	}
	return table, text, archive, nil
}

// verifyReportPackage checks that the report just read is the app's own.
//
// Play's file names cannot always settle this. A subscriptions file puts the
// product id between the package and the month, and a product id may contain
// underscores — so where a developer owns both `com.foo` and `com.foo_bar`,
// one app's file can satisfy the other's name pattern. The reports carry a
// package column, and reading the answer out of the data beats inferring it
// from the file name.
//
// A report with no such column is left alone: the check exists to catch a
// wrong file, not to reject a shape Play has changed.
func verifyReportPackage(table *reportTable, kind reportKind, pkg, object string) error {
	if pkg == "" {
		return nil
	}
	// A kind rollout recognizes as account-wide is exempt: sales and earnings
	// span every app, so a package there is context, not an assertion. An
	// *unrecognized* object is not exempt — `--object` can name a path outside
	// the known folders, and a package passed with it is still an assertion the
	// caller is entitled to have checked.
	if kind.Name != "" && !kind.PerPackage {
		return nil
	}
	for _, row := range table.Rows {
		got := strings.TrimSpace(row["package_name"])
		if got == "" {
			continue
		}
		if got != pkg {
			return fmt.Errorf("%s holds %s's data, not %s's — Play's file names cannot tell two packages apart when one is a prefix of the other; pick the right file with --object (see `rollout play reports list`)", object, got, pkg)
		}
		return nil
	}
	return nil
}

// reportsLagNote is the fact every one of these tools has to carry: the export
// is a batch job, not a live API, and today's numbers do not exist yet.
const reportsLagNote = "Play writes these CSVs a few days in arrears — expect the last 3–7 days of any window to be missing rather than zero."

// bucketPermissionHint attaches the report-bucket advice to a refused call, so
// a 403 here does not send the user to the release-permission checkbox that
// fixes a publishing 403 and does nothing for this.
func (c *Client) bucketPermissionHint(err error) error {
	return reportsBucketHint(err, c.cfg)
}

// --- play_reports_list ---

// ReportsListArgs lists what the export bucket holds.
type ReportsListArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app. Only the per-app report kinds are filtered by it"`
	Kind        string `json:"kind,omitempty" jsonschema:"narrow to one report kind: installs, ratings, crashes, store_performance, subscriptions, reviews, sales, or earnings; omit to list the whole bucket"`
	Month       string `json:"month,omitempty" jsonschema:"narrow to one month as YYYY-MM, for example 2026-07"`
	Max         int    `json:"max,omitempty" jsonschema:"cap how many objects to return; defaults to 200"`
}

// ReportObject is one exported file.
type ReportObject struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Month     string `json:"month,omitempty"`
	Dimension string `json:"dimension,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Updated   string `json:"updated,omitempty"`
}

// ReportsListResult is the bucket listing.
type ReportsListResult struct {
	Bucket      string         `json:"bucket"`
	Kind        string         `json:"kind,omitempty"`
	Month       string         `json:"month,omitempty"`
	PackageName string         `json:"package_name,omitempty"`
	Objects     []ReportObject `json:"objects"`
	Truncated   bool           `json:"truncated,omitempty"`
	Note        string         `json:"note,omitempty"`
}

func (r ReportsListResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Objects), []string{"kind", "month", "dimension", "size_bytes", "updated", "name"}
}

func (r ReportsListResult) completeness() string {
	if r.Truncated {
		return r.Note
	}
	return ""
}

// defaultReportsListMax bounds a bare listing. A developer account with a few
// apps and a few years of history holds thousands of objects, and an agent that
// asked "what reports are there" does not want all of them.
const defaultReportsListMax = 200

// runReportsList lists the export bucket, optionally narrowed to one kind and
// month.
func runReportsList(ctx context.Context, c *Client, args ReportsListArgs) (ReportsListResult, error) {
	bucket, err := c.reportsBucket()
	if err != nil {
		return ReportsListResult{}, err
	}
	// A package is optional here: the financial kinds are account-wide, and
	// listing the bucket is a reasonable thing to do before setting a default.
	pkg := strings.TrimSpace(args.PackageName)
	if pkg == "" {
		pkg = c.cfg.PackageName
	}

	prefix := ""
	var only reportKind
	if args.Kind != "" {
		if only, err = lookupReportKind(args.Kind); err != nil {
			return ReportsListResult{}, err
		}
		prefix = objectPrefix(only, pkg)
	}
	month := ""
	if args.Month != "" {
		if month, err = parseReportMonth(args.Month); err != nil {
			return ReportsListResult{}, err
		}
	}

	objects, truncated, err := c.listStorageObjects(ctx, bucket, prefix)
	if err != nil {
		return ReportsListResult{}, toolError("reports list", c.bucketPermissionHint(err))
	}

	limit := args.Max
	if limit <= 0 {
		limit = defaultReportsListMax
	}
	out := ReportsListResult{Bucket: bucket, PackageName: pkg, Truncated: truncated, Objects: []ReportObject{}}
	if month != "" {
		// Echo the normalized form, so the filter reads the same way as the
		// months on the objects it selected.
		out.Month = month[:4] + "-" + month[4:]
	}
	if args.Kind != "" {
		out.Kind = only.Name
		if !only.PerPackage {
			// Sales and earnings cover the whole developer account. Stamping an
			// app on a listing of them would invite a consumer to file the
			// account's revenue under one product.
			out.PackageName = ""
		}
	}
	for _, obj := range objects {
		base := obj.base()
		if month != "" && objectMonth(base) != month {
			continue
		}
		kind, known := kindForObject(obj.Name)
		// A bucket holds every app in the developer account. Listing another
		// app's exports under this app's package_name is how a caller ends up
		// reading the wrong numbers, so a per-app kind is scoped to the package
		// whenever there is one; the account-wide financial reports are not.
		if known && !objectBelongsToPackage(base, kind, pkg) {
			continue
		}
		entry := ReportObject{
			Name: obj.Name, Month: objectMonth(base), Dimension: objectDimension(base),
			SizeBytes: obj.sizeBytes(), Updated: obj.Updated,
		}
		if entry.Month != "" {
			entry.Month = entry.Month[:4] + "-" + entry.Month[4:]
		}
		if known {
			entry.Kind = kind.Name
			// A dimension is only meaningful where the kind has some; the
			// trailing segment of a reviews file is not a breakdown.
			if len(kind.Dimensions) == 0 {
				entry.Dimension = ""
			}
		}
		out.Objects = append(out.Objects, entry)
	}

	// Newest month first, and only then the cap. Cloud Storage lists
	// lexicographically, which for these names is oldest-first — capping in
	// that order would drop exactly the months someone ran this to find.
	sort.SliceStable(out.Objects, func(i, j int) bool {
		if out.Objects[i].Month != out.Objects[j].Month {
			return out.Objects[i].Month > out.Objects[j].Month
		}
		return out.Objects[i].Name < out.Objects[j].Name
	})
	if len(out.Objects) > limit {
		out.Objects = out.Objects[:limit]
		out.Truncated = true
	}

	scope := ""
	if out.PackageName != "" {
		scope = fmt.Sprintf("Per-app reports are scoped to %s; sales and earnings cover the whole developer account. ", out.PackageName)
		// A subscriptions name puts a product id between the package and the
		// month, so a sibling package whose name extends this one satisfies the
		// same pattern. The listing cannot tell; say so rather than present the
		// scope as certain. Reading one is still safe — play_report checks the
		// package column in the data.
		if listingHasKind(out.Objects, "subscriptions") {
			scope += "Subscription file names cannot tell two packages apart when one name extends the other, so a sibling app's file may be listed here; reading one verifies the package from the data. "
		}
	}
	switch {
	case truncated:
		// The listing itself stopped at the page cap, so "newest" is only the
		// newest of what was read — claiming otherwise would hide exactly the
		// current months this command exists to surface.
		out.Note = scope + "The bucket listing stopped at the page cap, so objects past it were never read and newer months may exist — narrow with --kind. " + reportsLagNote
	case len(out.Objects) == 0:
		out.Note = scope + "Nothing matched — check the bucket name in `rollout config show`, and note that a report kind appears only once Play has exported a month of it. " + reportsLagNote
	case out.Truncated:
		out.Note = fmt.Sprintf("%sShowing the %d newest objects — narrow with --kind or --month, or raise --max. %s", scope, len(out.Objects), reportsLagNote)
	default:
		out.Note = scope + reportsLagNote
	}
	return out, nil
}

// --- play_report ---

// listingHasKind reports whether a listing holds any object of one kind.
func listingHasKind(objects []ReportObject, kind string) bool {
	for _, obj := range objects {
		if obj.Kind == kind {
			return true
		}
	}
	return false
}

// defaultReportMaxRows bounds the rows a single report returns. A month of
// installs broken down by country is thousands of rows; returning all of them
// to an MCP host spends its whole context on a CSV it could have read from
// disk. The cap is reported, and --out always writes the complete file.
const defaultReportMaxRows = 2000

// ReportArgs downloads and parses one exported report.
type ReportArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app. When object names a file outright there is no default: the named file is read as-is, and passing package_name then asserts which app it should belong to"`
	Kind        string `json:"kind,omitempty" jsonschema:"which report to read: installs, ratings, crashes, store_performance, subscriptions, reviews, sales, or earnings. Required unless object is given"`
	Month       string `json:"month,omitempty" jsonschema:"the month as YYYY-MM, for example 2026-07. Required unless object is given"`
	Dimension   string `json:"dimension,omitempty" jsonschema:"the breakdown to read, for example overview, country, device, os_version, app_version, carrier, or language; defaults to the kind's own"`
	Object      string `json:"object,omitempty" jsonschema:"read this exact object path from the bucket instead of resolving one from kind and month; use the names play_reports_list returns"`
	// Out and Force are CLI-only — `json:"-"` keeps them out of the MCP input
	// schema.
	//
	// Everything else here reads; these write. Serving them over MCP would give
	// an otherwise read-only tool the power to create a file at any path the
	// server can write to, on an argument the model chose, with no preview and
	// no confirm token — which is precisely what safety.go exists to prevent for
	// every other write rollout performs.
	//
	// Nothing is lost by it. The agent skill drives the CLI through a shell,
	// where `--out` is available and the file lands where the user's own command
	// said; an MCP host that wants a report on disk can save the tool's result
	// itself, through whatever file access it already grants.
	Out     string `json:"-"`
	Force   bool   `json:"-"`
	MaxRows int    `json:"max_rows,omitempty" jsonschema:"cap how many parsed rows to return; defaults to 2000"`
}

// ReportResult is one parsed report.
type ReportResult struct {
	Bucket      string `json:"bucket"`
	Object      string `json:"object"`
	Kind        string `json:"kind,omitempty"`
	Month       string `json:"month,omitempty"`
	Dimension   string `json:"dimension,omitempty"`
	PackageName string `json:"package_name,omitempty"`
	// ArchiveMember names the CSV read out of a zipped financial report, and
	// ArchiveMembers everything the archive held — an account whose earnings
	// archive holds several files should not have one silently chosen for it.
	ArchiveMember  string              `json:"archive_member,omitempty"`
	ArchiveMembers []string            `json:"archive_members,omitempty"`
	Columns        []string            `json:"columns"`
	Rows           []map[string]string `json:"rows"`
	RowCount       int                 `json:"row_count"`
	Truncated      bool                `json:"truncated,omitempty"`
	OutPath        string              `json:"out_path,omitempty"`
	Note           string              `json:"note,omitempty"`
}

func (r ReportResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Rows), r.Columns
}

func (r ReportResult) completeness() string {
	if r.Truncated {
		return fmt.Sprintf("showing %d of %d rows — raise --max-rows, or write the whole file with --out", len(r.Rows), r.RowCount)
	}
	return ""
}

// runReport downloads one exported report and parses it.
func runReport(ctx context.Context, c *Client, args ReportArgs) (ReportResult, error) {
	bucket, err := c.reportsBucket()
	if err != nil {
		return ReportResult{}, err
	}

	object, kind, month, dimension, pkg, err := c.locateReport(ctx, bucket, args)
	if err != nil {
		return ReportResult{}, err
	}

	text, archive, err := c.fetchReportText(ctx, bucket, object, kind.Zipped)
	if err != nil {
		return ReportResult{}, toolError("report", c.bucketPermissionHint(err))
	}

	limit := args.MaxRows
	if limit <= 0 {
		limit = defaultReportMaxRows
	}
	table, err := parseReportCSV(text, limit)
	if err != nil {
		// A file rollout cannot read is exactly the one someone needs on disk to
		// look at, and the parser's own advice is to fetch it with --out. So
		// this failure alone still writes it: normally nothing about the file is
		// in doubt except rollout's ability to parse it, because the object was
		// matched to the app by name.
		//
		// The exception is an object named outright *and* asserted to belong to
		// an app. That assertion is checked against the data, and unparseable
		// data cannot answer it — writing the file would commit a report whose
		// ownership the command was specifically asked to establish.
		asserted := strings.TrimSpace(args.Object) != "" && pkg != ""
		switch {
		case args.Out != "" && asserted:
			return ReportResult{}, toolError("report", fmt.Errorf("%s could not be parsed, so it cannot be shown to be %s's — not written to %s; drop --package to fetch it anyway: %w", object.Name, pkg, args.Out, err))
		case args.Out != "":
			if writeErr := writeReportFile(args.Out, text, args.Force); writeErr != nil {
				return ReportResult{}, writeErr
			}
		}
		return ReportResult{}, toolError("report", fmt.Errorf("%s: %w", object.Name, err))
	}
	if err := verifyReportPackage(table, kind, pkg, object.Name); err != nil {
		// Nothing is written here. A report that turned out to be another app's
		// must not be left on disk under the name this call was asked for.
		return ReportResult{}, toolError("report", err)
	}
	outPath := ""
	if args.Out != "" {
		if err := writeReportFile(args.Out, text, args.Force); err != nil {
			return ReportResult{}, err
		}
		outPath = args.Out
	}

	out := ReportResult{
		Bucket: bucket, Object: object.Name, Kind: kind.Name, Dimension: dimension,
		PackageName: pkg, Columns: table.Columns, Rows: table.Rows, RowCount: table.Total,
		OutPath: outPath,
	}
	if month != "" {
		out.Month = month[:4] + "-" + month[4:]
	}
	if !kind.PerPackage {
		// A financial report covers the whole developer account; naming an app
		// on it would suggest a filter that was never applied.
		out.PackageName = ""
	}
	if archive != nil {
		out.ArchiveMember, out.ArchiveMembers = archive.Member, archive.Members
	}

	if out.RowCount > len(out.Rows) {
		out.Truncated = true
		out.Note = fmt.Sprintf("showing %d of %d rows — raise max_rows, or narrow the dimension. ", len(out.Rows), out.RowCount)
	}
	out.Note += reportsLagNote
	return out, nil
}

// reportFileMode is owner-only. A sales, earnings, subscription or review
// export is commercial data, and dropping it into a shared directory
// world-readable is not a default a report reader should pick for its user.
const reportFileMode = 0o600

// writeReportFile saves a report next to the caller's other files.
//
// It refuses to replace one that already exists. This is the only tool that
// writes to the local filesystem, and it is a *read* tool — an out path is
// wherever the caller said, and over MCP that caller is an agent. Destroying a
// config file or a source file because a path was guessed is not a failure mode
// a report reader should have. --force lifts that for the CI job that means it,
// and is CLI-only for the same reason (see ReportArgs.Force).
func writeReportFile(path, text string, force bool) error {
	if force {
		// Write-then-rename, so a full disk leaves the destination as it was
		// rather than half a CSV — which the next run would then refuse as an
		// existing file, with nothing usable in it.
		if err := writeFileAtomic(path, []byte(text), reportFileMode); err != nil {
			return fmt.Errorf("write report to %q: %w", path, err)
		}
		return nil
	}
	// Same write-then-publish, but published with a hard link rather than a
	// rename: rename replaces, link refuses to. Checking with Lstat first and
	// renaming after would let two runs writing the same new path both pass the
	// check, and the second would silently replace the first's report.
	target := resolveWritePath(path)
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write report to %q: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if err := writeTempReport(tmp, text); err != nil {
		return fmt.Errorf("write report to %q: %w", path, err)
	}
	if err := os.Link(tmp.Name(), target); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists — choose another path, or pass --force to replace it", path)
		}
		return fmt.Errorf("write report to %q: %w", path, err)
	}
	return nil
}

func writeTempReport(tmp *os.File, text string) error {
	if err := tmp.Chmod(reportFileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	return tmp.Close()
}

// locateReport resolves the request to a single object, either the one named
// outright or the one a kind, month and dimension select.
func (c *Client) locateReport(ctx context.Context, bucket string, args ReportArgs) (obj storageObject, kind reportKind, month, dimension, pkg string, err error) {
	if name := strings.TrimSpace(args.Object); name != "" {
		// A named object is read as named — the configured default app is not
		// applied here on purpose. --object is the way out of an ambiguous file
		// name, including the sibling-package case verifyReportPackage refuses,
		// and resolving a default would make it refuse the very read it exists
		// to allow. A package passed alongside it is an assertion, and is still
		// checked.
		kind, _ = kindForObject(name)
		base := name
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		dimension := objectDimension(base)
		if len(kind.Dimensions) == 0 {
			// The trailing segment of an earnings archive is an account id, not
			// a breakdown; reporting it as one invites a --dimension the kind
			// does not have.
			dimension = ""
		}
		// Stat it rather than trusting the name: a typo becomes a 404 before any
		// transfer starts, and the size is what lets an implausibly large report
		// be refused rather than pulled into memory.
		named, err := c.statStorageObject(ctx, bucket, name)
		if err != nil {
			return obj, kind, "", "", "", toolError("report", c.bucketPermissionHint(err))
		}
		return named, kind, objectMonth(base), dimension, strings.TrimSpace(args.PackageName), nil
	}

	if strings.TrimSpace(args.Kind) == "" {
		return obj, kind, "", "", "", fmt.Errorf("no report kind — pass --kind (%s), or --object to name a file from `rollout play reports list`", strings.Join(reportKindNames(), ", "))
	}
	if kind, err = lookupReportKind(args.Kind); err != nil {
		return obj, kind, "", "", "", err
	}
	if month, err = parseReportMonth(args.Month); err != nil {
		return obj, kind, "", "", "", err
	}
	if dimension, err = kind.resolveDimension(args.Dimension); err != nil {
		return obj, kind, "", "", "", err
	}
	if kind.PerPackage {
		if pkg, err = c.resolvePackage(args.PackageName); err != nil {
			return obj, kind, "", "", "", err
		}
	}

	objects, truncated, err := c.listStorageObjects(ctx, bucket, objectPrefix(kind, pkg))
	if err != nil {
		return obj, kind, "", "", "", toolError("report", c.bucketPermissionHint(err))
	}
	obj, err = resolveReportObject(reportListing{objects, truncated}, kind, pkg, month, dimension)
	return obj, kind, month, dimension, pkg, err
}

// --- play_installs / play_ratings ---

// installsColumns are the installs figures worth a table column, in the order
// a person reads them. A report that does not carry one is not an error: Play
// has changed this column set over the years.
var installsColumns = []string{
	"date", "daily_device_installs", "daily_device_uninstalls", "daily_device_upgrades",
	"active_device_installs", "daily_user_installs", "daily_user_uninstalls", "total_user_installs",
}

// ratingsColumns are the rating figures, same idea.
var ratingsColumns = []string{"date", "daily_average_rating", "total_average_rating"}

// DailyReportArgs is the trailing-window convenience over play_report.
type DailyReportArgs struct {
	PackageName string `json:"package_name,omitempty" jsonschema:"the Android package name; omit to use the configured default app"`
	Days        int    `json:"days,omitempty" jsonschema:"how many days back to report, ending yesterday; defaults to 30, at most 731"`
	End         string `json:"end,omitempty" jsonschema:"the last day of the window as YYYY-MM-DD; defaults to yesterday"`
}

// DailyReportResult is a daily series stitched together from the monthly CSVs
// that cover the window.
type DailyReportResult struct {
	Bucket      string   `json:"bucket"`
	PackageName string   `json:"package_name"`
	Kind        string   `json:"kind"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Days        int      `json:"days"`
	Months      []string `json:"months"`
	// MissingMonths names months whose export does not exist at all — a new
	// app, or a month Play has not written yet.
	MissingMonths []string            `json:"missing_months,omitempty"`
	Columns       []string            `json:"columns"`
	Rows          []map[string]string `json:"rows"`
	// DataThrough is the latest day that actually has a row.
	DataThrough string `json:"data_through,omitempty"`
	// MissingDays lists every day in the window with no row, so a caller reads
	// "not exported yet" instead of inferring zero installs.
	MissingDays []string `json:"missing_days,omitempty"`
	Note        string   `json:"note,omitempty"`
}

func (r DailyReportResult) tableRows() ([]json.RawMessage, []string) {
	return jsonRows(r.Rows), r.Columns
}

// completeness names the days the table has no row for. Absent days are the one
// thing these tools exist to be honest about, and a table that simply skips
// them looks like a shorter month.
func (r DailyReportResult) completeness() string {
	if len(r.MissingDays) == 0 {
		return ""
	}
	through := r.DataThrough
	if through == "" {
		through = "nothing in this window"
	}
	return fmt.Sprintf("%d of %d days have no row yet (latest is %s): %s", len(r.MissingDays), r.Days, through, strings.Join(r.MissingDays, ", "))
}

// runInstalls reports daily installs over a trailing window.
func runInstalls(ctx context.Context, c *Client, args DailyReportArgs) (DailyReportResult, error) {
	return runDailyReport(ctx, c, "installs", installsColumns, args)
}

// runRatings reports the daily average rating over a trailing window.
func runRatings(ctx context.Context, c *Client, args DailyReportArgs) (DailyReportResult, error) {
	return runDailyReport(ctx, c, "ratings", ratingsColumns, args)
}

// runDailyReport stitches the overview CSVs covering a trailing window into one
// daily series.
//
// The exports are monthly, so a 30-day window normally spans two files, and the
// current month's file is rewritten as days land. Reading both and filtering by
// date is what makes `--days 30` mean thirty days rather than "this month".
func runDailyReport(ctx context.Context, c *Client, kindName string, preferred []string, args DailyReportArgs) (DailyReportResult, error) {
	kind, err := lookupReportKind(kindName)
	if err != nil {
		return DailyReportResult{}, err
	}
	bucket, err := c.reportsBucket()
	if err != nil {
		return DailyReportResult{}, err
	}
	pkg, err := c.resolvePackage(args.PackageName)
	if err != nil {
		return DailyReportResult{}, err
	}
	start, end, days, err := resolveDayWindow(args.Days, args.End)
	if err != nil {
		return DailyReportResult{}, err
	}

	objects, truncated, err := c.listStorageObjects(ctx, bucket, objectPrefix(kind, pkg))
	if err != nil {
		return DailyReportResult{}, toolError(kindName, c.bucketPermissionHint(err))
	}
	listing := reportListing{objects, truncated}

	out := DailyReportResult{
		Bucket: bucket, PackageName: pkg, Kind: kind.Name,
		Start: start.Format(dateLayout), End: end.Format(dateLayout), Days: days,
		Rows: []map[string]string{},
	}
	var columns []string
	for _, month := range monthsInWindow(start, end) {
		out.Months = append(out.Months, month[:4]+"-"+month[4:])
		obj, err := resolveReportObject(listing, kind, pkg, month, kind.DefaultDimension)
		if err != nil {
			// A month with no export is the normal state for the current one,
			// and for every month before the app existed: it contributes
			// missing days, not a failed call. Every other resolution failure —
			// an ambiguous match, a listing that stopped short — is a reason the
			// answer would be wrong, and reporting those as "not exported" would
			// assert an absence nobody established.
			var absent *reportNotExportedError
			if !errors.As(err, &absent) {
				return DailyReportResult{}, toolError(kindName, err)
			}
			out.MissingMonths = append(out.MissingMonths, month[:4]+"-"+month[4:])
			continue
		}
		table, _, _, err := c.readReportObject(ctx, bucket, obj, kind.Zipped, 0)
		if err != nil {
			return DailyReportResult{}, toolError(kindName, c.bucketPermissionHint(err))
		}
		if err := verifyReportPackage(table, kind, pkg, obj.Name); err != nil {
			return DailyReportResult{}, toolError(kindName, err)
		}
		// Union rather than first-wins: Play has changed this column set between
		// months, and a field the later file added is in the rows either way —
		// dropping it from the column list would hide it from every table and
		// CSV rendering while the JSON still carried it.
		columns = unionColumns(columns, table.Columns)
		for _, row := range table.Rows {
			if withinWindow(row["date"], start, end) {
				out.Rows = append(out.Rows, row)
			}
		}
	}

	sort.SliceStable(out.Rows, func(i, j int) bool { return out.Rows[i]["date"] < out.Rows[j]["date"] })
	out.Columns = presentColumns(preferred, columns)
	out.MissingDays, out.DataThrough = missingDays(out.Rows, start, end)

	switch {
	case len(out.Rows) == 0:
		out.Note = "no rows in this window — " + reportsLagNote
	case len(out.MissingDays) > 0:
		out.Note = fmt.Sprintf("%d of %d days have no row (latest is %s) — %s",
			len(out.MissingDays), days, out.DataThrough, reportsLagNote)
	default:
		out.Note = reportsLagNote
	}
	return out, nil
}

// dateLayout is the date format the CSV exports use and these tools speak.
const dateLayout = "2006-01-02"

// maxReportDays bounds a trailing window. The series is stitched from monthly
// files, so days translate directly into downloads: a mistyped `--days 36500`
// would ask for a hundred years of them. Two years is past any question these
// tools are for, and a longer span is a job for `reports get` a month at a time.
const maxReportDays = 731

// resolveDayWindow turns --days and --end into an inclusive date range.
//
// The window ends yesterday by default, but that is not the real edge: these
// exports lag by days, so the last rows are usually missing regardless. Ending
// today would only add one more guaranteed-empty day.
func resolveDayWindow(days int, endStr string) (start, end time.Time, resolved int, err error) {
	if days <= 0 {
		days = 30
	}
	if days > maxReportDays {
		return start, end, 0, fmt.Errorf("days = %d is more than the %d-day maximum — read a longer span a month at a time with `rollout play reports get`", days, maxReportDays)
	}
	// "Yesterday" means the day before the caller's own calendar date — the
	// same reading `rollout play vitals` takes. Reading it off UTC would, just
	// after local midnight east of Greenwich, hand back the day before that.
	// The date is then carried as UTC midnight purely so the arithmetic below
	// has no zone to trip over.
	now := time.Now()
	end = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	if strings.TrimSpace(endStr) != "" {
		if end, err = time.Parse(dateLayout, strings.TrimSpace(endStr)); err != nil {
			return start, end, 0, fmt.Errorf("invalid end %q — use YYYY-MM-DD", endStr)
		}
		end = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	}
	return end.AddDate(0, 0, -(days - 1)), end, days, nil
}

// monthsInWindow lists the yyyyMM stamps a date range touches.
func monthsInWindow(start, end time.Time) []string {
	var months []string
	for t := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC); !t.After(end); t = t.AddDate(0, 1, 0) {
		months = append(months, t.Format("200601"))
	}
	return months
}

// withinWindow reports whether a CSV date column falls inside the range. A row
// whose date does not parse is dropped rather than kept: these files carry a
// trailing total row in some months, and counting it as a day would double one.
func withinWindow(date string, start, end time.Time) bool {
	t, err := time.Parse(dateLayout, strings.TrimSpace(date))
	if err != nil {
		return false
	}
	return !t.Before(start) && !t.After(end)
}

// unionColumns appends the columns b adds to a, preserving first-seen order.
func unionColumns(a, b []string) []string {
	have := make(map[string]bool, len(a))
	for _, c := range a {
		have[c] = true
	}
	for _, c := range b {
		if !have[c] {
			have[c] = true
			a = append(a, c)
		}
	}
	return a
}

// presentColumns keeps the preferred column order but drops the ones this
// report does not carry.
//
// Curation is the point: an installs overview has a dozen columns and a
// terminal table of all of them is unreadable. But Play has renamed and dropped
// these columns over the years, and a curated view that matched nothing but the
// date would render a table with no numbers in it — so a view left with no
// column beyond the date key falls back to the report's own header.
func presentColumns(preferred, actual []string) []string {
	have := make(map[string]bool, len(actual))
	for _, c := range actual {
		have[c] = true
	}
	var columns []string
	for _, c := range preferred {
		if have[c] {
			columns = append(columns, c)
		}
	}
	if len(columns) < 2 {
		return actual
	}
	return columns
}

// missingDays lists the days in the window with no row, and the latest day that
// has one. Naming them is the point of the tool: an absent day means "not
// exported yet", and a chart that fills it with zero shows a cliff that never
// happened.
func missingDays(rows []map[string]string, start, end time.Time) (missing []string, dataThrough string) {
	present := make(map[string]bool, len(rows))
	for _, row := range rows {
		if date := strings.TrimSpace(row["date"]); date != "" {
			present[date] = true
			if date > dataThrough {
				dataThrough = date
			}
		}
	}
	for t := start; !t.After(end); t = t.AddDate(0, 0, 1) {
		if day := t.Format(dateLayout); !present[day] {
			missing = append(missing, day)
		}
	}
	return missing, dataThrough
}

// --- CLI front-end ---

var (
	reportsListArgs   ReportsListArgs
	reportsListFormat string
	reportArgs        ReportArgs
	reportFormat      string
	installsArgs      DailyReportArgs
	installsFormat    string
	ratingsArgs       DailyReportArgs
	ratingsFormat     string
)

var reportsCmd = &cobra.Command{
	Use:   "reports",
	Short: "Read the CSV report exports from the Play reports bucket",
	Long: "Installs, ratings, crash statistics, store performance, reviews and the financial\n" +
		"reports have no Play API — Play exports them as monthly CSVs to a Cloud Storage\n" +
		"bucket. Point rollout at it with `rollout config play set-reports-bucket`.\n\n" +
		"Report kinds:\n" + reportKindHelp(),
}

// reportKindHelp renders the kind table for `rollout play reports --help`,
// which is where someone reaches for it after a rejected --kind.
func reportKindHelp() string {
	var b strings.Builder
	width := 0
	for _, kind := range reportKinds {
		if len(kind.Name) > width {
			width = len(kind.Name)
		}
	}
	for _, kind := range reportKinds {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, kind.Name, kind.Description)
		if len(kind.Dimensions) > 0 {
			fmt.Fprintf(&b, "  %-*s  dimensions: %s\n", width, "", strings.Join(kind.Dimensions, ", "))
		}
	}
	return b.String()
}

var reportsListCmd = &cobra.Command{
	Use:         "list",
	Short:       "List the exported report files in the bucket",
	Annotations: mcpTool("reports_list"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, reportsListArgs, reportsListFormat, runReportsList)
	},
}

var reportsGetCmd = &cobra.Command{
	Use:         "get",
	Short:       "Download and parse one exported report",
	Annotations: mcpTool("report"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, reportArgs, reportFormat, runReport)
	},
}

var reportsInstallsCmd = &cobra.Command{
	Use:         "installs",
	Short:       "Daily installs, uninstalls and active devices over a trailing window",
	Annotations: mcpTool("installs"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, installsArgs, installsFormat, runInstalls)
	},
}

var reportsRatingsCmd = &cobra.Command{
	Use:         "ratings",
	Short:       "Daily average rating over a trailing window",
	Annotations: mcpTool("ratings"),
	Args:        cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runPlayRead(cmd, ratingsArgs, ratingsFormat, runRatings)
	},
}

func init() {
	addPackageFlag(reportsListCmd, &reportsListArgs.PackageName)
	reportsListCmd.Flags().StringVar(&reportsListArgs.Kind, "kind", "", "narrow to one report kind ("+strings.Join(reportKindNames(), ", ")+")")
	reportsListCmd.Flags().StringVar(&reportsListArgs.Month, "month", "", "narrow to one month (YYYY-MM)")
	reportsListCmd.Flags().IntVar(&reportsListArgs.Max, "max", defaultReportsListMax, "cap how many objects to list")
	addFormatFlag(reportsListCmd, &reportsListFormat)

	addPackageFlag(reportsGetCmd, &reportArgs.PackageName)
	reportsGetCmd.Flags().StringVar(&reportArgs.Kind, "kind", "", "report kind ("+strings.Join(reportKindNames(), ", ")+")")
	reportsGetCmd.Flags().StringVar(&reportArgs.Month, "month", "", "month to read (YYYY-MM)")
	reportsGetCmd.Flags().StringVar(&reportArgs.Dimension, "dimension", "", "breakdown to read (default: the kind's own)")
	reportsGetCmd.Flags().StringVar(&reportArgs.Object, "object", "", "read this exact object path instead of resolving one from --kind/--month (the default app is not applied)")
	reportsGetCmd.Flags().StringVar(&reportArgs.Out, "out", "", "also write the complete report to this file, as UTF-8 CSV (CLI only)")
	reportsGetCmd.Flags().BoolVar(&reportArgs.Force, "force", false, "let --out replace a file that already exists (CLI only)")
	reportsGetCmd.Flags().IntVar(&reportArgs.MaxRows, "max-rows", defaultReportMaxRows, "cap how many parsed rows to return")
	addFormatFlag(reportsGetCmd, &reportFormat)

	addPackageFlag(reportsInstallsCmd, &installsArgs.PackageName)
	reportsInstallsCmd.Flags().IntVar(&installsArgs.Days, "days", 30, "days back to report, ending yesterday")
	reportsInstallsCmd.Flags().StringVar(&installsArgs.End, "end", "", "last day of the window (YYYY-MM-DD)")
	addFormatFlag(reportsInstallsCmd, &installsFormat)

	addPackageFlag(reportsRatingsCmd, &ratingsArgs.PackageName)
	reportsRatingsCmd.Flags().IntVar(&ratingsArgs.Days, "days", 30, "days back to report, ending yesterday")
	reportsRatingsCmd.Flags().StringVar(&ratingsArgs.End, "end", "", "last day of the window (YYYY-MM-DD)")
	addFormatFlag(reportsRatingsCmd, &ratingsFormat)

	reportsCmd.AddCommand(reportsListCmd, reportsGetCmd, reportsInstallsCmd, reportsRatingsCmd)
}
