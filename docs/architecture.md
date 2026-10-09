# Architecture

[日本語](architecture.ja.md)

## Position of review-raven

`review-raven` is the **reviewed-side** MCP server in the review infrastructure. It provides operations for the side that *receives* a review and works on fixes.

This repository does not implement reviewer-side operations (review posting, webhook reception, queue management). Those belong to [thread-owl](https://github.com/scottlz0310/thread-owl).

## Five-component review infrastructure

| # | Repository | Role | Responsibility |
|---|-----------|------|----------------|
| 1 | **[thread-owl](https://github.com/scottlz0310/thread-owl)** | reviewer side | GitHub App bot. Posts reviews and in-line comments from a consistent account |
| 2 | **review-raven** (this repo) | reviewed side | MCP server. Thread read, reply, resolve/unresolve, re-review request |
| 3 | **[mcp-resource-subscriber](https://github.com/scottlz0310/mcp-resource-subscriber)** | status subscription bridge | Called from both review-raven and thread-owl via skill. Subscribes to `resources/updated` notifications |
| 4 | **[mcp-gateway](https://github.com/scottlz0310/mcp-gateway)** | MCP reverse proxy | Routing and authentication boundary for MCP servers |
| 5 | **[Mcp-Docker](https://github.com/scottlz0310/Mcp-Docker)** | container orchestration | Container management, gateway route generation, CLI agent config automation |

## Responsibility of review-raven

- Read PR review threads
- Extract and classify unresolved threads
- Reply to review comments
- Resolve / unresolve review threads
- Request re-review after fixes are applied
- Handle any review thread regardless of provider (Copilot review, human reviewer, bot reviewer)

Copilot review is one acquisition provider, not the permanent core concept of this repository.

## Boundary with thread-owl

| review-raven | thread-owl |
|---|---|
| Reviewed-side operations | Reviewer-side operations |
| Thread read / reply / resolve | Review posting |
| Re-review request | Webhook reception |
| — | Review candidate queue |
| — | `queue://review/queue` MCP resource |

Do not add reviewer-side queue / webhook / GitHub App review posting to this repository. Do not add reviewed-side thread resolve / re-review request workflows to thread-owl.

## Boundary with mcp-resource-subscriber

`mcp-resource-subscriber` is an external CLI / bridge that connects MCP resource subscription to agent workflows. review-raven does not embed a long-lived subscription client or watcher CLI. When needed, agent skills call `mcp-resource-subscriber` externally.

## MCP server details

専用Appモードでは、gatewayの利用者認証とGitHub APIの資格情報を分離する。review-raven自身が専用Appのinstallation tokenを発行し、reviewed用6 toolだけを公開する。既定のgatewayモードは従来の資格情報委譲・Copilot・watchを維持する。[設定・信頼境界・移行手順](github-app-auth.md)を参照。

- **Server name**: `review-raven`
- **MCP client key**: `review-raven` (tool prefix: `mcp__review-raven__*`)
- **Resource URI scheme**: `review-raven://watch/{watch_id}`
- **Auth**: gateway-delegated via mcp-gateway (`X-Authenticated-User` + `Authorization` headers)

## Migration / Compatibility

### Resource URI scheme

Watch resources use the `review-raven://watch/{id}` scheme. The legacy `copilot-review://watch/{id}` scheme (used before the rename) is **no longer accepted**. Re-request any active watches after upgrading from `copilot-review-mcp`.

### Environment variables

`REVIEW_RAVEN_GATEWAY_INTERNAL_URL` and `REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET` are the only supported names. The former `COPILOT_REVIEW_GATEWAY_INTERNAL_URL/SECRET` names are **no longer read**.

### MCP tool names

Tool names (`request_copilot_review`, `start_copilot_review_watch`, etc.) are unchanged for public API compatibility.

## Re-review request flow

After completing fixes, review-raven posts `@thread-owl re-review requested` on the PR via `add_issue_comment`. This is the boundary between the reviewed-side cycle and the reviewer-side cycle.

| Component | Responsibility |
|---|---|
| **review-raven** (reviewed side) | Posts `@thread-owl re-review requested` comment after fix → reply → resolve. This ends the reviewed-side cycle. |
| **thread-owl** (reviewer side) | Detects the `@thread-owl` mention via `issue_comment.created` webhook → enqueues as `ReviewCandidate.reason = "re-review-requested"` → notifies `queue://review/queue` resource. |
| **mcp-resource-subscriber** | Subscribes to the queue resource and bridges the update to the agent workflow. |

review-raven does not own or manage the review queue. The `@thread-owl` comment is the handoff point. After posting it, the reviewed-side cycle is complete; the next reviewer-side cycle starts from thread-owl's queue.

This approach uses GitHub's standard PR conversation rather than a Copilot-specific API, so it works with any reviewer identity (Copilot, human, bot).

## Relation to pr-review-subscribe skill

The `pr-review-subscribe` skill is the upper-level workflow integrating review acquisition, thread handling, and fix workflow. review-raven serves as the reviewed-side MCP provider within that skill.

## Future direction

As [github-mcp-server](https://github.com/github/github-mcp-server) matures, some MCP tools currently provided by review-raven may become redundant. If github-mcp-server covers the full set, review-raven may eventually become a skill-only repository. New MCP tools should be evaluated against github-mcp-server coverage before being added here.

## Related issues

- [review-raven #63](https://github.com/scottlz0310/review-raven/issues/63) — Responsibility boundary definition (this document)
- [thread-owl #75](https://github.com/scottlz0310/thread-owl/issues/75) — Thread Owl responsibility boundary
- [mcp-resource-subscriber #86](https://github.com/scottlz0310/mcp-resource-subscriber/issues/86) — `--json` output mode
- [mcp-gateway #92](https://github.com/scottlz0310/mcp-gateway/issues/92) — MCP reverse proxy / auth boundary
- [Mcp-Docker #158](https://github.com/scottlz0310/Mcp-Docker/issues/158) — Container orchestration
