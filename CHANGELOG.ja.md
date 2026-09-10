# Changelog

[English](CHANGELOG.md)

このプロジェクトにおける注目すべき変更は、すべてこのファイルに記録されます。

このフォーマットは [Keep a Changelog](https://keepachangelog.com/en/1.0.0/) に基づいており、
このプロジェクトは [Semantic Versioning](https://semver.org/spec/v2.0.0.html) に準拠しています。

## [Unreleased]

### 追加

- `get_review_threads` に `include_bodies: false` のメタデータのみの射影を追加しました。GraphQL query レベルでレビュースレッド本文を選択せず、コメント ID・投稿者メタデータ・URL・スレッドの解決状態・ページネーション完了情報を返します。([Issue #124](https://github.com/scottlz0310/review-raven/issues/124))

### 修正

- stale-guard バグ報告の旧ディレクトリを参照するリンク5件を、現行の `internal/` 配下へ修正しました。

### 削除

- skill の収蔵・配置を Mcp-Docker に一本化し、`review-raven-thread-owl-cycle` 日英版を削除しました。英語版と未使用の Copilot 専用 `pr-review-cycle` 日英版を廃止し、利用案内を Mcp-Docker の正本・配置手順へ更新しました。([Issue #122](https://github.com/scottlz0310/review-raven/issues/122))

## [0.3.0] - 2026-08-26

> **破壊的変更を含むリリースです。** `initialize` handshake でセッションを開く client は拒否されます。protocol `2026-07-28` の `server/discover` で discovery してください。

### 変更

- **破壊的変更: 非推奨の `initialize` handshake を拒否するようにしました。** review-raven は MCP protocol `2026-07-28` のみを提供するため、`initialize` を含む POST には HTTP 400 と JSON-RPC `-32022`（`unsupported protocol version`）を返します。`data` には `supported: ["2026-07-28"]` と client が要求したバージョンを載せます。batch body は全体を 1 つの応答で拒否するため、その応答の `id` は null になります（batch 内のどの request にも帰属しないため）。従来は go-sdk が handshake に応答し、client の legacy revision へネゴシエートしていました — Codex 0.149.1 の A/B 検証（`mcp_2026_07_28` を process-local に無効化）では、thread-owl が既に拒否している `2025-06-18` に対して review-raven だけが `ready` のままであり、SEP-2575 以前の client が到達できる最後の server として残っていました。go-sdk には server 側の切り替えスイッチが無いため、SDK handler の手前で handshake を遮断します。この拒否契約は、TS SDK の `legacy: "reject"` が thread-owl に提供している modern-only 拒否と同じ形です。client は `server/discover` で discovery してください。[thread-owl#165](https://github.com/scottlz0310/thread-owl/issues/165) の repo 横断 MCP `2026-07-28` 移行の一部です。([Issue #117](https://github.com/scottlz0310/review-raven/issues/117))

## [0.2.0] - 2026-08-25

> **破壊的変更を含むリリースです。** 運用者側の対応が必要な変更が 3 件あります: MCP transport が protocol `2026-07-28` の stateless になったこと、プロセス全体の GitHub token フォールバックが削除されたこと、legacy `copilot-review://` スキームと `COPILOT_REVIEW_*` 環境変数が削除されたこと。詳細は下記の **変更** / **削除** を参照してください。

### 追加

- `diagnose_github_token` MCP tool を追加 — 現在のリクエストで使われているトークンの login と OAuth スコープ(GitHub `GET /user` の `X-OAuth-Scopes` レスポンスヘッダーから解析)を返す。トークン生値は返さない。read 系操作は成功するのに write 系操作(`reply_and_resolve_review_thread` 等)がトークンのスコープ不足で `PERMISSION_DENIED` になるケースの原因切り分け用。([Issue #89](https://github.com/scottlz0310/review-raven/issues/89))

- `spike-request-scoped-reauth.md` / `.ja.md` — `get_review_threads` 等のリクエストスコープ（非 watch）tool call で `REAUTH_REQUIRED` が頻発する問題の spike 調査結果。根本原因は mcp-gateway builtin mode が token 交換時に GitHub provider アクセストークンを破棄するため、`EnsureFreshAccessTokenForSubject` が gateway RS256 JWT を Bearer として review-raven に渡し、GitHub が毎回 401 を返すこと。修正は mcp-gateway 側に帰属（[mcp-gateway#188](https://github.com/scottlz0310/mcp-gateway/issues/188) として起票済み）。review-raven 側のコード修正は不要。([Issue #87](https://github.com/scottlz0310/review-raven/issues/87))

- `docs/skills/thread-owl-review-cycle.md` / `.ja.md` — thread-owl reviewed-side cycle 用の新 skill を追加。ページネーション付き GraphQL でレビュースレッドを取得し、分類・修正・返信・resolve を行い、再レビューが必要な場合は `@thread-owl re-review requested` コメントを投稿して cycle を完了する。次の reviewer-side cycle は thread-owl webhook が起動する。([Issue #71](https://github.com/scottlz0310/review-raven/issues/71))

### 修正

- Docker ビルドで、ベースイメージの更新が `go.mod` の Go toolchain patch version に遅れている場合でも、宣言された Go toolchain を自動取得できるようにしました。

- `ClassifyGitHubError` が GitHub GraphQL の `"Resource not accessible by integration"` メッセージ(GitHub App installation token に必要なリポジトリ権限がない場合、HTTP 200 とともに返される)を認識し、生の Go エラー文字列を MCP tool 呼び出し元に漏らす代わりに `PERMISSION_DENIED` として分類するようにしました。[Issue #89](https://github.com/scottlz0310/review-raven/issues/89) の調査中に発見。([Issue #92](https://github.com/scottlz0310/review-raven/issues/92))

### 変更

- **破壊的変更: MCP Streamable HTTP transport を stateless 化しました（`StreamableHTTPOptions.Stateless: true`）。negotiate される protocol は `2026-07-28` です。** `github.com/modelcontextprotocol/go-sdk` を v1.6.1 → v1.7.0 に更新しました。go-sdk は Streamable HTTP で `2026-07-28` を stateless server にのみ許可します（stateful server は `2025-11-25` へフォールバックします）。`Mcp-Session-Id` ベースの session/login 紐付け（`sessionLogins` map、`authorizeSession`/`rememberSession`/`forgetSession`、periodic pruning loop、`sessionRecorder`/`sessionRecorderFlusher` のレスポンスヘッダー横取り機構）を削除しました — session が存在しないため、認可境界は per-request の GitHub token 認証のみとなり、これらが防いでいた session hijacking の攻撃面自体が消滅しています。`MCP_SESSION_TIMEOUT_MIN` と `EventStore` による stream resumption も削除しました（`2026-07-28` は `Last-Event-ID` を非サポート）。GET / DELETE request は stateless server の仕様に従い `405 Method Not Allowed` を返すようになりました。採用 protocol は contract test で端から端まで固定しています: discovery-first negotiation が `2026-07-28` に解決すること、negotiate 済み session 上での代表的な tool / resource request、`server/discover` が `Mcp-Session-Id` を返さないこと、`subscriptions/listen` の acknowledgement が `io.modelcontextprotocol/subscriptionId` を載せること、および 405 応答。[thread-owl#165](https://github.com/scottlz0310/thread-owl/issues/165) の repo 横断 MCP `2026-07-28` 移行の一部です。([Issue #111](https://github.com/scottlz0310/review-raven/issues/111))

- `pr-review-cycle` と `review-raven-thread-owl-cycle` の skill テンプレートに、PR コメント本文を読む前の fail-closed ガードを追加。`scottlz0310-user`、Copilot、Thread Owl、Codecov のコメントだけを信頼する。Copilot identity にはリポジトリで既知の `copilot`、`github-copilot`、`copilot-pull-request-reviewer` login family を含め、各 GitHub App について GraphQL の `[bot]` なし login と REST の `[bot]` あり login を同一 identity として許可する。本文を返す review tool より先に、本文なしの GraphQL query およびメタデータのみの REST 処理（`--jq` による投影など）で事前検査する。それ以外または投稿者を検証できないコメントが存在する場合は本文を引用・処理せず、人間エスカレーションとして自動処理を停止する。
- **`docs/skills/thread-owl-review-cycle.md` / `.ja.md` を `docs/skills/review-raven-thread-owl-cycle.md` / `.ja.md` にリネームしました**。あわせて frontmatter の `name` と、`pr-review-cycle.md` / `.ja.md` 内の相互参照リンクを更新しました。旧名（`thread-owl-*`）は thread-owl 自身の reviewer-side スキル（`thread-owl-pr-reviewer`）と紛らわしかったため、`review-raven-` を前置することで所属リポジトリと reviewed-side の役割を明確にしました。個人のインストール済みSkillコピー（`~/.claude/skills/`、`~/.gemini/antigravity-cli/skills/`）は、この変更を反映するために手動でのリネームが必要です。([Issue #96](https://github.com/scottlz0310/review-raven/issues/96))
- `docs/skills/thread-owl-review-cycle.ja.md` / `.md` — Phase 7 に thread-owl の Verdict コメント（`## @thread-owl Review Verdict: APPROVED` かつ `Status: READY_TO_MERGE`）の検証を追加し、記載された `Reviewed HEAD SHA` が現在の PR HEAD SHA と一致することを確認した上で Phase 8 マージゲートへ進むようにしました（従来の曖昧な「最終レビューでAPPROVEされたSHA」条件を置き換え。証拠がない・不一致の場合は `AWAITING_THREAD_OWL_VERDICT` として停止）。あわせて、`trivial` 修正のみでも PR HEAD が更新される点を踏まえ、Phase U6 の `need_re_review` 判定をすべての修正コミット（`trivial` 含む）で再レビュー要求するよう変更し、既存 Verdict コメントが恒久的に古いままになるデッドロックを解消しました。([Issue #94](https://github.com/scottlz0310/review-raven/issues/94))
- `docs/skills/thread-owl-review-cycle.ja.md` / `.md` — PR HEAD 同期ゲートを追加し、未 push の状態で返信・resolve・再レビュー依頼へ進むのを防止するようにしました。また、再レビュー依頼のアノテーションやサマリコメントに `expected_head` を記録するようにし、Phase 8 のマージ条件に PR HEAD SHA の照合ゲート（不一致時は `APPROVED_HEAD_MISMATCH` として停止）を追加しました。([Issue #84](https://github.com/scottlz0310/review-raven/issues/84))
- `docs/skills/pr-review-cycle.ja.md` / `.md` — Copilot レビュー用の対応サイクルスキルにも同様の PR HEAD 同期ゲートと Phase 8 マージ条件の SHA 照合ゲートを追加しました。([Issue #84](https://github.com/scottlz0310/review-raven/issues/84))
- `docs/skills/thread-owl-review-cycle.md` / `.ja.md` — 投稿者を問わずすべての未解決レビュー指摘に対応するよう更新。スレッドの取得・返信・解決において、第一選択（プライマリ）として `review-raven` MCP ツール（`get_review_threads`, `reply_and_resolve_review_thread`等）を、使えない場合のフォールバックとして `gh` CLI（GraphQL/REST API）を使用する構成へ変更。レビュー本文（review body）やPRコメント（issue comment）といった非スレッド指摘をページネーション付きで取得・返信・処理済み状態の記録・再確認を行う詳細な手順を明記。さらに、処理済みの非スレッド指摘IDをPRコメント内のアノテーション（`handled_comments`）に永続化し、次回サイクル開始時に復元して重複処理を防ぐ機能を追加。インストール済みSkillテンプレートへの更新手順を追記。([Issue #76](https://github.com/scottlz0310/review-raven/issues/76))
- `docs/skills/pr-review-cycle.md` / `.ja.md` — MCP ポーリング Phase 1 の代替として **Phase 1S**（`mcp-resource-subscriber --json` を使うサブスクリプション方式）を追加。全体フロー図を 2 経路エントリに更新。([Issue #74](https://github.com/scottlz0310/review-raven/issues/74))
- `docs/skills/pr-review-cycle.md` / `.ja.md` — **Copilot review 専用** スキルとして明示的にスコープを限定。Phase 6 `REQUEST_REREVIEW` を `request_copilot_review` + Copilot watch ループ（当初設計）に戻した。スコープ注意書きと `## 関連スキル`（`thread-owl-review-cycle` へのリンク）を追加。([Issue #71](https://github.com/scottlz0310/review-raven/issues/71))
- `docs/architecture.md` / `.ja.md` — 再レビュー依頼フローのセクションを追加。review-raven（コメント投稿）・thread-owl（webhook → queue）・mcp-resource-subscriber（購読ブリッジ）の責務境界を文書化。([Issue #69](https://github.com/scottlz0310/review-raven/issues/69))

- **Go toolchain の要求バージョンを 1.27 へ引き上げ。** `go.mod` の宣言が `go 1.27.0` になり、builder image も `golang:1.27-alpine` へ移行。ソースからビルドする場合は Go 1.27+ が必要（README の要求記述も合わせて更新）。`modernc.org/sqlite` は v1.57.0 へ更新。([PR #108](https://github.com/scottlz0310/review-raven/pull/108))

### 削除

- process-wide な `GITHUB_PERSONAL_ACCESS_TOKEN` / `REVIEW_RAVEN_DEFAULT_USER` fallback を削除。すべての GitHub API 経路で mcp-gateway が request ごとに注入する identity と Bearer token を必須とし、ヘッダー欠落・不正時は fail-closed で拒否する。([Issue #106](https://github.com/scottlz0310/review-raven/issues/106))

- **旧 `copilot-review://` URI スキームおよび `COPILOT_REVIEW_*` 環境変数を削除** ([Issue #66](https://github.com/scottlz0310/review-raven/issues/66)):
  - `SubscribeHandler` が `copilot-review://watch/...` URI に対して `ResourceNotFoundError` を返すよう修正。従来は `nil` を返すためゴースト購読が成立していた。アクティブな watch は再依頼が必要。
  - `parseWatchIDFromURI()` が `copilot-review://watch/{id}` を受け付けなくなった。有効なのは `review-raven://watch/{id}` のみ。
  - `loadConfig()` から `COPILOT_REVIEW_GATEWAY_INTERNAL_URL` / `COPILOT_REVIEW_GATEWAY_INTERNAL_SECRET` の fallback 読み込みを削除。`REVIEW_RAVEN_GATEWAY_INTERNAL_URL` / `REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET` を直接設定すること。

## [0.1.0] - 2026-06-08

**review-raven** としての最初のリリース — プロダクト名変更とアーキテクチャドキュメントの追加。
このバージョンからバージョン番号を `0.1.0` に振り直す。旧 `copilot-review-mcp` 系統（v2.5.0 – v3.2.0）の履歴は下記の旧バージョン履歴セクションに保存する。

### 変更

- **リポジトリ名を `copilot-review-mcp` から `review-raven` へ改名** ([Issue #63](https://github.com/scottlz0310/review-raven/issues/63)):
  - Go module path 変更: `github.com/scottlz0310/copilot-review-mcp` → `github.com/scottlz0310/review-raven`
  - MCP server 実装名変更: `"copilot-review-mcp"` → `"review-raven"`; バージョンを `0.1.0` に振り直し
  - Resource URI スキーム変更: `copilot-review://watch/{id}` → `review-raven://watch/{id}`
  - MCP client 設定キー変更: `"copilot-review"` → `"review-raven"`（tool prefix: `mcp__review-raven__*`）
  - 環境変数名変更: `COPILOT_REVIEW_GATEWAY_INTERNAL_URL/SECRET` → `REVIEW_RAVEN_GATEWAY_INTERNAL_URL/SECRET`（旧名は後方互換 fallback として引き続き読まれる）
  - SQLite デフォルトパス変更: `/data/copilot-review.db` → `/data/review-raven.db`（`SQLITE_PATH` 環境変数で上書き可能）
  - Docker イメージ・コンテナ・ボリューム名変更: `copilot-review-mcp` → `review-raven`、`copilot-review-data` → `review-raven-data`
  - `.env.template` を更新: 正式な `REVIEW_RAVEN_*` 変数名に統一し、旧 `COPILOT_REVIEW_*` を migration / compatibility セクションに移動
  - 旧 URI スキーム `copilot-review://watch/{id}` を deprecated read/subscribe alias として受け付けるよう対応。新規 URI は `review-raven://watch/{id}` のみ生成

### 追加

- `docs/architecture.md` / `docs/architecture.ja.md` — reviewed 側 MCP server としての位置づけと、Thread Owl・mcp-resource-subscriber との責務境界を文書化。URI スキーム・環境変数・tool 名の互換性情報を含む Migration / 互換性セクションを追加。

---

## 旧バージョン履歴 (copilot-review-mcp)

以下のエントリは `copilot-review-mcp` 時代（git タグ `v2.5.0` – `v3.2.0`）の記録である。
このセクションのバージョン番号は旧系統のものであり、上記 `review-raven` のバージョニングとは無関係。

### [3.2.0] - 2026-05-18

#### 追加

- **Phase B 委譲バックグラウンドアクセス — gateway 統合テスト (PR-C)** — [Issue #40](https://github.com/scottlz0310/review-raven/issues/40)（[Issue #29](https://github.com/scottlz0310/review-raven/issues/29) の一部）:
  - `internal/watch/gateway_integration_test.go` で `gatewayTokenSource → oauth2.ReuseTokenSource → oauth2.Transport → *ghclient.Client → watch.Manager.pollOnce` の経路全体を fake `POST /internal/v1/whoami` と最小限の fake GitHub REST サーバで end-to-end 実行。production 配線 (`cmd/server/main.go` の `buildGatewayClientFactory`) と同じ組み立てを再現する。
  - 6 シナリオを網羅: happy path (200 → `COMPLETED`)、subject gone (404 → `FAILED`/`AUTH_EXPIRED` + re-seed hint)、rotation_failed (502/rotation_failed → `FAILED`/`AUTH_EXPIRED` + refresh-rejected hint)、upstream_failure 単発で `WATCHING` 維持、upstream_failure 連続で `FAILED`/`AUTH_EXPIRED` + consecutive-polls hint、token rotation が GitHub 側に観測可能 (`oauth2.ReuseTokenSource` が rotate 後の値を取り直す)。
  - 既存 `manager_test.go` の sentinel error 直接注入とは独立した経路で結線を検証し、factory リファクタ等で chain が壊れた場合に fail-closed する。
- **Phase B 委譲バックグラウンドアクセス — クライアントコア (PR-A)** — [Issue #29](https://github.com/scottlz0310/review-raven/issues/29):
  - `internal/github/gateway_token_source.go` — gateway の `POST /internal/v1/whoami` を叩く `oauth2.TokenSource` 実装 `gatewayTokenSource`。コンストラクタで loopback ホスト (`127.0.0.1` / `::1` / `localhost`) を検証。`expires_at` を `oauth2.Token.Expiry` に反映するため `oauth2.ReuseTokenSource` で whoami 呼び出しを抑制可能。
  - Sentinel エラー `ErrGatewaySubjectGone` (404)、`ErrGatewayUnauthorized` (401)、`ErrGatewayLoopbackRequired` (403)、`ErrGatewayUpstreamFailure` (502)、`ErrGatewayBadRequest` (その他 4xx)、`ErrGatewayNonLoopback`。`FailureReasonAuthExpired` / recovery hint へのマッピングは PR-B に延期。
  - `internal/github/client.go` — 動的トークン用 `NewClientWithTokenSource(ctx, ts, threshold)` を追加（`invalidatingTransport` は付与せず、PR-B で対応）。
  - `internal/tools/server.go` — `BuilderOptions{GatewayClientFactory}` と `BuildStreamableHandlerWithOptions` を追加。既存 `BuildStreamableHandler(db, threshold)` のシグネチャは維持。
  - `cmd/server/main.go` — `REVIEW_RAVEN_GATEWAY_INTERNAL_URL` と `REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET` の両方を設定したときのみ opt-in。未設定時は従来通り `oauth2.StaticTokenSource`（動作変更なし）。片方のみ設定された場合は fail-closed で起動を中断。
  - gateway に送る **subject** は認証済み GitHub login（gateway 仕様に準拠）。
  - **制約**: PoC のためクライアントと gateway は同一ホスト (loopback) 必須。Docker Compose の複数コンテナ構成は PR-A では非対応。

#### 変更

- `watch.Options.ClientFactory` のシグネチャを `func(ctx, token string) ReviewDataFetcher` から `func(ctx, token, login string) ReviewDataFetcher` に拡張。内部呼び出し側のみの修正。
- **Phase B PR-A レビュー反映 (PR #30 Copilot レビュー)**:
  - `gatewayTokenSource.Token()` のリクエストコンテキストを設定可能な親 (`GatewayTokenSourceConfig.Context`) と単一の `defaultGatewayTimeout` 定数 (10秒) から派生するよう変更。watch のキャンセル/サーバーシャットダウンが in-flight な whoami 呼び出しに伝播するようになった。
  - 非 200 応答時にレスポンスボディの一部を破棄してから sentinel エラーを返すことで、`net/http` の keep-alive 接続再利用を可能化。
  - `ghclient.ValidateGatewayEndpoint(url, secret)` を新設し、`loadConfig` の起動時チェックに組み込み。不正な URL・非 http(s) スキーム・非ループバックホスト・空シークレットは起動時に fail-fast。watch 毎に static トークンへサイレント降格していた挙動を排除。
  - `buildGatewayClientFactory` で `*http.Client` を 1 度だけ生成し、`GatewayTokenSourceConfig.HTTPClient` 経由で全 watch のトークンソースに共有 (transport / idle 接続プール再利用)。空 login 時のみ到達する static トークンへの runtime フォールバックは `slog.Error` でログ。
  - `GatewayTokenSourceConfig.HTTPClient` の docstring を修正: トークンソースは subject 毎だが、内部の `*http.Client` / `http.Transport` は並行再利用可能で watch 間で共有すべき。

### [3.1.0] - 2026-05-09

#### 追加

- **5 つの新しい構造化エラー型** を `internal/autherr` に追加し、[Issue #21](https://github.com/scottlz0310/review-raven/issues/21) を完結:
  - `PERMISSION_DENIED` — HTTP 403 レスポンス（rate limit 以外）
  - `RATE_LIMITED` — プライマリ rate limit（`*github.RateLimitError`）とセカンダリ/abuse rate limit（`*github.AbuseRateLimitError`）。`retryable` と `safe_to_continue` は状況依存
  - `NOT_FOUND` — HTTP 404 レスポンス
  - `VALIDATION_ERROR` — HTTP 400 / 422 レスポンス
  - `TRANSIENT_UPSTREAM_ERROR` — HTTP 5xx レスポンス（retryable）
- **`ClassifyGitHubError(err error) *autherr.AuthError`** を `internal/github/client.go` に追加。REST `*github.ErrorResponse`、`*github.RateLimitError`、`*github.AbuseRateLimitError`、shurcooL/githubv4 の文字列マッチ、既分類済みの `*autherr.AuthError` を含む任意の GitHub API エラーを適切な構造化エラー型に変換する単一エントリポイント。
- `internal/tools/auth_result.go` の `tryAuthResult` および `authErrString` が `IsAuthError` の代わりに `ClassifyGitHubError` を呼ぶようになり、8 種類のエラー型すべてに対してハンドラごとの変更なしに構造化エラーを返せるようになった。

#### 変更

- skill テンプレート（`docs/skills/`）の MCP サーバーキーを `copilot-review`（旧 `review-raven`）と `github`（旧 `github-mcp-server-docker`）に統一。mcp-docker / mcp-gateway のデフォルト設定に合わせた規約変更（#23）。usage docs（`docs/usage.md`、`docs/usage.ja.md`）も同規約に合わせて更新。

### [3.0.0] - 2026-05-06

#### 削除

- **スタンドアロン GitHub OAuth App フローを完全削除。** `internal/auth` パッケージ（handler、session、token cache）を削除。
- `AuthModeStandalone`、`AuthModeGateway` 定数と `AuthMode` 型を `internal/middleware` から削除。
- `TokenInvalidator` インタフェースと `BuildStreamableHandler` の第三引数 `inv TokenInvalidator` を削除。
- 削除された環境変数: `GITHUB_CLIENT_ID`、`GITHUB_CLIENT_SECRET`、`BASE_URL`、`GITHUB_OAUTH_SCOPES`、`SESSION_TTL_MIN`、`TOKEN_CACHE_TTL_MIN`、`TOKEN_EXPIRES_IN_SEC`、`AUTH_MODE`。
- OAuth エンドポイント（`/.well-known/oauth-authorization-server`、`/authorize`、`/callback`、`/token`、`/register`）は **410 Gone** と移行案内を返すようになった。

#### 変更

- **認証に mcp-gateway が必須**。サーバーはゲートウェイが注入する `X-Authenticated-User` ヘッダーと `Authorization: Bearer` トークンを信頼する。
- `BuildStreamableHandler(db, threshold)` — 第三引数を削除。
- `middleware.Auth()` — `TokenValidator` と `AuthMode` を引数に取らなくなった（gateway のみ対応）。
- MCP サーバー実装メタデータのバージョンを `3.0.0` に更新。

#### 追加

- `BIND_ADDR` 環境変数（デフォルト `127.0.0.1`）。Docker で mcp-gateway（別コンテナ）から到達可能にするには `0.0.0.0` を指定する。

#### 移行ガイド

`AUTH_MODE=standalone` または `AUTH_MODE=gateway` で運用していた場合:

1. このサーバーの前段に [mcp-gateway](https://github.com/mcp-b/mcp-gateway) をデプロイする。
2. 以下の環境変数を削除する: `GITHUB_CLIENT_ID`、`GITHUB_CLIENT_SECRET`、`BASE_URL`、`AUTH_MODE`、`GITHUB_OAUTH_SCOPES`、`SESSION_TTL_MIN`、`TOKEN_CACHE_TTL_MIN`、`TOKEN_EXPIRES_IN_SEC`（削除された変数の全リストは上記「破壊的変更」を参照）。
3. MCP クライアントの接続先を mcp-gateway の URL に変更する。stdio クライアントは [mcp-remote](https://github.com/geelen/mcp-remote) を使用する。

### [2.5.0] - 2026-04-26

#### 追加

- [scottlz0310/Mcp-Docker](https://github.com/scottlz0310/Mcp-Docker) の `services/review-raven/` を独立リポジトリへ分離
- Copilot review workflow 向けの OAuth 対応 Streamable HTTP MCP サーバーを追加
- async watch ツール、review thread の reply/resolve ツール、`pr-review-cycle` skill テンプレートを追加
- SQLite による watch state 永続化と、プロセス再起動後の stale watch 検知を追加
- README、changelog、watch tool docs、skill docs、usage docs を英日バイリンガル化
- test、scan、build、ghcr.io への Docker image 公開 CI を追加

#### 補足

- この独立リポジトリでは、Mcp-Docker 時代の `review-raven` service 作業から release continuity を引き継ぐ。git 履歴は移行していない。
- 関連する設計・移行経緯は `docs/` 配下を参照。

[Unreleased]: https://github.com/scottlz0310/review-raven/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/scottlz0310/review-raven/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/scottlz0310/review-raven/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/scottlz0310/review-raven/releases/tag/v0.1.0
