# rollout

A Google Play Console **CLI** and **MCP server** in one Go binary.

`rollout` manages Android releases the way a release engineer actually works —
upload an artifact, stage it to a track, watch the rollout, halt or complete it —
and exposes exactly the same operations to an AI agent over MCP. Store listings,
reviews, and vitals come along for the ride.

Every write is previewed and confirmed before it executes.

```bash
rollout play tracks                      # CLI
```

```json
{ "mcpServers": { "rollout": { "command": "rollout", "args": ["mcp"] } } }
```

## Install

```bash
go build -o build/rollout .
```

Homebrew and release binaries arrive with v0.1.0.

## Quick start

```bash
export PLAY_SERVICE_ACCOUNT_FILE=/path/to/key.json
export PLAY_PACKAGE_NAME=com.example.app
rollout doctor play
```

Or sign in with your own Play Console account:

```bash
rollout login play
```

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

## Safety

No mutating call executes on first request. A write tool returns a preview and a
confirm token valid for 10 minutes; you apply it with `--confirm <token>` or
`rollout confirm <token>`. Halting or completing a production rollout, and any
deletion, takes a second confirmation. Every applied write — and every failed
apply — is appended to an audit log.

See `docs/play.md` for setup and the full tool list, and `docs/name-map.md` for
the CLI ↔ MCP name map.

## License

Apache-2.0.
