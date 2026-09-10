# review-raven

<p align="center">
  <img src="assets/review_raven_avatar.png" alt="review-raven" width="150">
</p>

[日本語](README.ja.md)

An MCP (Model Context Protocol) server for the **reviewed side** of a PR review workflow. Reads, replies to, and resolves review threads regardless of review provider (Copilot, human reviewer, or bot). Re-review requests are routed through the configured acquisition provider. Built on an **async watch + notification** model designed for LLM agents.

> **mcp-gateway required**: This server must be deployed behind **[mcp-gateway](https://github.com/mcp-b/mcp-gateway)**, which handles authentication and injects `X-Authenticated-User` + `Authorization` headers. Standalone OAuth is not supported. Migrating from `copilot-review-mcp`? See [docs/architecture.md — Migration / Compatibility](docs/architecture.md#migration--compatibility).

## Features

- **Async watch + notification** based. Start a background watch with `start_copilot_review_watch`, then track progress via the cheap `get_copilot_review_watch_status` read and `notifications/resources/updated` events.
- **GraphQL-based Copilot review request**. Avoids the issue where REST `requested_reviewers` silently ignores bot actors.
- **Per-thread review operations**. Reply, resolve, or reply+resolve individual threads using `PRRT_xxx` node IDs.
- **mcp-gateway integration** for authentication. The gateway handles OAuth and injects verified identity headers.
- **Stateless Streamable HTTP** on MCP protocol `2026-07-28`, and on that revision only. No `Mcp-Session-Id` is issued or read; every request is authorized on its own from the GitHub token injected by mcp-gateway. The deprecated `initialize` handshake is refused with JSON-RPC `-32022` (`Unsupported protocol version`), so a legacy client cannot negotiate down — clients discover the server through `server/discover`.
- **SQLite-persisted watch state**. Active watches that survive a process restart are observable as `STALE`.

## Tools

| Tool | Description |
|---|---|
| `request_copilot_review` | Request a Copilot review on a PR |
| `get_copilot_review_status` | Fetch an instant snapshot from GitHub |
| `start_copilot_review_watch` | Start a background watch (recommended entry point) |
| `get_copilot_review_watch_status` | Cheap read of the current watch state |
| `list_copilot_review_watches` | List your active/recent watches |
| `cancel_copilot_review_watch` | Stop a watch |
| `get_pr_review_cycle_status` | Overall review cycle status and next-action recommendation |
| `get_review_threads` | List review threads (raw data; classification is left to the calling LLM) |
| `reply_to_review_thread` | Post a reply to a thread |
| `resolve_review_thread` | Mark a thread as resolved |
| `reply_and_resolve_review_thread` | Reply then resolve in sequence |
| `wait_for_copilot_review` | Legacy blocking wait (fallback) |
| `diagnose_github_token` | Report the current token's login and OAuth scopes (from the `X-OAuth-Scopes` response header) for diagnosing `PERMISSION_DENIED` failures |

`get_review_threads` accepts the optional `include_bodies` input. It defaults to `true` for compatibility; set it to `false` to receive a metadata-only projection that does not select review comment bodies from GitHub. The response includes comment IDs, author metadata, URLs, thread resolution state, and pagination completion information.

See [docs/usage.md](docs/usage.md) for setup and operation. Tool-level details are in [docs/watch-tools.md](docs/watch-tools.md); skill location and installation are described in the [skill guide (Japanese)](docs/skills/README.md). For the architecture and responsibility boundaries with Thread Owl and mcp-resource-subscriber, see [docs/architecture.md](docs/architecture.md).

## Quick Start (Docker + mcp-gateway)

This server requires [mcp-gateway](https://github.com/mcp-b/mcp-gateway) to handle authentication.

```bash
# Start review-raven (internal, not exposed directly)
docker run --rm -p 127.0.0.1:8083:8083 \
  -e BIND_ADDR=0.0.0.0 \
  -v review-raven-data:/data \
  ghcr.io/scottlz0310/review-raven:latest
```

Configure mcp-gateway to proxy the internal address of this server as seen **from the gateway** (e.g., `http://review-raven:8083` on a shared Docker network, or `http://host.docker.internal:8083` on Docker Desktop). See [mcp-gateway docs](https://github.com/mcp-b/mcp-gateway).

**For stdio clients** (Claude Desktop, etc.) use [mcp-remote](https://github.com/geelen/mcp-remote):

```json
{
  "mcpServers": {
    "review-raven": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "https://your-gateway-url/mcp"]
    }
  }
}
```

See [docs/usage.md](docs/usage.md) for the full setup guide.

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `MCP_PORT` | | `8083` | Listen port |
| `BIND_ADDR` | | `127.0.0.1` | Bind address. Use `0.0.0.0` in Docker so the container is reachable from mcp-gateway on the same network |
| `LOG_LEVEL` | | `info` | `debug` / `info` / `warn` / `error` |
| `SQLITE_PATH` | | `/data/review-raven.db` | Path to the watch-state database |
| `IN_PROGRESS_THRESHOLD_SEC` | | `30` | Grace period after a review request before treating the review as in-progress (seconds) |
| `REVIEW_RAVEN_GATEWAY_INTERNAL_URL` | | _(unset)_ | **Phase B** — Full URL of the mcp-gateway internal whoami endpoint (e.g. `http://127.0.0.1:8080/internal/v1/whoami`). Must be a loopback address. Set together with `REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET` or leave both unset. |
| `REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET` | | _(unset)_ | **Phase B** — Shared bearer secret for the gateway internal API. Must be set together with `REVIEW_RAVEN_GATEWAY_INTERNAL_URL`. |

**Not supported** (removed in pre-rename `copilot-review-mcp` lineage): `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `BASE_URL`, `GITHUB_OAUTH_SCOPES`, `SESSION_TTL_MIN`, `TOKEN_CACHE_TTL_MIN`, `TOKEN_EXPIRES_IN_SEC`, `AUTH_MODE`.

## Local Development

Requires Go 1.27+.

```bash
# Run tests
go test ./...

# Build
go build -o bin/review-raven ./cmd/server

# Build Docker image
docker build -t review-raven:dev .
```

## History

This repository is a split-out of `services/review-raven/` from [scottlz0310/Mcp-Docker](https://github.com/scottlz0310/Mcp-Docker). Git history was not migrated. Related PRs and Issues from Mcp-Docker (`#47`, `#52`, `#53`, `#55`–`#58`, `#62`, `#63`–`#68`, `#74`–`#77`, `#92`, etc.) are referenced in the documents under `docs/`.

## License

MIT License — see [LICENSE](LICENSE).
