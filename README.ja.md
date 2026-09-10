# review-raven

<p align="center">
  <img src="assets/review_raven_avatar.png" alt="review-raven" width="150">
</p>

[English](README.md)

PR レビューを受けて直す側の MCP（Model Context Protocol）サーバー。review thread の読み取り・返信・resolve は reviewer provider（Copilot review・human reviewer・bot reviewer）を問わず対応する。再レビュー依頼は設定された acquisition provider 経由で行う。LLM 向けの async watch + notification モデルで提供する。

> **mcp-gateway 必須**: このサーバーは認証を担う **[mcp-gateway](https://github.com/mcp-b/mcp-gateway)** の背後にデプロイする必要があります。mcp-gateway が OAuth を処理し `X-Authenticated-User` と `Authorization` ヘッダーを注入します。スタンドアロン OAuth は非対応です。`copilot-review-mcp` からの移行については [docs/architecture.ja.md — Migration / 互換性](docs/architecture.ja.md#migration--互換性) を参照。

## 特徴

- **async watch + notification** ベース。`start_copilot_review_watch` で background watch を開始し、`get_copilot_review_watch_status` の cheap read と `notifications/resources/updated` で進捗を取る
- **GraphQL ベースの Copilot review request**。REST `requested_reviewers` が bot actor を黙って無視する問題を回避する
- **PR レビュースレッド単位の操作**。`PRRT_xxx` ノード ID で reply / resolve / reply+resolve を行う
- **mcp-gateway** による認証。ゲートウェイが OAuth を処理し、検証済みの identity ヘッダーを注入する
- **Stateless Streamable HTTP**（MCP protocol `2026-07-28` のみ）。`Mcp-Session-Id` は発行も参照もせず、各リクエストは mcp-gateway が注入する GitHub token で個別に認可する。非推奨の `initialize` handshake は JSON-RPC `-32022`（`Unsupported protocol version`）で拒否するため、旧 client が下位バージョンへネゴシエートすることはない（client は `server/discover` で discovery する）
- **SQLite による watch state 永続化**。プロセス再起動後の active watch は `STALE` として観測できる

## 提供ツール

| ツール | 用途 |
|---|---|
| `request_copilot_review` | PR に Copilot レビューを依頼する |
| `get_copilot_review_status` | GitHub から即時 snapshot を取る |
| `start_copilot_review_watch` | background watch を開始（推奨経路の入口） |
| `get_copilot_review_watch_status` | watch の現在状態を cheap read |
| `list_copilot_review_watches` | 自分の active / recent watch 一覧 |
| `cancel_copilot_review_watch` | watch を停止 |
| `get_pr_review_cycle_status` | レビューサイクル全体の状態と次のアクション提案 |
| `get_review_threads` | レビュースレッド一覧（Raw データ。分類は呼び出し元 LLM 側で行う） |
| `reply_to_review_thread` | スレッドに返信 |
| `resolve_review_thread` | スレッドを解決済みにする |
| `reply_and_resolve_review_thread` | 返信→解決を順次実行 |
| `wait_for_copilot_review` | legacy blocking wait（fallback） |
| `diagnose_github_token` | 現在のトークンの login と OAuth スコープ(`X-OAuth-Scopes` レスポンスヘッダー由来)を返す。`PERMISSION_DENIED` の原因切り分け用 |

`get_review_threads` は任意の `include_bodies` input を受け付けます。後方互換性のため省略時は `true` です。`false` を指定すると、GitHub からレビュースレッド本文を選択しないメタデータのみの射影を返します。レスポンスにはコメント ID、投稿者メタデータ、URL、スレッドの解決状態、ページネーション完了情報が含まれます。

セットアップと運用は [docs/usage.ja.md](docs/usage.ja.md) を参照。ツール単位の詳細は [docs/watch-tools.ja.md](docs/watch-tools.ja.md) と [skill の所在・配置案内](docs/skills/README.md) を参照。アーキテクチャおよび Thread Owl・mcp-resource-subscriber との責務境界は [docs/architecture.ja.md](docs/architecture.ja.md) を参照。

## クイックスタート（Docker + mcp-gateway）

このサーバーは認証のために [mcp-gateway](https://github.com/mcp-b/mcp-gateway) が必要です。

```bash
# review-raven を起動（直接公開せずに内部で稼働させる）
docker run --rm -p 127.0.0.1:8083:8083 \
  -e BIND_ADDR=0.0.0.0 \
  -v review-raven-data:/data \
  ghcr.io/scottlz0310/review-raven:latest
```

mcp-gateway で、**gateway から到達可能**なこのサーバーの内部アドレスをプロキシするよう設定する（例：Docker ネットワーク上では `http://review-raven:8083`、Docker Desktop では `http://host.docker.internal:8083`）。[mcp-gateway ドキュメント](https://github.com/mcp-b/mcp-gateway) 参照。

**stdio クライアント**（Claude Desktop 等）では [mcp-remote](https://github.com/geelen/mcp-remote) を使用：

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

詳細なセットアップ手順は [docs/usage.ja.md](docs/usage.ja.md) を参照。

## 環境変数

| 変数 | 必須 | 既定値 | 説明 |
|---|---|---|---|
| `MCP_PORT` | | `8083` | リッスンポート |
| `BIND_ADDR` | | `127.0.0.1` | バインドアドレス。Docker で mcp-gateway（別コンテナ）から到達可能にするには `0.0.0.0` を指定する |
| `LOG_LEVEL` | | `info` | `debug` / `info` / `warn` / `error` |
| `SQLITE_PATH` | | `/data/review-raven.db` | watch state DB のパス |
| `IN_PROGRESS_THRESHOLD_SEC` | | `30` | review request から in-progress とみなすまでの猶予（秒） |

**非対応**（旧 `copilot-review-mcp` 系統で削除済み）: `GITHUB_CLIENT_ID`、`GITHUB_CLIENT_SECRET`、`BASE_URL`、`GITHUB_OAUTH_SCOPES`、`SESSION_TTL_MIN`、`TOKEN_CACHE_TTL_MIN`、`TOKEN_EXPIRES_IN_SEC`、`AUTH_MODE`。

## ローカル開発

Go 1.27+ が必要。

```bash
# テスト
go test ./...

# ビルド
go build -o bin/review-raven ./cmd/server

# Dockerイメージビルド
docker build -t review-raven:dev .
```

## 履歴

このリポジトリは [scottlz0310/Mcp-Docker](https://github.com/scottlz0310/Mcp-Docker) の `services/review-raven/` を分離したもの。git 履歴は移行していない。Mcp-Docker 側の関連 PR / Issue（`#47`, `#52`, `#53`, `#55`–`#58`, `#62`, `#63`–`#68`, `#74`–`#77`, `#92` など）は `docs/` 配下のドキュメントから参照される。

## ライセンス

MIT License — [LICENSE](LICENSE) を参照。
