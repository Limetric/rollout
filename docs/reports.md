# CSV report exports

Installs, ratings, crash statistics, store performance, subscriptions, the full
review history, and the financial reports are **not in any Play API**. Play
exports them as monthly CSVs to a Cloud Storage bucket — the one Play Console →
Download reports calls the "Cloud Storage URI" — and reading that bucket is the
only way to get these numbers.

That makes them a different animal from [`reporting.md`](reporting.md): Android
vitals come from a live API, these come from a batch job that runs a few days
behind.

- [Setup](#setup)
- [Report kinds](#report-kinds)
- [Listing what exists](#listing-what-exists)
- [Reading one report](#reading-one-report)
- [Trailing windows](#trailing-windows)
- [What the files are really like](#what-the-files-are-really-like)
- [Troubleshooting](#troubleshooting)

## Setup

```bash
rollout config play set-reports-bucket pubsite_prod_rev_1234567890
rollout doctor play
```

The bucket name is in Play Console → **Download reports**, on any report page,
shown as `gs://pubsite_prod_rev_<developer id>`. A full `gs://…` URI is accepted
and trimmed. `PLAY_REPORTS_BUCKET` is the environment equivalent.

Two things about access:

- The bucket inherits Play Console permissions. A credential needs **View app
  information** for the statistics reports, and **View financial data** for
  sales, earnings, and subscriptions.
- Setting a bucket is what adds the read-only Cloud Storage scope to the
  credential. **If you ran `rollout login play` before setting it, run it
  again** — the saved token predates the scope, and no amount of re-configuring
  fixes that. Service accounts are unaffected.

`rollout doctor play` runs a `reports probe` against the bucket once one is
configured, and unlike the reporting probe its failure is a real failure:
setting `reports_bucket` says you want this capability.

## Report kinds

| `--kind` | Bucket folder | Dimensions | Notes |
| --- | --- | --- | --- |
| `installs` | `stats/installs/` | `overview` (default), `country`, `language`, `app_version`, `carrier`, `device`, `os_version` | device/user installs, uninstalls, upgrades, active devices |
| `ratings` | `stats/ratings/` | same as installs | daily and cumulative average star rating |
| `crashes` | `stats/crashes/` | `overview` (default), `app_version`, `device`, `os_version` | the CSV export, not Android vitals |
| `store_performance` | `stats/store_performance/` | `country` (default), `traffic_source` | listing visitors and acquisitions |
| `subscriptions` | `financial-stats/subscriptions/` | `country` | **one file per product id** — see below |
| `reviews` | `reviews/` | — | every review with its text and reply, going back further than the one week the reviews API returns |
| `sales` | `sales/` | — | order-level sales, whole developer account, **zipped** |
| `earnings` | `earnings/` | — | payouts and fees, whole developer account, **zipped** |

`sales` and `earnings` cover the whole developer account rather than one app, so
`--package` does not narrow them. Every other kind, `reviews` included, names the
package in its file name and is scoped to it.

## Listing what exists

```bash
rollout play reports list --format table
rollout play reports list --kind installs --format table
rollout play reports list --kind sales --month 2026-07
```

`play_reports_list` returns each object with the kind, month, and dimension read
out of its name, plus its size and last-updated time. Start here: it is the
cheapest way to see which months Play has actually written, and the `name` it
returns is what `--object` takes.

Per-app kinds are scoped to the default app (or `--package`); `sales` and
`earnings` are account-wide and always listed. Listings are capped at 200
objects (`--max`) and return the **newest months first**, so the cap never hides
the current export; a capped result says so.

## Reading one report

```bash
# Parsed rows as JSON
rollout play reports get --kind installs --month 2026-07

# Broken down by country, as a table
rollout play reports get --kind installs --month 2026-07 --dimension country --format table

# The whole file on disk, as UTF-8 CSV
rollout play reports get --kind sales --month 2026-07 --out sales-2026-07.csv
```

An object is **resolved by listing**, never by building a file name: a
subscriptions file embeds the product id and an earnings archive a per-account
suffix, so a template would fail on exactly the reports people most want. A miss
therefore names the months that do exist instead of returning a bare 404.

Where several files match — a subscriptions month with two products — the tool
refuses to pick, because reading the first would report one product's numbers as
the app's. Name one outright:

```bash
rollout play reports list --kind subscriptions --month 2026-07
rollout play reports get --object financial-stats/subscriptions/subscriptions_com.example.app_sku.monthly_202607_country.csv
```

If a developer account owns two packages where one name is a prefix of the
other (`com.foo` and `com.foo_bar`), a subscriptions file name cannot always say
which app it belongs to — the product id sits in between, and `reports list`
says so when it cannot be certain. The report's own package column is checked
after reading, and a mismatch is refused rather than returned.

`--object` reads exactly the file you name and applies no default app, so it is
the way past that check. Pass `--package` alongside it to assert which app the
file should belong to and have it verified anyway.

Parsed rows are capped at 2000 (`--max-rows`) so a country-dimension month
cannot flood an agent's context; `row_count` always reports what the file
actually held. **The cap never applies to `--out`**, which
always writes the complete file.

`--out` is **CLI-only**. Everything else these tools do is a read, and serving
an output path over MCP would let an agent create a file at any path the server
can write to, with no preview and no confirm token — the thing `rollout`'s
safety flow exists to prevent for every other write. An MCP host that wants a
report on disk can save the tool's result itself.

On the CLI, `--out` will not replace a file that already exists; `--force` lifts
that. The file is written `0600` — sales, earnings, subscription and review
exports are commercial data, and a shared directory is not the place to leave
them world-readable. A report that fails its package check is never written; one that fails to
*parse* is, because inspecting the file is the next thing you need.

## Trailing windows

```bash
rollout play reports installs --days 30 --format table
rollout play reports ratings --days 30
```

These stitch the monthly files covering the window into one daily series, so
`--days 30` means thirty days rather than "this month". `--end YYYY-MM-DD` moves
the window; it ends yesterday by default. Days translate directly into
downloads, so the window is capped at two years — read a longer span a month at
a time with `reports get`.

Days that have not been exported yet come back as `missing_days`, with
`data_through` naming the last day that has a row:

```json
{
  "data_through": "2026-08-22",
  "missing_days": ["2026-08-23", "2026-08-24", "2026-08-25", "2026-08-26"],
  "note": "4 of 30 days have no row (latest is 2026-08-22) — Play writes these CSVs a few days in arrears…"
}
```

That is the whole point of naming them: a missing day means *not exported yet*,
and a chart that fills it with zero shows a cliff that never happened. A month
Play has not written at all is reported as a `missing_months` entry rather than
failing the call.

## What the files are really like

Three properties trip up every first attempt at reading this bucket, and
`rollout` handles all three:

- **UTF-16.** The CSVs are UTF-16 little-endian with a byte-order mark. Read as
  UTF-8 they look like a header full of NUL bytes. Everything `rollout` returns
  — and everything it writes with `--out` — is UTF-8.
- **A size bound.** These files are read whole — `--out` writes the entire
  document, and the UTF-16 decode has no incremental form — so an object (or an
  archive's contents) past 512 MB is refused with a pointer to `gsutil cp`
  rather than an allocation failure. No export Play actually produces comes
  close.
- **Zipped financials.** `sales` and `earnings` are zip archives holding the
  CSV. It is extracted for you, and an archive holding several CSVs has them
  joined — reading only the first would report a slice of the payout as the
  whole of it. `archive_member` names what was read and `archive_members`
  everything the archive held. CSVs that do not share a header are not one
  report, and that is an error rather than a number that is quietly wrong.
- **Snake-cased headers.** `Daily Device Installs` becomes
  `daily_device_installs`, matching how every other `rollout` result names its
  fields. Values are returned exactly as the file has them, as strings — a
  leading-zero country code or a large version code that went through a float
  would come back wrong, and a review's text may open or close with whitespace.

`reports installs` and `reports ratings` pick a readable subset of columns for
`--format table` and `--format csv`; Play's overview export has around a dozen,
and a terminal table of all of them is unreadable. **`--format json` always
carries every column the file has**, so reach for it when a figure you expect is
not in the table.

## Troubleshooting

**`no reports bucket configured`** — run `rollout config play
set-reports-bucket`, or set `PLAY_REPORTS_BUCKET`.

**`403` on the bucket** — if you signed in with `rollout login play` before
setting the bucket, the saved token predates the Cloud Storage scope: sign in
again. For a service account, grant it report access in Play Console → Users &
permissions (**View app information** for statistics, **View financial data**
for sales, earnings, and subscriptions).

**A month is missing** — exports run a few days in arrears, and the current
month's file is rewritten as days land. `rollout play reports list --kind
installs` shows exactly which months exist.

**Numbers do not match the Console** — the statistics exports are aggregated in
a fixed time zone and the Console renders in yours, so day boundaries can shift
one bucket. The last few days of any window are also incomplete by design.
