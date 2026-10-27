# Architecture

[日本語](architecture.ja.md)

## Position of review-raven

review-raven is the MCP server for the side that requests reviews and addresses their findings. Its public responsibilities cover initial requests, re-review requests, progress and completion retrieval and subscription, thread retrieval, replies, and resolution.

These responsibilities include the target architecture; they do not mean that all APIs already exist. The [#144 responsibility design](design/review-responsibility-boundaries.md) (Japanese) records the differences between current implementation and target behavior, source evidence, API/state/authentication contracts, and phased migration. The [follow-up issue proposals](design/review-responsibility-followups.md) (Japanese) have not been filed.

## Six-component review infrastructure

| Component | Responsibility |
|---|---|
| review-raven | Request intake, re-review requests, request state and progress publication, and finding remediation |
| thread-owl | Reviewer target validation, the internal candidate queue, and finding and result publication |
| squirrel-notifier | Launching, CI waits, holds, resumption, notifications, and execution observations |
| mcp-resource-subscriber / resource-bridge-cli | Subscriptions, update reception, resource reads, tool calls, and structured delivery to the same session |
| mcp-gateway | Client authentication, verified identity propagation, connectivity, and routing |
| Mcp-Docker | Canonical skills and instructions, distribution, Compose, routes, and CLI registration |

review-raven does not perform reviews. thread-owl retains its internal reviewer queue. A Squirrel process exit is not a review result. GitHub posts made by thread-owl are the source of truth for review results; the request side does not create an independent Verdict.

## Current implementation

専用GitHub Appモードはレビュー対応の取得・返信・resolve、SHA固定check runs、投稿者検証、ランナーの定型エスカレーションを公開する。review-ravenがinstallation tokenを発行・更新し、gatewayの検証済みidentityと専用共有Bearerを要求する。詳細は [App認証](github-app-auth.md) を参照する。

The default gateway mode also retains Copilot requests, status, cycle, wait, watch, and diagnostic tools. Watch resources use `review-raven://watch/{id}`; their persistent state resides in SQLite's `trigger_log` and `review_watch` tables. The dedicated App mode does not expose those tools or resources. There is no persistent ledger for the new review-request workflow yet.

`get_review_threads(include_bodies=false)` returns metadata without bodies. Omitting that option returns bodies, so the current skill also bears responsibility for validating authors before body retrieval. A reader that enforces this gate within the tool remains follow-up work.

## Current request and completion wait

- In `--mcp-http` mode, the implementation agent calls thread-owl's `enqueue_review`, then subscribes to `review://status/{owner}/{repo}/{prNumber}` through the external `resource-bridge-cli`.
- In `--webhook-mcp-http` mode, GitHub events enqueue candidates. Do not add a manual enqueue for the same event. Webhook intake does not reset the status to pending, so the manual path's completion-wait contract cannot be assumed for that path.
- The current skill posts re-review comments through GitHub MCP or another explicitly supported route. review-raven does not have a generic `add_issue_comment` tool. A comment alone does not guarantee queue registration, reviewer startup, or completion.
- Validate the URI, acknowledgement, target PR, final status, and agreement between the pinned HEAD, result HEAD, and current PR HEAD. Fetch unresolved threads after completion. Timeouts and missing state are not success.
- Queue registration does not launch a reviewer. An independent reviewer starts through the existing Squirrel launch contract or an explicit instruction to a separate CLI. Do not perform reviewer-side self-review in the implementation session.

Keep this path until the phased migration defined in #144. Do not register or discover the CLI as an MCP server. The broader subscription-internalization and CLI-removal initiative remains on hold.

## Target migration

review-raven will persist requests with a fixed cycle and HEAD, then deliver candidates to thread-owl's internal intake. It will receive execution observations from Squirrel and result references from thread-owl and expose them through a resource for each request. Initial request, intermediate reporting, completion, fixes, replies, resolution, and re-review will use the same public entry point. The [design document](design/review-responsibility-boundaries.md) (Japanese) defines the proposed API names, URIs, authorization, duplicate and delayed delivery handling, retention, and rollback conditions.

The reviewer-side dependency on review-raven's `list_check_runs_for_sha` is documented and retained. The permanent CI data source will be decided separately when the deferred D3 discussion resumes. GitHub MCP removal depends on replacement readers, narrowly scoped posting APIs, consumer migration, and live validation.

The legacy `copilot-review://watch/{id}` scheme and `COPILOT_REVIEW_*` environment variables are unsupported. Retiring existing watch or Copilot APIs requires a separate PR after a consumer audit.
