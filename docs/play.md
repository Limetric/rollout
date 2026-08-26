# Google Play

Everything `rollout` does with Google Play, and what you need to set up first.

- [Prerequisites](#prerequisites)
- [Guided setup](#guided-setup)
- [Manual setup (CI)](#manual-setup-ci)
- [Pick a default app](#pick-a-default-app)
- [Everyday commands](#everyday-commands)
- [Tool coverage](#tool-coverage)
- [Release semantics](#release-semantics)
- [Guard rails](#guard-rails)
- [Troubleshooting](#troubleshooting)
- [Running the integration suite](#running-the-integration-suite)

## Prerequisites

You need a Google Play Console account with an app that already exists. **There
is no API for creating an app** — it must be created in the Console, and it must
have at least one uploaded artifact before the API can see it at all.

Then, in a Google Cloud project:

1. Enable the **Google Play Android Developer API**
   ([console](https://console.cloud.google.com/apis/library/androidpublisher.googleapis.com)).
2. Enable the **Google Play Developer Reporting API**
   ([console](https://console.cloud.google.com/apis/library/playdeveloperreporting.googleapis.com))
   if you want vitals, crash clusters, or the app list.
3. Create either a **service account** (headless, what CI should use) or a
   **Desktop-app OAuth client** (for signing in as yourself).

Finally, grant access in Play Console → **Users & permissions**. This is the
step people miss: a credential that authenticates perfectly still cannot see
your app until it is invited there.

| To use | Grant |
| --- | --- |
| Reads, releases, store listing writes | *Release to production, exclude devices, and use Play App Signing* on the app |
| `play_vitals`, `play_error_issues`, `play_anomalies`, `play_apps` | *View app information (read-only)* — **Release Manager alone does not grant this** |
| Reviews | *Reply to reviews* |

## Guided setup

```bash
rollout login play
```

Walks you through enabling the APIs, choosing a credential, and granting access,
then finishes with a live check against your app. Use
`rollout login play --service-account key.json` to skip the wizard and just
record a key you already have.

The OAuth path runs Google's loopback authorization-code flow with PKCE: your
browser opens, the code is captured on `127.0.0.1`, and the refresh token is
saved to the token store (`rollout config show` prints where). It never lands in
`config.toml`.

## Manual setup (CI)

Everything can come from the environment; no config file is needed.

| Variable | Meaning |
| --- | --- |
| `PLAY_SERVICE_ACCOUNT_FILE` | path to a service-account JSON key |
| `PLAY_SERVICE_ACCOUNT_JSON` | the key inline, for secret stores that hand out values rather than files |
| `PLAY_PACKAGE_NAME` | default app, so `--package` can be omitted |
| `PLAY_CLIENT_ID` / `PLAY_CLIENT_SECRET` | OAuth client, for the user sign-in flow |
| `PLAY_DEVELOPER_ID` | Play Console developer account ID (users and permissions tools) |
| `PLAY_REPORTS_BUCKET` | `pubsite_prod_rev_…` GCS bucket for CSV report exports |
| `PLAY_API_BASE_URL` / `PLAY_REPORTING_BASE_URL` | endpoint overrides (tests) |
| `ROLLOUT_TOKEN_STORE` | where the refresh-token store lives |

`GOOGLE_APPLICATION_CREDENTIALS` is accepted as a fallback, so a machine already
set up for another Google tool works — but `PLAY_SERVICE_ACCOUNT_FILE` wins.

Check it with:

```bash
rollout doctor play            # opens and deletes a real edit
rollout doctor play --offline  # credentials only, no network
```

## Pick a default app

```bash
rollout config play set-package com.example.app
```

Every tool then takes `--package` optionally. `rollout play apps` lists what
your credential can reach, with the default first.

## Everyday commands

```bash
# What is live where?
rollout play releases --format table

# Ship a build to internal as a draft
rollout play upload --file app/build/outputs/bundle/release/app.aab
rollout confirm <token>

# Promote it to beta, fully rolled out
rollout play promote --from internal --to beta
rollout confirm <token>

# Start a 10% production rollout
rollout play promote --from beta --to production --status inProgress --rollout 0.1
rollout confirm <token>

# Watch it, then grow it
rollout play vitals summary --days 3
rollout play release update --track production --rollout 0.5
rollout confirm <token>

# Something is wrong — stop it (two confirmations)
rollout play release halt --track production
rollout confirm <token>
rollout confirm <second-token>
```

## Tool coverage

Reads:

| Tool | What it does |
| --- | --- |
| `play_apps` | Apps these credentials can reach; the default is marked and listed first |
| `play_tracks` | Tracks with their releases: status, version codes, rollout percentage, note locales |
| `play_releases` | Every release across tracks, flattened — "what is live where" |
| `play_artifacts` | Uploaded bundles and APKs, newest first, with SHA-256 |
| `play_listing` | Store listing text per locale |
| `play_images` | Store listing images for a locale, with SHA-256 |
| `play_details` | Default language and developer contact details |
| `play_testers` | Google Groups testing a track |
| `play_countries` | Where a track is available |
| `play_device_tiers` | Device tier configurations |
| `play_edit_status` | Cheap "can I publish to this app?" probe |

Release management (all preview first):

| Tool | What it does |
| --- | --- |
| `play_upload_artifact` | Upload an `.aab`/`.apk` and add it to a track |
| `play_create_release` | Create a release from uploaded version codes |
| `play_update_release` | Change a release — the staged-rollout dial |
| `play_halt_release` | Stop a rollout (two confirmations) |
| `play_resume_release` | Restart a halted rollout |
| `play_complete_release` | Roll out to every user (two confirmations on production) |
| `play_promote_release` | Move a release between tracks, carrying its notes across |
| `play_set_testers` | Replace a track's tester Google Groups |
| `play_set_countries` | Set the countries a release ships to |
| `play_upload_deobfuscation` | Attach a mapping file or native symbols to a build |

Store listing (all preview first):

| Tool | What it does |
| --- | --- |
| `play_update_listing` | Set title, descriptions, and promo video for a locale |
| `play_delete_listing` | Delete a locale's listing, or all of them (two confirmations) |
| `play_upload_images` | Upload screenshots and graphics |
| `play_delete_images` | Delete images by id, or a whole type (two confirmations) |
| `play_update_details` | Set default language and contact details |
| `play_sync_listing` | Reconcile a metadata directory with the store, in one edit |

Reviews:

| Tool | What it does |
| --- | --- |
| `play_reviews` | List reviews with their existing replies |
| `play_review` | Read one review |
| `play_reply_review` | Reply publicly (previews the review first) |

Reporting:

| Tool | What it does |
| --- | --- |
| `play_vitals` | Query a vitals metric set over a date range |
| `play_vitals_summary` | Crash and ANR rates against Play's thresholds, with an `ok` flag |
| `play_error_issues` | Crash and ANR clusters, most-users-affected first |
| `play_error_reports` | Individual reports behind an issue — the stack traces |
| `play_anomalies` | Metric anomalies Play itself flagged |

See [`name-map.md`](name-map.md) for the CLI command each one corresponds to,
and [`reporting.md`](reporting.md) for the vitals metric sets in detail.

## Release semantics

A release moves through four statuses:

| Status | Meaning |
| --- | --- |
| `draft` | On the track, reaching nobody. Where uploads land by default. |
| `inProgress` | A staged rollout, reaching `userFraction` of users. |
| `completed` | Reaching every user. Carries no fraction. |
| `halted` | A stopped rollout. Users who already have it keep it. |

Two rules the API enforces and this tool checks first, because its own errors
name neither the field nor the value:

- `inProgress` requires a fraction strictly between 0 and 1.
- `completed` and `draft` must not carry one. **"Roll out to 100%" is
  `completed`, not `inProgress` at 1.0** — `--rollout 1` is translated for you.

A track can hold several releases at once: a `completed` one still serving most
users, an `inProgress` one rolling out, a `draft` waiting. Every write here
reads the track first and changes exactly one release, so a rollout you did not
mention is never dropped.

Changes are staged in an **edit**, a server-side transaction. `rollout` opens
one per call, and a write validates before it commits and deletes the edit on
any failure — so a rejected write leaves nothing half-staged. Edit IDs are never
persisted: they expire, and opening a new edit invalidates the last.

## Guard rails

Optional, off by default. Set them in `config.toml` or the environment:

```toml
[play.safety]
production_lock = true                 # PLAY_PRODUCTION_LOCK=1
max_rollout_fraction = 0.2             # PLAY_MAX_ROLLOUT_FRACTION=0.2
blocked_operations = ["halt_release"]  # PLAY_BLOCKED_OPS=halt_release
```

| Setting | Effect |
| --- | --- |
| `production_lock` | Every write touching `production` takes a second confirmation |
| `max_rollout_fraction` | Refuses a write past this fraction. Completing a release counts as 1.0, so `0.2` lets CI stage and grow a rollout but never finish one |
| `blocked_operations` | Refuses these tools outright |

They are re-checked when a token is confirmed, not only when it is issued, so
tightening one always beats a preview already in flight.

`rollout audit` prints every applied write, and every failed apply, one JSON
line each.

## Troubleshooting

**`403` / "The caller does not have permission"** — the credential is valid but
was never invited. Play Console → Users & permissions, add the service-account
email (`rollout config show` prints it) and grant it access to the app.

**`403` on vitals only** — the Reporting API needs *View app information
(read-only)*, which Release Manager does not include, and the Google Play
Developer Reporting API must be enabled in the Cloud project.

**`404` on the package** — check the spelling, and remember a new app must be
created in the Console and have one uploaded artifact before the API can see it.

**`409` / `editAlreadyCommitted`** — something else changed the app while your
edit was open. Re-run; a fresh edit is opened each time.

**`400` on release notes** — a locale is over 500 characters. `rollout` checks
this first and names the locale; if you see the raw API error, the notes came
from somewhere else.

**Empty vitals or reviews** — vitals lag real time by up to a few days, versions
with too few users are withheld for privacy, and the reviews API returns only
the last week and only for apps with a live production release.

**Quota** — the Publisher API allows 200,000 requests/day and the Reporting API
10 queries/second. Reads that open an edit count against it, which is why each
tool uses exactly one.

## Running the integration suite

The live suite runs against a real app and skips itself without credentials:

```bash
PLAY_SERVICE_ACCOUNT_FILE=key.json PLAY_PACKAGE_NAME=com.example.app \
  go test -tags integration -count=1 -v ./...
```

It never uploads an artifact and never touches production — a guard test
enforces both by scanning its own source. The one write it performs sets a
testing track's tester groups to the value they already have, which exercises
the whole staging, validate, and commit path without changing anything.
