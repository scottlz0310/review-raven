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
- **Stateless Streamable HTTP**（MCP protocol `2026-07-28` のみ）。`Mcp-Session-Id` は発行も参照もせず、各リクエストの利用者認証はmcp-gatewayに委任する。GitHub APIには設定に応じて利用者tokenまたは専用App tokenを使う。非推奨の `initialize` handshake は JSON-RPC `-32022`（`Unsupported protocol version`）で拒否するため、旧 client が下位バージョンへネゴシエートすることはない（client は `server/discover` で discovery する）
- **SQLite による watch state 永続化**。プロセス再起動後の active watch は `STALE` として観測できる

## 提供ツール

専用Appモード（`REVIEW_RAVEN_AUTH_MODE=github-app`）では、スレッド取得・返信・resolve・返信後resolve・check-runs取得・許可リスト取得の6 toolだけを公開します。返信は`review-raven[bot]`名義になり、Copilot系・ユーザートークン診断・watchは非公開です。既定の`gateway`モードは従来の機能を維持します。[設定・移行・V4検証](docs/github-app-auth.md)を参照してください。

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
| `list_check_runs_for_sha` | 固定したコミット SHA の check runs 一覧（Raw データ。合否判定は呼び出し元で行う） |
| `wait_for_copilot_review` | legacy blocking wait（fallback） |
| `diagnose_github_token` | 現在のトークンの login と OAuth スコープ(`X-OAuth-Scopes` レスポンスヘッダー由来)を返す。`PERMISSION_DENIED` の原因切り分け用 |
| `get_trusted_comment_authors` | PR のコメントの本文を、エージェントの文脈に入れてよい投稿者の許可リスト（サーバーの `TRUSTED_COMMENT_AUTHORS`）を返す。read-only。未設定のときは `TRUSTED_AUTHORS_NOT_CONFIGURED` で失敗する |
| `request_review` | _opt-in。専用 App モードのみ。_ 期待する PR の HEAD に固定してレビューを依頼する（初回と再依頼は同じ入口）。主体と PR の範囲で冪等。購読先の `review-raven://requests/{cycleId}` を返す |
| `get_review_request` | _opt-in。_ レビュー依頼の現在の snapshot（配送・取消・実行観測・結果参照・`terminal`）を返す |
| `cancel_review_request` | _opt-in。_ 結果なしで失敗した依頼を明示的に取り消し、同じ HEAD を新しい round で依頼し直せるようにする |

`get_review_threads` は任意の `include_bodies` input を受け付けます。後方互換性のため省略時は `true` です。`false` を指定すると、GitHub からレビュースレッド本文を選択しないメタデータのみの射影を返します。レスポンスにはコメント ID、投稿者メタデータ、URL、スレッドの解決状態、ページネーション完了情報が含まれます。

`list_check_runs_for_sha` は `owner`・`repo`・`sha` を受け取ります。`sha` は 40 桁の小文字 16 進数のコミット SHA に限り、呼び出しの間に対象が動かないよう PR 番号や branch 名は受け付けません。全ページを取得して各 run の `head_sha`・`status`・`conclusion`・`app`・`html_url` を返すため、呼び出し元は CI 結果が固定したコミットのものか照合できます。再実行 run は (app ID, name) ごとに ID が最大の run に集約し、`deduplication` にその方式と重複排除前の件数を示します。合否判定は行いません。

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
| `TRUSTED_COMMENT_AUTHORS` | | _(未設定)_ | PR のコメントの本文を、エージェントの文脈に入れてよい GitHub の login（カンマ区切り。`name` でも `name[bot]` でもよい。大文字小文字は区別しない）。`get_trusted_comment_authors` が公開する。不正な要素（ワイルドカードなど）があると、起動を止める。未設定のときは、ツールが `TRUSTED_AUTHORS_NOT_CONFIGURED` で失敗する（fail-closed） |
| `REVIEW_RAVEN_REVIEW_REQUESTS` | | `false` | `true` でレビュー依頼の tool と `review-raven://requests/{cycleId}` resource を公開する。専用 GitHub App モード（呼び出し主体を proxy で検証できること）と `REVIEW_RAVEN_REQUEST_ACL` が必須で、満たさなければ起動を止める。無効のときは既存の公開面を変えない |
| `REVIEW_RAVEN_REQUEST_ACL` | | _(未設定)_ | レビュー依頼の tool のアクセスリスト（JSON）。例: `{"users":{"your-login":["your-org/*"]},"operators":["your-login"]}`。`users` は GitHub の login が依頼・読取でき、自分の依頼を取り消せる repo（`owner/repo` または `owner/*`）、`operators`（`users` の部分集合）は他の主体の依頼も取り消せる。cycleId を知っていることや App の広い installation 権限だけでは許可しない。不正または空の設定は起動を止める |
| `REVIEW_RAVEN_INTERNAL_SURFACE` | | `false` | `true` で、thread-owl と Squirrel 向けの内部面（サービス間）を、別パス `/internal/mcp` の別の MCP サーバーとして公開する。`ingest_review_candidate`・`publish_review_result`・`publish_review_observation` は agent 向けの tool 一覧に現れない。`REVIEW_RAVEN_REVIEW_REQUESTS=true`・`REVIEW_RAVEN_SERVICE_ACL`・`REVIEW_RAVEN_INTERNAL_PROXY_SECRET` が必須で、満たさなければ起動を止める |
| `REVIEW_RAVEN_INTERNAL_PROXY_SECRET` | | _(未設定)_ | gateway が内部面のルートで提示する共有シークレット（32 文字以上）。`REVIEW_RAVEN_PROXY_SECRET` と異なる値にする（公開面のシークレットを持つ呼び出し元が内部面に到達できないようにするため） |
| `REVIEW_RAVEN_SERVICE_ACL` | | _(未設定)_ | 内部面のアクセスリスト（JSON）。例: `{"services":{"svc-owl":{"actions":["ingest","publish_result"],"repos":["your-org/*"]},"svc-squirrel":{"actions":["observe"],"repos":["your-org/*"],"producers":["squirrel-notifier"]}}}`。サービス主体ごとに、呼べる操作（`ingest`・`publish_result`・`observe`）と、作用できる repo（`owner/repo` または `owner/*`）を指定する。`observe` を持つサービスは、送信できる `producers` も指定する。同じ名前を利用者（`REVIEW_RAVEN_REQUEST_ACL`）とサービスの両方に置けない。不正または空の設定は起動を止める |
| `REVIEW_RAVEN_OWL_INTERNAL_URL` | | _(未設定)_ | thread-owl の内部面の MCP エンドポイント（通常は gateway のルート）。キューに載った候補と取消を thread-owl へ送る配送 worker を起動する。https、または loopback ホストの http のみ。`REVIEW_RAVEN_OWL_INTERNAL_TOKEN` と同時に指定し、`REVIEW_RAVEN_REVIEW_REQUESTS=true` が必須。片方だけの設定や平文での配送は起動を止める |
| `REVIEW_RAVEN_OWL_INTERNAL_TOKEN` | | _(未設定)_ | thread-owl へ Bearer で提示するサービス資格情報（gateway がサービス主体へ対応付ける）。ログには出さない |
| `REVIEW_RAVEN_DELIVERY_INTERVAL_SEC` | | `5` | 配送 worker が期限の来たメッセージを探す間隔（1〜3600 秒） |

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
