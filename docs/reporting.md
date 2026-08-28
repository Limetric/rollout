# Reporting

Android vitals, crash and ANR clusters, and anomalies, via the **Play Developer
Reporting API v1beta1** — a different service from the publishing API, with its
own scope, its own permission, and its own quota.

Installs, ratings, store performance and the financial reports are *not* here:
they have no API and are exported as CSV to a Cloud Storage bucket. See
[`reports.md`](reports.md).

## Access

| Requirement | Where |
| --- | --- |
| Google Play Developer Reporting API enabled | Cloud Console → APIs & Services |
| Scope `playdeveloperreporting` | requested automatically |
| *View app information (read-only)* on the app | Play Console → Users & permissions |

The third is the one that catches people: a service account with full release
permissions publishes fine and gets a `403` on every vitals call. *Release
Manager does not include it.*

Quota is **10 queries per second**. `rollout` validates metric and dimension
names locally so a typo does not spend one.

## Metric sets

`rollout play vitals --metric <name>` / `play_vitals`:

| `--metric` | Metric set | Default metrics |
| --- | --- | --- |
| `crashrate` | `crashRateMetricSet` | `crashRate`, `crashRate7dUserWeighted`, `distinctUsers` |
| `anrrate` | `anrRateMetricSet` | `anrRate`, `anrRate7dUserWeighted`, `distinctUsers` |
| `errors` | `errorCountMetricSet` | `errorReportCount`, `distinctUsers` |
| `excessivewakeuprate` | `excessiveWakeupRateMetricSet` | `excessiveWakeupRate`, `distinctUsers` |
| `stuckbackgroundwakelockrate` | `stuckBackgroundWakelockRateMetricSet` | `stuckBgWakelockRate`, `distinctUsers` |
| `slowrenderingrate` | `slowRenderingRateMetricSet` | `slowRenderingRate20Fps`, `distinctUsers` |
| `slowstartrate` | `slowStartRateMetricSet` | `slowColdStartRate`, `distinctUsers` |
| `lmkrate` | `lmkRateMetricSet` | `userPerceivedLmkRate`, `distinctUsers` |

`--metrics` overrides the defaults; run with an unknown name to see what a set
accepts.

## Dimensions

Every metric set accepts the same breakdown dimensions:

`apiLevel`, `versionCode`, `countryCode`, `deviceModel`, `deviceType`,
`deviceRamBucket`, `deviceSocMake`, `deviceSocModel`, `deviceCpuMake`,
`deviceCpuModel`, `deviceGpuMake`, `deviceGpuModel`, `deviceGpuVersion`,
`deviceVulkanVersion`, `deviceGlEsVersion`, `deviceScreenSize`,
`deviceScreenDpi`.

```bash
rollout play vitals --metric crashrate --dimension versionCode --days 14 --format table
```

## Time windows

- `--days N` looks back N days ending **yesterday**. Today's data is partial,
  and a rate computed over a few hours reads as a spike.
- `--start` / `--end` take `YYYY-MM-DD` and override it.
- `--period daily` (default) aggregates in **America/Los_Angeles**, which is how
  Play buckets daily vitals — sending another zone shifts every bucket.
  `--period hourly` uses UTC.
- `--freshness` reports how far behind real time the metric set is. Vitals lag
  by hours to days, so a number without that context can predate the release you
  are judging.

## Thresholds

`rollout play vitals summary` / `play_vitals_summary` compares the **worst day**
in the window — not the average, which smooths exactly the spike a release
decision needs to see — against Play's bad-behaviour thresholds:

| Metric | Threshold |
| --- | --- |
| User-perceived crash rate | 1.09% |
| User-perceived ANR rate | 0.47% |

Above either, an app risks reduced discoverability. The result carries a single
`ok` flag so a pipeline can gate a rollout on it. **An unknown rate reports as
not-ok**: no data is a reason to look, not to ship.

## Errors and anomalies

- `play_error_issues` lists crash and ANR *clusters*, most-users-affected first,
  with the failing cause and location and the version range they span.
- `play_error_reports` lists the individual reports behind one cluster — this is
  where the stack traces are. Upload a mapping file with
  `play_upload_deobfuscation` or they are unreadable.
- `play_anomalies` lists what Play's own detection flagged. An empty list means
  nothing crossed *Play's* thresholds, which is not the same as the app being
  healthy — `play_vitals_summary` answers that.

Issues and reports are withheld when they affect too few users, for privacy.

## CSV report exports

Installs, ratings, financials, and full review archives are not in either API.
Play writes them as CSV into a Cloud Storage bucket named
`pubsite_prod_rev_<developer-id>`, readable with the
`devstorage.read_only` scope — which `rollout` requests only when
`reports_bucket` is configured, so nobody is asked to approve storage access
they do not use.

```toml
[play]
reports_bucket = "pubsite_prod_rev_01234567890123456789"
```

Tools that read the bucket are not implemented yet; the configuration and scope
are in place for them.
