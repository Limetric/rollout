---
name: rollout
description: Use when working with Google Play — checking what version is live, staging or growing a rollout, promoting a build between tracks, halting a bad release, editing store listings, managing in-app products and subscriptions, replying to reviews, reading crash/ANR vitals, or pulling install, rating, and revenue numbers from the CSV report exports. Drives the `rollout` CLI. Triggers include "google play", "play console", "staged rollout", "release notes", "store listing", "app bundle", "crash rate", "ANR", "promote to production", "in-app product", "subscription", "base plan", "offer", "how many installs", "download numbers", "app revenue", "average rating".
---

# rollout — Google Play via the `rollout` CLI

`rollout` is a single binary that talks to the Google Play REST APIs. Prefer it
over ad-hoc HTTP. Drive it from the shell; pipe large results through `jq` so
they never land in full in context.

Google Play lives in its own namespace, `rollout play …`; shared plumbing
(`doctor`, `login`, `config`, `confirm`, `audit`) sits at the top level.

## Setup check

Run this first if anything errors about credentials:

```bash
rollout doctor play
```

`NOT READY` means the setup is incomplete. **Never invent credentials or a
package name** — ask the user. The single most common cause is a service account
that authenticates fine but was never invited in Play Console → Users &
permissions; `rollout config show` prints the email they need to invite.

If no default app is configured, `rollout play apps` lists what the credential
can reach. Pass `--package com.example.app` to any command, or suggest
`rollout config play set-package`.

## Reading

Reads print JSON. Add `--format table` when a person is going to read it.

```bash
rollout play releases --format table          # what is live where
rollout play tracks --track production        # one track in detail
rollout play artifacts                        # uploaded bundles and APKs
rollout play listing --locale en-US           # store listing text
rollout play reviews --max 20 --min-stars 1 --max-stars 2 --unanswered
rollout play vitals summary --days 7          # crash/ANR against Play's thresholds
rollout play errors --days 7 --type crash     # top crash clusters
```

Useful facts when interpreting results:

- A track can hold several releases at once — a `completed` one still serving
  most users and an `inProgress` staged rollout. `rollout_percent` is the share
  of users on the staged one.
- Reviews only cover the **last week**, and only for apps with a live production
  release. An empty list is not evidence of no complaints.
- Vitals lag real time by up to a few days, and versions with too few users are
  withheld for privacy. `--freshness` says how current the data is.

## Writing — always previews first

Write commands are **two-step**. The first call changes nothing: it returns a
preview and a `confirm_token`. Show the preview to the user and get their
go-ahead before confirming.

```bash
# 1. Preview — read confirm_token from the output
rollout play release update --track production --rollout 0.25

# 2. Apply only after the user agrees
rollout confirm <token-from-step-1>
```

Re-running the original command with `--confirm <token>` works too. Tokens last
10 minutes.

**Destructive operations return a second token that must be confirmed once
more**: halting a rollout, completing a production rollout, deleting a store
listing, deleting a whole image type, deactivating a subscription base plan,
revoking a grant, and removing a user. That is deliberate — surface it to the
user rather than confirming twice on your own initiative.

Never skip the preview, never guess a token, and never confirm a write the user
has not approved. `rollout audit` lists every write that has been applied.

## Release workflow

```bash
# Ship a build to internal as a draft (the default)
rollout play upload --file app.aab

# Promote what was tested, keeping its release notes
rollout play promote --from internal --to beta

# Start a production rollout at 10%
rollout play promote --from beta --to production --status inProgress --rollout 0.1

# Grow it once vitals look fine
rollout play vitals summary --days 3
rollout play release update --track production --rollout 0.5

# Finish it (two confirmations on production)
rollout play release complete --track production

# Or stop it (two confirmations)
rollout play release halt --track production
```

Rules worth knowing before you compose a command:

- **"Roll out to 100%" is `--status completed`, not `--rollout 1`.** The tool
  translates `--rollout 1` for you, but say `complete` when that is the intent.
- `inProgress` needs a fraction strictly between 0 and 1; `draft` and
  `completed` must not carry one.
- Release notes are capped at 500 characters per locale. `--notes-dir` reads a
  fastlane `metadata/android` tree.
- A release write never rewrites a track from your arguments — it changes the
  one release you named and leaves the rest alone.

## Install, rating and revenue numbers

These live in **no Play API**. Play exports them as monthly CSVs to a Cloud
Storage bucket, and `rollout play reports` reads it:

```bash
rollout play reports installs --days 30 --format table   # daily installs/uninstalls/active devices
rollout play reports ratings --days 30                   # daily average rating
rollout play reports list --kind sales                   # what months exist
rollout play reports get --kind sales --month 2026-07 --out sales.csv
```

Kinds are `installs`, `ratings`, `crashes`, `store_performance`,
`subscriptions`, `reviews`, `sales`, `earnings`. `reports get` also takes
`--dimension` (`overview`, `country`, `device`, `os_version`, `app_version`,
`carrier`, `language` — the set varies by kind).

Facts worth carrying into an answer:

- **The exports run a few days behind.** The last 3–7 days of any window are
  normally absent. `installs`/`ratings` return them as `missing_days` with a
  `data_through` date — report the gap, never read a missing day as zero.
- If the command says no bucket is configured, ask the user to run
  `rollout config play set-reports-bucket <bucket>` with the
  `pubsite_prod_rev_…` name from Play Console → Download reports. **Never guess
  a bucket name.** A `403` afterwards usually means they signed in before
  setting it and need to run `rollout login play` again.
