# AGENTS.md

Google Play Console MCP server + CLI, in Go. Single binary named `rollout`, all
source in `package main` at the repo root (module `github.com/Limetric/rollout`).
The binary exposes two front-ends over one shared set of tool handlers:

- **CLI** — `rollout login play`, `rollout play tracks`, `rollout play releases`,
  `rollout play promote-release …` (for humans, scripts, CI, and the agent skill
  that drives it via shell).
- **MCP server** — `rollout mcp` serves the same tools over stdio for MCP hosts
  (Claude Desktop, Cursor, …), under platform-prefixed names (`play_tracks`).

Each tool is defined once: a typed `Args` struct + a pure
`func(ctx, *Client, Args) (Result, error)` handler. The CLI wires flags → handler;
the MCP front-end registers the same handler with `mcp.AddTool`, which derives the
JSON input schema from the `Args` struct by reflection.

## Platforms

Every app store lives in its own namespace: `rollout <platform> <command>` on
the CLI, `<platform>_<tool>` over MCP. **Google Play is the platform implemented
today**; App Store Connect is the obvious second, and the registry exists so it
is a `platform_*.go` away. Shared infrastructure — `login`, `doctor`, `config`,
`confirm`, `audit`, `mcp`, `version` — is unnamespaced but platform-aware
(`rollout login play`, `rollout doctor play`, `rollout config play set-package`).

A platform exposes only what its API has. Creating an app has no Play API at all
(Console only), so there is no `play_create_app` — a tool that answers
"unsupported" is worse than its absence, because an agent has to call it to find
out.

A platform is a `Platform` value (see `platform.go`) registered from a
package-level variable:

```go
var playPlatform = registerPlatform(&Platform{Name: "play", …})
```

It supplies its CLI subcommands, MCP registration, login command, config
section, and doctor checks. **Adding a platform must not require edits to the
shared config/auth/doctor/MCP plumbing** — if it does, the abstraction is in the
wrong place.

## Commands

**Go code must be formatted before every commit** — CI rejects unformatted code.

```bash
go fmt ./...                      # Format — run before commit
go mod tidy                       # Resolve/lock deps (run once after clone)
go build -o build/rollout .       # Build binary
go vet ./...                      # Lint
go tool staticcheck ./...         # Static analysis
go test ./... -count=1            # Unit tests (no network; uses httptest)

# Live smoke test against the real API (requires real credentials)
PLAY_SERVICE_ACCOUNT_FILE=key.json PLAY_PACKAGE_NAME=com.example.app \
go test -tags integration -count=1 -v ./...
```

## Layout

Core (platform-neutral):

- `main.go` — Cobra root command, subcommand wiring, `main()`.
- `platform.go` — the `Platform` struct + registry; the `login` parent command.
- `config.go` / `config_paths.go` — TOML loading, env overlay, path resolution.
- `auth.go` — token sources: service-account JWT and the OAuth user flow.
- `token_store.go` — per-platform refresh-token store and rotation write-back.
- `doctor.go` — `doctor` command, verdict classification, exit codes.
- `config_command.go` — `config path` / `config show` and the TOML writers.
- `cli_helpers.go` — `--format json|table|csv` rendering for read commands.
- `mcp.go` — `rollout mcp`; iterates platforms and namespaces their tools. A
  platform whose credentials don't resolve is skipped with a warning; the server
  fails only when nothing can be served.
- `safety.go` — the confirm-token flow: staging, TTL, double confirmation. It
  knows a write's *platform*, never its payload.
- `write_tool.go` — `WriteResult`, and the shared apply path over the
  `mutationApplier` interface each platform's client implements.
- `guards.go` — production lock, max rollout fraction, blocked-op list.
- `audit.go` / `confirm.go` — `rollout audit` and `rollout confirm <token>`.

Google Play provider:

- `platform_play.go` — the registered `Platform` value.
- `config_play.go` — `PlayConfig`: the `[play]` TOML table + `PLAY_*` env,
  credential modes, base-URL overrides, validation.
