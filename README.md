# rollout

**Ship an Android release without opening Play Console.** `rollout` is the CLI
and MCP server that lets you (or your favorite AI agent) push a build to a
track, watch a staged rollout climb, halt it if things look wrong, and answer
"why did our crash rate spike" — all from a terminal instead of a maze of
dashboard tabs. Store listings and user reviews come along for the ride. It
talks straight to the Android Publisher and Play Developer Reporting APIs from
your own machine — no relay server sitting between you and your app's data.

It ships as a single binary with two front-ends over one shared set of tools:

- **CLI** — `rollout play releases`, `rollout play tracks`, `rollout play
  promote …`. Scriptable, pipeable into `jq`, usable in CI. This is what the
  bundled agent **skill** drives.
- **MCP server** — `rollout mcp` serves the same tools over stdio to MCP hosts
  (Claude Desktop, Cursor, …), so an agent can manage your release the same
  careful way you would.

```bash
rollout play releases --format table
```

```
track       | status     | version_codes | rollout_percent | name
------------+------------+---------------+-----------------+------
production  | completed  | ["41"]        |                 | 1.4.0
production  | inProgress | ["42"]        | 10              | 1.5.0
beta        | completed  | ["43"]        |                 | 1.6.0
```

Nothing mutates on the first ask. Every write — halting a rollout, promoting a
build, editing a listing — comes back as a preview and a confirm token first.
You (or the agent) apply it on purpose, or it expires.

## Platforms

| Platform | Setup guide | Surface |
| --- | --- | --- |
| Google Play | [`docs/play.md`](docs/play.md) | 35 tools — tracks, releases, listings, reviews, and Android vitals |

The platform registry exists so a second store (App Store Connect is the
obvious one) is a new file rather than an edit to the shared auth, config, and
safety plumbing.

## Install

Three ways in, no wrong answer:

- **Homebrew** (macOS/Linux):

  ```bash
  brew install Limetric/tap/rollout
  ```

- **Prebuilt binary** — grab one from the
  [releases page](https://github.com/Limetric/rollout/releases/latest).

- **Build from source**:

  ```bash
  go build -o build/rollout .
  ```

## Quick start

```bash
rollout login play                              # guided setup + live check
rollout config play set-package com.example.app # so --package can be omitted
rollout play tracks
```

CI skips the wizard:

```bash
export PLAY_SERVICE_ACCOUNT_FILE=/path/to/key.json
export PLAY_PACKAGE_NAME=com.example.app
rollout doctor play
```

Don't forget to invite the credential in Play Console → **Users & permissions**
— authenticating isn't the same as having access. Details in
[`docs/play.md`](docs/play.md#prerequisites).

## Integrations

### As an MCP server

`rollout mcp` serves the same tools over stdio, under a platform prefix —
`play_tracks`, `play_releases`, `play_reviews`, … Point your host at the
binary and pass credentials through the environment, no config file needed:

```json
{
  "mcpServers": {
    "rollout": {
      "command": "rollout",
      "args": ["mcp"],
      "env": {
        "PLAY_SERVICE_ACCOUNT_FILE": "/path/to/key.json",
        "PLAY_PACKAGE_NAME": "com.example.app"
      }
    }
  }
}
```

A platform whose credentials don't resolve is skipped with a note on stderr
rather than taking the whole server down, so an unconfigured second store
never blocks the first.

### As a Claude Code plugin

The repo bundles a skill (`plugins/rollout/skills/rollout/SKILL.md`) that
teaches an agent when and how to drive the CLI. If you installed `rollout` via
Homebrew and don't have the repo cloned, install it as a plugin instead:

```text
/plugin marketplace add Limetric/rollout
/plugin install rollout@rollout
```

## Concepts

### Writes preview first

Every mutating call returns a preview and a confirm token before it touches
anything:

```bash
rollout play release halt --track production
# → preview + confirm token, valid for 10 minutes
rollout confirm <token>       # applies it (or re-run with --confirm <token>)
rollout audit                 # log of every write rollout has applied
```

Writes are transactional — a release write opens a fresh edit, mutates one
release, validates, and commits, deleting the edit on any failure, so nothing
is left half-staged. It also reads the track first and touches only the
release it names, so an in-progress rollout is never dropped by a write that
didn't mention it. Halting a rollout, completing a production rollout, and
deleting a listing or image type ask for a second confirmation, since none of
those undo with the opposite command.

Guard rails are optional and off by default — a production lock, a rollout
percentage ceiling, a blocked-operations list — and they're re-checked at
confirm time, not just when the preview is issued. See
[`docs/play.md`](docs/play.md#guard-rails) for the full list.

### Where the refresh token lives

`rollout login play` writes the refresh token to a per-platform **token
store** (`0600`, under the config directory), never to `config.toml` and
never to an environment variable:

```bash
rollout doctor play                     # store location, writability, sign-in age
export ROLLOUT_TOKEN_STORE=/path/to/tokens   # containers/CI: mount a writable volume
```

A service-account JSON key skips the token store entirely and wins when both
credential modes are configured — it's what CI and Google's own docs
recommend for the Publisher API.

### Defaults and output

```bash
rollout config play set-package com.example.app   # drop --package everywhere else
rollout play releases --format table               # or --format csv for a spreadsheet
rollout play vitals --metric crashrate --dimension versionCode | jq '.rows'
```

Reads print JSON by default, so they pipe straight into `jq`. Human-facing
output — `login`, `doctor`, `config show`, table headers, write previews — is
colored on a terminal and plain everywhere else, so scripts and `rollout
audit` stay byte-identical either way. Override with `--color always|never|auto`;
`NO_COLOR` and `TERM=dumb` are honored too.

## Documentation

- [`docs/play.md`](docs/play.md) — setup, tool coverage, release semantics, troubleshooting
- [`docs/reporting.md`](docs/reporting.md) — vitals metric sets, dimensions, thresholds
- [`docs/name-map.md`](docs/name-map.md) — CLI ↔ MCP name map

See [`AGENTS.md`](AGENTS.md) for the contributor workflow and conventions.

## License

Apache-2.0. See [`LICENSE`](LICENSE).
