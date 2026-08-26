# rollout

A Google Play Console **CLI** and **MCP server** in one Go binary.

`rollout` manages Android releases the way a release engineer actually works —
upload an artifact, stage it to a track, watch the rollout, halt or complete it —
and exposes exactly the same operations to an AI agent over MCP. Store listings,
reviews, and Android vitals come along for the ride.

Every write is previewed and confirmed before it executes.

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

## Platforms

| Platform | Namespace | Docs |
| --- | --- | --- |
| Google Play | `rollout play …` / `play_…` | [docs/play.md](docs/play.md) |

The platform registry exists so a second store (App Store Connect is the obvious
one) is a new file rather than an edit to the shared auth, config, and safety
plumbing.

## Install

```bash
go build -o build/rollout .
```

Release binaries and a Homebrew tap arrive with v0.1.0:

```bash
brew install Limetric/tap/rollout
```

## Quick start

```bash
rollout login play                              # guided setup, ends with a live check
rollout config play set-package com.example.app # so --package can be omitted
rollout doctor play                             # opens and deletes a real edit
rollout play tracks
```

For CI, skip the wizard entirely:

```bash
export PLAY_SERVICE_ACCOUNT_FILE=/path/to/key.json
export PLAY_PACKAGE_NAME=com.example.app
rollout doctor play
```

The one step that is easy to miss: the credential must be invited in Play
Console → **Users & permissions**. Authenticating is not the same as having
access to an app. See [docs/play.md](docs/play.md#prerequisites).

## MCP hosts

Point your host at the binary and pass credentials through the environment — no
config file needed:

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

Tools appear namespaced: `play_tracks`, `play_releases`, `play_reviews`. A
platform whose credentials do not resolve is skipped with a note in the server
log rather than taking the whole server down, so an unconfigured second store
never blocks the first.

## Namespaces

Google Play is `rollout play …` on the CLI and `play_…` over MCP. Shared
infrastructure is unnamespaced but takes a platform argument:

| Command | What it does |
| --- | --- |
| `rollout doctor [platform]` | Check that credentials resolve and the API answers |
| `rollout login play` | Sign in and save credentials |
| `rollout config show` | Print the resolved configuration, secrets redacted |
| `rollout confirm <token>` | Apply a previewed write |
| `rollout audit` | Show every write rollout has applied |
| `rollout mcp` | Serve the tools over stdio |
| `rollout version` | Print the version |

## Credentials

Two modes, and a service-account key wins when both are configured:

- **Service-account JSON key** — headless, what Google recommends for the
  Publisher API, and what CI should use.
- **OAuth user sign-in** — `rollout login play` runs the loopback
  authorization-code flow with PKCE against your own Console account.

Refresh tokens live in a per-user token store (0600, under the config
directory), never in `config.toml` and never in an environment variable.
`ROLLOUT_TOKEN_STORE` relocates it for containers and CI.

## Output

Reads print JSON by default, so they pipe into `jq`. `--format table` and
`--format csv` are there when a person or a spreadsheet is reading.

```bash
rollout play errors --days 7 --format table
rollout play vitals --metric crashrate --dimension versionCode | jq '.rows'
```

## Safety

No mutating call executes on first request. A write tool returns a preview and a
confirm token valid for 10 minutes; you apply it with `--confirm <token>` or
`rollout confirm <token>`. Every applied write — and every failed apply — is
appended to an audit log you can read with `rollout audit`.

Writes are transactional. A release write opens a fresh edit, mutates one
release, validates, and commits; anything that fails along the way deletes the
edit, so nothing is left half-staged. A release write also reads the track
first and touches only the release it names — an in-progress rollout is never
dropped by a write that did not mention it.

Two confirmations are required for the operations that cannot be undone by
running the opposite command: halting a rollout, completing a production
rollout, and deleting a listing or a whole image type.

### Guard rails

Optional, and off by default. Set them in the environment or in `config.toml`:

```toml
[play.safety]
production_lock = true                 # PLAY_PRODUCTION_LOCK=1
max_rollout_fraction = 0.2             # PLAY_MAX_ROLLOUT_FRACTION=0.2
blocked_operations = ["halt_release"]  # PLAY_BLOCKED_OPS=halt_release
```

| Setting | Effect |
| --- | --- |
| `production_lock` | Every write touching the `production` track takes a second confirmation |
| `max_rollout_fraction` | Refuses a write past this staged-rollout fraction — `0.2` lets CI stage and grow a rollout but never finish one |
| `blocked_operations` | Refuses these tools outright |

They are re-checked when a token is confirmed, not only when it is issued, so
tightening one always wins over a preview that is already in flight.

## Documentation

- [docs/play.md](docs/play.md) — setup, tool coverage, release semantics, troubleshooting
- [docs/reporting.md](docs/reporting.md) — vitals metric sets, dimensions, thresholds
- [docs/name-map.md](docs/name-map.md) — CLI ↔ MCP name map

## Building from source

```bash
go build -o build/rollout .
go test ./... -count=1
go vet ./... && go tool staticcheck ./...
```

Contributor conventions are in [AGENTS.md](AGENTS.md).

## License

Apache-2.0.