- `reports get` caps its JSON rows and reports `row_count`, the file's real
  size. For a whole month of a wide breakdown, pass `--out file.csv` and analyse
  the file with a script instead of pulling rows into context. `--out` only ever
  creates a file — pick a fresh path, and if one is in the way say so rather
  than replacing it.
- Several files can match one month (subscriptions have one per product id); the
  tool refuses to guess and lists them. Use `reports list`, then `--object`.

## Store listings

```bash
rollout play listing set --locale en-US --title "Example"
rollout play listing sync --dir metadata/android --images
```

`sync` reconciles a whole fastlane-layout directory: it previews a per-locale
plan, skips images whose SHA-256 already matches the store, and applies
everything in one edit — if any locale fails, none of it lands. Limits are title
30, short description 80, full description 4000 characters.

## Monetization

```bash
rollout play products --format table            # managed and one-time products
rollout play subscriptions --format table       # base plans and offers inlined
rollout play subscription get --id premium

rollout play product set --sku coins_100 --from-file product.json
rollout play subscription base-plan set-state --id premium --base-plan monthly --state inactive
rollout play subscription offer set-state --id premium --base-plan monthly --offer intro --state active
```

Things to get right here, because the API is unforgiving and the words are
misleading:

- **Deactivating does not cancel anybody.** It stops *new* subscribers. Everyone
  already on the base plan keeps their subscription and keeps being billed. Say
  this to the user before they confirm — it is the assumption they will have
  wrong. Deactivating a base plan takes **two** confirmations.
- **An offer is only live while its base plan is active**, so activating an
  offer under an inactive plan does nothing visible.
- **`product set` replaces the whole price map rather than merging it.** A
  region missing from the JSON file loses its price. The preview lists every
  add, change, and `REMOVED` region — read those lines out; they are the reason
  this write previews.
- Subscriptions are not in `products` and cannot be written with `product set`;
  they have their own commands. `rollout` says so if you try.
- Archiving a subscription is **not supported by Play's API** and has no command.
  Do not go looking for one.

Unlike a release, these writes are not staged in an edit — a confirmed one is
live immediately.

## Reviews

```bash
rollout play reviews --unanswered --max-stars 3
rollout play review reply --id <review-id> --text "…"
```

A reply is **public and immediate** — it is not staged in an edit like a
release. The preview quotes the review being answered; check it is the right one
before confirming. Replies are capped at 350 characters.

## Users and permissions

These act on the developer **account**, not on an app, so they need its numeric
id from the Play Console URL — `--developer-id`, `PLAY_DEVELOPER_ID`, or
`rollout config play set-developer-id`. If it is not configured, ask; never
guess it.

```bash
rollout play users --format table
rollout play user invite --email dev@example.com --apps com.example.app --app-permissions CAN_MANAGE_TRACK_APKS
rollout play user grant --email dev@example.com --package com.example.app --permissions CAN_MANAGE_TRACK_APKS
rollout play user revoke --email dev@example.com --package com.example.app
rollout play user remove --email dev@example.com
```

Play has two permission vocabularies that differ only by a `_GLOBAL` suffix:
account-wide ones go to `--permissions` on `user invite`, per-app ones to
`--app-permissions` there and to `--permissions` on `user grant`. Read the
preview: it lists the exact enums, and `user grant` **replaces** a user's
permissions on that app rather than adding to them.

`user revoke` and `user remove` take **two confirmations** — the API cannot say
afterwards what a removed grant held, so the preview is the only record. Copy it
somewhere before confirming.

## App-level utilities

```bash
rollout play internal-share --file app.aab       # install link, no track, no release
rollout play device-tiers create --file tiers.json
rollout play data-safety set --file data-safety.csv
```

`internal-share` hands back a link that installs the build — it ships to no
track, but the Publisher API cannot withdraw the link afterwards.
`data-safety set` has **no read counterpart**: Play offers no endpoint for the
current declaration, so it cannot be shown, diffed, or restored. Do not run it
unless the user has the CSV they mean to publish.

## Command reference

Every command below is `rollout play <command>`; the shared ones (`confirm`,
`audit`, `doctor`, `config`) are `rollout <command>`.

**Reads**: `apps`, `tracks`, `releases`, `artifacts`, `listing`, `images`,
`details`, `testers`, `countries`, `device-tiers`, `users`, `edit status`,
`products`, `subscriptions`, `subscription get`, `reviews`, `review get`,
`vitals`, `vitals summary`, `errors`, `error reports`, `anomalies`,
`reports list`, `reports get`, `reports installs`, `reports ratings`.

**Writes** (preview then confirm): `upload`, `release create`, `release update`,
`release halt`, `release resume`, `release complete`, `promote`, `testers set`,
`countries set`, `deobfuscation upload`, `listing set`, `listing delete`,
`listing sync`, `images upload`, `images delete`, `details set`,
`product set`, `subscription base-plan set-state`,
`subscription offer set-state`, `review reply`, `internal-share`,
`device-tiers create`, `data-safety set`, `user invite`, `user grant`,
`user revoke`, `user remove`.

Run `rollout play <command> --help` for the flags. Full documentation is in the
repository's `docs/play.md`, with the CSV exports in `docs/reports.md`.

## Guard rails

The user may have configured limits that refuse a write outright:
`blocked_operations`, `max_rollout_fraction` (a cap on how far a rollout may
go), and `production_lock` (every production write takes two confirmations).
These are re-checked at confirm time. If one refuses a command, report what it
said — do not work around it.
