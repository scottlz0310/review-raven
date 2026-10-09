# アーキテクチャ

[English](architecture.md)

## review-raven の位置づけ

`review-raven` はレビュー基盤における **review される側** の MCP server である。レビューを*受けて*修正する側の操作を提供する。

このリポジトリは reviewer 側の操作（review 投稿・webhook 受信・queue 管理）を実装しない。それらは [thread-owl](https://github.com/scottlz0310/thread-owl) の責務である。

## 5本立てレビュー基盤

| # | リポジトリ | 立場 | 責務 |
|---|-----------|------|------|
| 1 | **[thread-owl](https://github.com/scottlz0310/thread-owl)** | review する側 | GitHub App bot。同一アカウントから review・in-line-comment を投稿する |
| 2 | **review-raven**（このリポジトリ） | review される側 | MCP server。スレッド読み取り・返信・resolve/unresolve・再レビュー依頼 |
| 3 | **[mcp-resource-subscriber](https://github.com/scottlz0310/mcp-resource-subscriber)** | 状態購読ブリッジ | review-raven・thread-owl 両方から skill 経由で呼ばれる。`resources/updated` 通知を購読する |
| 4 | **[mcp-gateway](https://github.com/scottlz0310/mcp-gateway)** | MCP reverse proxy | MCP server 群への routing と認証境界 |
| 5 | **[Mcp-Docker](https://github.com/scottlz0310/Mcp-Docker)** | container orchestration | コンテナ管理・gateway route 生成・CLI agent 設定自動化 |

## review-raven の責務

- PR レビュースレッドを読む
- unresolved スレッドを抽出・分類する
- review comment に返信する
- review スレッドを resolve / unresolve する
- 修正完了後に再レビュー依頼を行う
- Copilot review に限らず GitHub review スレッド全般を扱う（AI reviewer / human reviewer / bot reviewer 問わず）

Copilot review は取得 provider の一つであり、このリポジトリの恒久的な中心概念ではない。

## thread-owl との責務境界

| review-raven | thread-owl |
|---|---|
| review される側の操作 | review する側の操作 |
| スレッド読み取り・返信・resolve | review 投稿 |
| 再レビュー依頼 | webhook 受信 |
| — | review candidate queue |
| — | `queue://review/queue` MCP resource |

このリポジトリに reviewer 側の queue / webhook / GitHub App 投稿基盤を追加しない。thread-owl に reviewed 側の thread resolve / re-review request workflow を追加しない。

## mcp-resource-subscriber との責務境界

`mcp-resource-subscriber` は MCP resource subscription を agent workflow に接続する外部 CLI / ブリッジである。review-raven は長時間稼働する subscription client や watcher CLI を内蔵しない。必要な場合は agent skill から `mcp-resource-subscriber` を外部呼び出しする。

## MCP server 詳細

専用Appモードでは、gatewayの利用者認証とGitHub APIの資格情報を分離する。review-raven自身が専用Appのinstallation tokenを発行し、reviewed用6 toolだけを公開する。既定のgatewayモードは従来の資格情報委譲・Copilot・watchを維持する。[設定・信頼境界・移行手順](github-app-auth.md)を参照。

- **server 名**: `review-raven`
- **MCP client キー**: `review-raven`（tool prefix: `mcp__review-raven__*`）
- **Resource URI スキーム**: `review-raven://watch/{watch_id}`
- **認証**: mcp-gateway 経由の gateway 委任（`X-Authenticated-User` + `Authorization` ヘッダー）

## Migration / 互換性

### Resource URI スキーム

watch resource のスキームは `review-raven://watch/{id}`。改名前に使われていた旧スキーム `copilot-review://watch/{id}` は**受け付けない**。`copilot-review-mcp` からアップグレードした場合、active な watch は再依頼が必要。

### 環境変数

`REVIEW_RAVEN_GATEWAY_INTERNAL_URL` と `REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET` のみサポート。旧名 `COPILOT_REVIEW_GATEWAY_INTERNAL_URL/SECRET` は**読まれない**。

### MCP ツール名

公開 API 互換維持のため、ツール名（`request_copilot_review`、`start_copilot_review_watch` 等）は変更しない。

## 再レビュー依頼フロー

修正完了後、review-raven は `add_issue_comment` で PR に `@thread-owl re-review requested` を投稿する。これが reviewed-side cycle と reviewer-side cycle の境界となる。

| コンポーネント | 責務 |
|---|---|
| **review-raven**（reviewed 側） | 修正 → reply → resolve 完了後に `@thread-owl re-review requested` コメントを投稿する。これが reviewed-side cycle の終端。 |
| **thread-owl**（reviewer 側） | `issue_comment.created` webhook で `@thread-owl` メンションを検出 → `ReviewCandidate.reason = "re-review-requested"` として queue に積む → `queue://review/queue` resource を更新通知する。 |
| **mcp-resource-subscriber** | queue resource を購読し、更新を agent workflow に橋渡しする。 |

review-raven は review queue を管理しない。`@thread-owl` コメントが引き渡しポイントであり、投稿後に reviewed-side cycle は完了する。次の reviewer-side cycle は thread-owl の queue から始まる。

この方式は Copilot 固有 API ではなく GitHub の通常 PR conversation を利用するため、任意の reviewer（Copilot・人間・bot）に対して機能する。

## pr-review-subscribe skill との関係

`pr-review-subscribe` skill は review 取得・スレッド処理・修正 workflow を統合する上位 workflow である。review-raven はその中で reviewed-side MCP provider として機能する。

## 将来方針

[github-mcp-server](https://github.com/github/github-mcp-server) の成熟により、review-raven が現在提供している MCP ツール群が不要になる可能性がある。github-mcp-server が全ツールをカバーした場合、review-raven は skill のみの構成になるかもしれない。新しい MCP ツールを追加する前に、github-mcp-server での代替可能性を先に確認すること。

## 関連 ISSUE

- [review-raven #63](https://github.com/scottlz0310/review-raven/issues/63) — 責務境界定義（このドキュメント）
- [thread-owl #75](https://github.com/scottlz0310/thread-owl/issues/75) — Thread Owl の責務境界
- [mcp-resource-subscriber #86](https://github.com/scottlz0310/mcp-resource-subscriber/issues/86) — `--json` output mode
- [mcp-gateway #92](https://github.com/scottlz0310/mcp-gateway/issues/92) — MCP reverse proxy / auth boundary
- [Mcp-Docker #158](https://github.com/scottlz0310/Mcp-Docker/issues/158) — container orchestration
