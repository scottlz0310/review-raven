# Usage

[日本語](usage.ja.md)

This guide covers the basic setup needed to run `review-raven` as an MCP server:

- Architecture overview (mcp-gateway required)
- Docker start, stop, and logs
- Connecting MCP clients via mcp-gateway
- Review response skill installation via Mcp-Docker

For the tool-level flow, see [watch-tools.md](watch-tools.md). For skill installation, see section 4 below and the [skill location guide (Japanese)](skills/README.md).

> **mcp-gateway required**: Standalone OAuth is not supported. All traffic must pass through mcp-gateway. Migrating from `copilot-review-mcp`? See [architecture.md — Migration / Compatibility](architecture.md#migration--compatibility).

## Architecture

```
MCP Client (Claude Code / Claude Desktop / VS Code)
    │
    │  HTTPS / OAuth  (handled by mcp-gateway)
    ▼
mcp-gateway  ──►  X-Authenticated-User + Authorization headers
    │
    │  HTTP (internal only)
    ▼
review-raven  :8083
    │
    │  SQLite
    ▼
/data/review-raven.db
```

`review-raven` trusts the headers injected by mcp-gateway and never performs OAuth directly.

## 1. Set up mcp-gateway

Follow the [mcp-gateway documentation](https://github.com/mcp-b/mcp-gateway) to deploy and configure the gateway.

Point one of its upstream routes at the address reachable **from the gateway** (e.g., `http://review-raven:8083` when both run on the same Docker network, or `http://host.docker.internal:8083` on Docker Desktop).

## 2. Run review-raven with Docker

### Pull the published image

```bash
docker pull ghcr.io/scottlz0310/review-raven:latest
```

### Build locally

```bash
docker build -t review-raven:dev .
```

### Start the container

Published image:

```bash
docker run -d --name review-raven \
  -p 127.0.0.1:8083:8083 \
  -e BIND_ADDR=0.0.0.0 \
  -v review-raven-data:/data \
  ghcr.io/scottlz0310/review-raven:latest
```

Local image:

```bash
docker run -d --name review-raven \
  -p 127.0.0.1:8083:8083 \
  -e BIND_ADDR=0.0.0.0 \
  -v review-raven-data:/data \
  review-raven:dev
```

Optional environment variables (all have defaults):

```env
MCP_PORT=8083
BIND_ADDR=127.0.0.1   # Use 0.0.0.0 in Docker so mcp-gateway (other container) can reach this server
LOG_LEVEL=info
SQLITE_PATH=/data/review-raven.db
IN_PROGRESS_THRESHOLD_SEC=30
```

`TRUSTED_COMMENT_AUTHORS` has no default. It is the comma-separated list of GitHub logins whose PR comments may enter an agent's context, and `get_trusted_comment_authors` publishes it. Without it, that tool fails closed with `TRUSTED_AUTHORS_NOT_CONFIGURED`:

```env
TRUSTED_COMMENT_AUTHORS=your-login,thread-owl,codecov
```

### Review requests (opt-in)

`request_review`, `get_review_request`, `cancel_review_request` and the `review-raven://requests/{cycleId}` resource are published only when both settings below are present **and** the server runs in the dedicated GitHub App mode. Enabling them without the App mode or without an access list stops startup; the existing tool surface does not change otherwise.

```env
REVIEW_RAVEN_REVIEW_REQUESTS=true
REVIEW_RAVEN_REQUEST_ACL={"users":{"your-login":["your-org/*"]},"operators":["your-login"]}
```

- `users` lists the repositories (`owner/repo`, or `owner/*`) a login may request and read. A principal may cancel its own requests; `operators` may also cancel other principals' requests.
- Every input carries the contract version `schemaVersion: 1` (required; any other value is `INVALID_ARGUMENT`).
- A request is bound to `expectedHeadSha`; a different current head is `HEAD_CONFLICT`. The same `idempotencyKey` and payload return the same cycle. `dispatch: pending` means "not delivered yet", not "queued".
- Subscribe to the returned `resourceUri` for progress. `terminal` becomes true only for a result reference, `superseded` or `cancelled` (see `terminalReason`); a process exit or a delivery failure alone never makes it terminal.
- Failures are returned as the contract error envelope (`code`, `retryable`). Authorization failures are never bypassed automatically, and a pull request outside the installation is reported as `PERMISSION_DENIED`.
- The wire contract is in [docs/design/contracts](design/contracts/README.md). Delivery to the reviewer side and execution observations are not part of this change yet: a request stays `dispatch: pending`.

### Internal surface for thread-owl and Squirrel (opt-in)

The reviewer side reports back over a **separate MCP server** on `/internal/mcp`. Its tools (`ingest_review_candidate`, `publish_review_result`, `publish_review_observation`) never appear in the agent-facing tool list, and the route has its own proxy secret.

```env
REVIEW_RAVEN_INTERNAL_SURFACE=true
REVIEW_RAVEN_INTERNAL_PROXY_SECRET=<32+ characters, different from REVIEW_RAVEN_PROXY_SECRET>
REVIEW_RAVEN_SERVICE_ACL={"services":{"svc-owl":{"actions":["ingest","publish_result"],"repos":["your-org/*"]},"svc-squirrel":{"actions":["observe"],"repos":["your-org/*"],"producers":["squirrel-notifier"]}}}
```

- Requires the review-request tools (`REVIEW_RAVEN_REVIEW_REQUESTS=true`). Startup fails when the ledger, the ACL, the secret, or a secret different from the public one is missing.
- Authorization is per service principal, operation and repository. A service may call only the operations listed for it; a service that may `observe` may publish only as one of its listed `producers`. A name cannot appear both as a user and as a service, and a user of the public surface cannot call the internal tools.
- `publish_review_result` stores only a reference to the reviewer's GitHub post. Results for a cycle that was superseded are kept on the old cycle; a result for a cancelled cycle is `CANDIDATE_CANCELLED`; a late delivery for a request removed after its retention is `DELIVERY_EXPIRED`.
- Observations are accepted only for the candidate the request was dispatched to, and are ordered by `producerEpoch` and `sequence`. Delivery of candidates from raven to thread-owl (the outbox worker) is a separate change: until then a request stays `dispatch: pending` and observations are rejected with `PRODUCER_NOT_ASSIGNED`.

### Check health

```bash
curl http://127.0.0.1:8083/health
```

PowerShell:

```powershell
Invoke-RestMethod http://127.0.0.1:8083/health
```

Expected response:

```json
{"status":"ok"}
```

### View logs

```bash
docker logs -f review-raven
```

### Stop, start again, and remove

```bash
docker stop review-raven
docker start review-raven
docker rm -f review-raven
```

The named volume keeps the SQLite watch-state database:

```bash
docker volume ls --filter name=review-raven-data
```

Remove it only when you intentionally want to delete local state:

```bash
docker volume rm review-raven-data
```

## 3. Configure an MCP client

### Streamable HTTP clients (Claude Code, VS Code)

Point the client at your mcp-gateway URL:

```json
{
  "mcpServers": {
    "review-raven": {
      "type": "http",
      "url": "https://your-gateway-url/mcp"
    }
  }
}
```

Some clients use `servers` instead of `mcpServers`, or `streamable-http` instead of `http`. Keep the URL unchanged.

### stdio clients (Claude Desktop, etc.) via mcp-remote

Use [mcp-remote](https://github.com/geelen/mcp-remote) as a bridge:

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

When the client first connects, mcp-gateway handles the OAuth authorization flow. Sign in with GitHub.

## 4. Install the review response skill

The canonical `review-raven-thread-owl-cycle` skill is [Mcp-Docker's SKILL.md (Japanese)](https://github.com/scottlz0310/Mcp-Docker/blob/main/skills/review-raven-thread-owl-cycle/SKILL.md). Mcp-Docker manages its source and installation.

Install v2.18.0 or later from [Mcp-Docker releases](https://github.com/scottlz0310/Mcp-Docker/releases), which provides the skill subcommand, then run:

```shell
mcp-docker skill install
mcp-docker skill status
```

Supported clients are Claude, Copilot, Codex, and Antigravity CLI. See the [Mcp-Docker guide (Japanese)](https://github.com/scottlz0310/Mcp-Docker#readme) for filtering targets and updating skills. Manually installed copies are treated as unmanaged and require confirmation before overwriting.

Skill templates in this repository have been retired. `review-raven-thread-owl-cycle` now uses the Japanese canonical source only; its English version and both language versions of the unused Copilot-only `pr-review-cycle` have been removed. For the Copilot MCP tools, see the [tool documentation](watch-tools.md).

## 5. Basic review response workflow

After thread-owl posts review comments, ask the implementation agent to address them, specifying the target PR:

```text
$review-raven-thread-owl-cycle owner/repo#123
```

Follow the [canonical skill (Japanese)](https://github.com/scottlz0310/Mcp-Docker/blob/main/skills/review-raven-thread-owl-cycle/SKILL.md) for required connections, fixes, replies, and re-review requests. Starting an independent reviewer and merging are separate operations; merging requires explicit user approval.

## Troubleshooting

### `missing_proxy_identity` (401)

The request reached `review-raven` without going through mcp-gateway, or the gateway is not configured to inject `X-Authenticated-User`. Ensure all traffic passes through mcp-gateway.

### `session_user_mismatch`

The same MCP session ID was reused with a different GitHub login. Clear the MCP client's cached session or reconnect.

### Container starts but `/health` fails

Check logs:

```bash
docker logs review-raven
```

Common causes are a port already in use or a bad `SQLITE_PATH`.
