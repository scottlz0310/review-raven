# 使い方

[English](usage.md)

このドキュメントでは、`review-raven` を MCP サーバーとして動かすための基本設定をまとめる。

- アーキテクチャ概要（mcp-gateway 必須）
- Docker コンテナの起動、終了、ログ確認
- mcp-gateway 経由の MCP クライアント接続
- Mcp-Docker 経由のレビュー対応 skill の配置

ツール単位の流れは [watch-tools.ja.md](watch-tools.ja.md)、skill の所在と配置は [案内](skills/README.md)を参照してください。

> **mcp-gateway 必須**: スタンドアロン OAuth は非対応です。すべてのトラフィックは mcp-gateway を経由する必要があります。`copilot-review-mcp` からの移行については [architecture.ja.md — Migration / 互換性](architecture.ja.md#migration--互換性) を参照。

## アーキテクチャ

```
MCP クライアント（Claude Code / Claude Desktop / VS Code）
    │
    │  HTTPS / OAuth（mcp-gateway が処理）
    ▼
mcp-gateway  ──►  X-Authenticated-User + Authorization ヘッダーを注入
    │
    │  HTTP（内部通信のみ）
    ▼
review-raven  :8083
    │
    │  SQLite
    ▼
/data/review-raven.db
```

`review-raven` は mcp-gateway が注入したヘッダーを信頼し、OAuth を直接行わない。

## 1. mcp-gateway をセットアップする

[mcp-gateway のドキュメント](https://github.com/mcp-b/mcp-gateway) に従ってデプロイ・設定する。

gateway の upstream ルートのひとつを、**gateway から到達可能**なアドレスに向ける（例：同一 Docker ネットワーク上では `http://review-raven:8083`、Docker Desktop では `http://host.docker.internal:8083`）。

## 2. Docker で review-raven を起動する

### 公開済みイメージを pull

```bash
docker pull ghcr.io/scottlz0310/review-raven:latest
```

### ローカルで build

```bash
docker build -t review-raven:dev .
```

### コンテナを起動

公開済みイメージ:

```bash
docker run -d --name review-raven \
  -p 127.0.0.1:8083:8083 \
  -e BIND_ADDR=0.0.0.0 \
  -v review-raven-data:/data \
  ghcr.io/scottlz0310/review-raven:latest
```

ローカル build イメージ:

```bash
docker run -d --name review-raven \
  -p 127.0.0.1:8083:8083 \
  -e BIND_ADDR=0.0.0.0 \
  -v review-raven-data:/data \
  review-raven:dev
```

任意の環境変数（すべてデフォルト値あり）:

```env
MCP_PORT=8083
BIND_ADDR=127.0.0.1   # Docker で別コンテナ（mcp-gateway 等）から到達させる場合は 0.0.0.0 を指定
LOG_LEVEL=info
SQLITE_PATH=/data/review-raven.db
IN_PROGRESS_THRESHOLD_SEC=30
```

`TRUSTED_COMMENT_AUTHORS` には既定値がない。PR のコメントの本文を、エージェントの文脈に入れてよい GitHub の login のカンマ区切りで、`get_trusted_comment_authors` が公開する。未設定のときは、そのツールが `TRUSTED_AUTHORS_NOT_CONFIGURED` で失敗する（fail-closed）:

```env
TRUSTED_COMMENT_AUTHORS=your-login,thread-owl,codecov
```

### health check

```bash
curl http://127.0.0.1:8083/health
```

PowerShell:

```powershell
Invoke-RestMethod http://127.0.0.1:8083/health
```

期待するレスポンス:

```json
{"status":"ok"}
```

### ログ確認

```bash
docker logs -f review-raven
```

### 停止、再起動、削除

```bash
docker stop review-raven
docker start review-raven
docker rm -f review-raven
```

named volume には SQLite の watch state DB が残る。

```bash
docker volume ls --filter name=review-raven-data
```

ローカル状態を削除したい場合だけ volume を削除する。

```bash
docker volume rm review-raven-data
```

## 3. MCP クライアントを設定する

### Streamable HTTP クライアント（Claude Code、VS Code）

mcp-gateway の URL をクライアントに登録する:

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

クライアントによっては `mcpServers` ではなく `servers`、または `http` ではなく `streamable-http` を使う。URL は変えずにフィールド名を合わせる。

### stdio クライアント（Claude Desktop 等）— mcp-remote 経由

[mcp-remote](https://github.com/geelen/mcp-remote) をブリッジとして使用する:

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

初回接続時に mcp-gateway が OAuth 認可フローを処理する。GitHub にログインして認可する。

## 4. レビュー対応 skill を配置する

`review-raven-thread-owl-cycle` の正本は [Mcp-Docker の SKILL.md](https://github.com/scottlz0310/Mcp-Docker/blob/main/skills/review-raven-thread-owl-cycle/SKILL.md) です。収蔵・配置は Mcp-Docker が管理します。

[Mcp-Docker のリリース](https://github.com/scottlz0310/Mcp-Docker/releases)から skill サブコマンドを備えた v2.18.0 以降を導入し、次を実行します。

```shell
mcp-docker skill install
mcp-docker skill status
```

配置対象は Claude / Copilot / Codex / Antigravity CLI です。対象の絞り込みや更新方法は [Mcp-Docker の利用案内](https://github.com/scottlz0310/Mcp-Docker#readme)を参照してください。手動配置済みの skill は管理外として扱われ、上書き時に確認されます。

このリポジトリの skill テンプレートは廃止しました。`review-raven-thread-owl-cycle` は日本語の正本に統一し、英語版と未使用の Copilot 専用 `pr-review-cycle` 日英版は削除しました。Copilot 用 MCP ツールの利用手順は[ツールドキュメント](watch-tools.ja.md)を参照してください。

## 5. 基本的なレビュー対応

thread-owl のレビューコメントを受けた実装側エージェントへ、対象 PR を指定して依頼します。

```text
$review-raven-thread-owl-cycle owner/repo#123
```

必要な接続と修正・返信・再レビュー依頼の手順は、[skill の正本](https://github.com/scottlz0310/Mcp-Docker/blob/main/skills/review-raven-thread-owl-cycle/SKILL.md)に従ってください。独立 reviewer の起動とマージは別の操作であり、マージにはユーザーの明示許可が必要です。

## トラブルシュート

### `missing_proxy_identity`（401）

リクエストが mcp-gateway を経由せずに `review-raven` に届いた、または gateway が `X-Authenticated-User` を注入するよう設定されていない。すべてのトラフィックが mcp-gateway を通るよう確認する。

### `session_user_mismatch`

同じ MCP session ID が別の GitHub login で使われた。MCP クライアント側の session cache を削除するか、再接続する。

### コンテナは起動したが `/health` が失敗する

ログを確認する。

```bash
docker logs review-raven
```

よくある原因は port の競合または `SQLITE_PATH` の誤り。