- `doctor_play.go` / `config_command_play.go` — Play's doctor probes and settings.
- `mcp_play.go` — every Play MCP tool registration.
- `client.go` — Android Publisher v3 **REST** client. No Google SDK.
- `edit.go` — edit sessions: `insert → mutate → validate → commit`, delete on
  any failure. An edit is owned by one call and never persisted across processes.
- `upload.go` — resumable uploads (bundles, APKs, images, deobfuscation files).
- `reporting.go` — Play Developer Reporting API v1beta1 (vitals, errors, anomalies).
- `login.go` / `login_wizard.go` — `rollout login play`, plus the loopback OAuth
  server the user flow captures its authorization code on.
- `write_play.go` — Play's dispatch routes and preview/apply helpers.
- `tool_*.go` — one file per tool (`Args` + handler + CLI subcommand). Test lives
  next to it.

## Conventions (match these)

- All code is `package main` at the repo root. No `cmd/` or `internal/`.
- New tool = new `tool_<name>.go` + `tool_<name>_test.go`. Register it in **two**
  places: the platform's `Commands` (CLI) and its MCP registration function —
  `playPlatform` in `platform_play.go` and `registerPlayTools` in `mcp_play.go`.
  Keep the two in sync; `mcp_test.go` asserts they match.
- MCP tool names are written unprefixed at the registration site; the registrar
  applies the platform prefix. Never hand-write `play_` into a tool name.
- Write/mutating tools MUST go through `safety.go`: return a preview + confirm
  token first, execute only on `--confirm <token>`. Never mutate on the first call.
- **Edits are transactional and never persisted.** A read that needs an edit
  opens one and deletes it in a `defer`; a write applies as
  `insert → mutate → validate → commit` inside one call, and deletes the edit on
  any failure. A staged confirm token describes the *intent*, never an edit ID —
  edits expire, and a token may be confirmed from another process.
- **Never clobber a track.** A release write fetches the track inside the edit
  and mutates one release; writing the whole `releases[]` array drops in-progress
  rollouts.
- Refresh tokens live in the token store (`token_store.go`), never in config
  TOML or an env var.
- A confirmed write is applied through the platform's `mutationApplier`, and the
  staged record names the platform — that is how `rollout confirm <token>` routes
  a token back to the API that staged it. New platform ⇒ set `Platform.NewApplier`.
- Partial success is not success. A multi-locale listing sync that fails on one
  locale must abort the edit (nothing commits) and name the locale.
- Errors: wrap with `%w`, and make messages actionable (tell the user the fix).
- Tests are table-driven and offline — use `net/http/httptest` to fake the Play
  API (set `PLAY_API_BASE_URL` / `PLAY_REPORTING_BASE_URL` to the test server; a
  loopback base URL is what puts a config in test mode). `//go:build integration`
  for live tests.

## Setup facts the tools rely on

- Enable **Google Play Android Developer API** and **Google Play Developer
  Reporting API** in the Cloud project; invite the service account in Play
  Console → Users & permissions with per-app permissions. Writes need *Release to
  production / testing tracks*; vitals needs *View app information (read-only)* —
  Release Manager alone does not grant Reporting access.
- Quotas: Publisher API 200k requests/day, Reporting API 10 QPS. Reads that open
  edits count against it; keep one edit per call.
- Testers via API are Google Groups only; reviews are returned only for the last
  week and only for apps with a live production release; release notes ≤ 500
  chars; listing title 30 / short description 80 / full description 4000 chars.

## Key references

- User-facing docs: `README.md` (shared concepts), `docs/play.md` (setup, login,
  tool coverage), `docs/name-map.md` (CLI ↔ MCP name map).
- Android Publisher API v3: <https://developers.google.com/android-publisher/api-ref/rest>
- Play Developer Reporting API v1beta1: <https://developers.google.com/play/developer/reporting>
- MCP Go SDK: <https://github.com/modelcontextprotocol/go-sdk>
