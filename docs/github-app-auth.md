# 専用GitHub App認証（Q8・V4）

## 認証と対象範囲

`REVIEW_RAVEN_AUTH_MODE=github-app` では、review-raven自身が専用Appのinstallation tokenを発行する。gatewayは利用者を認証し、検証済みの`X-Authenticated-User`と、専用の共有Bearerを転送する。review-ravenは共有Bearerを定数時間で照合し、未検証のidentity contextではinstallation tokenを発行しない。受信BearerをGitHub APIへ渡さず、スレッド返信は`review-raven[bot]`名義になる。利用者はgateway経由でのみ接続する。

専用Appのinstallation権限は利用者権限と独立する。初版は個人運用を対象とし、組織`scottlz0310`の全repo（今後作成するrepoを含む）を対象とする。利用者ごとのrepo権限チェックは提供しない。review-ravenを外部公開せず、gatewayと同じ内部ネットワークへ置く。

起動時にApp JWTで`GET /app`・`GET /app/installations/<id>`を呼び、App ID、Appの組織所有者、installation ID・App ID、対象組織、全repoへのインストール、停止状態、必要権限を照合する。不一致やAPIエラーは起動失敗とする。権限の変更後はサービスを再起動して照合する。

## 設定

| 環境変数 | 専用Appモードの値・用途 |
|---|---|
| `REVIEW_RAVEN_AUTH_MODE` | `github-app`。既定値は互換性のため`gateway` |
| `REVIEW_RAVEN_GITHUB_APP_ID` | `5184108` |
| `REVIEW_RAVEN_GITHUB_APP_INSTALLATION_ID` | `169443079` |
| `REVIEW_RAVEN_GITHUB_APP_OWNER` | `scottlz0310` |
| `REVIEW_RAVEN_PROXY_SECRET` | 32文字以上のランダムな専用共有シークレット。同じ値をgatewayとreview-ravenへ保管庫から注入 |
| `REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_B64` | PEM秘密鍵をbase64化した値。保管庫から環境変数へ注入 |
| `REVIEW_RAVEN_GITHUB_APP_PRIVATE_KEY_FILE` | base64注入の代わりに使用するPEMファイルのパス |
| `TRUSTED_COMMENT_AUTHORS` | 現行の投稿者許可リスト。`review-raven`を含める |

鍵はB64・FILEの片方だけを指定する。PKCS#1・PKCS#8のRSA PEMに対応する。鍵・tokenはログ、PR、リポジトリへ書かない。Client ID・Client secret・OAuth callback・専用Appのdevice flowは使用しない。

必要なinstallation権限はMetadata read、Pull requests write、Contents write、Checks read。token発行時もこの4権限だけを要求する。Commit statuses readは現行のcheck-runs取得に使用しないため、tokenに含めない。Contents writeはGraphQL resolveの実測を前提に採用しており、コードへの書き込み権限も含む。

installation tokenをメモリで共有し、有効期限の5分前から次のtool呼び出しで更新する。同時呼び出しによる重複発行を防ぐ。GitHubの401は該当世代のキャッシュを無効化し、元の呼び出しにはエラーを返す。書き込みの自動再試行は行わない。token発行失敗時は古いtoken・利用者tokenで継続せず、原因を呼び出し元へ返す。

専用Appモードでは`REVIEW_RAVEN_GATEWAY_INTERNAL_URL`・`REVIEW_RAVEN_GATEWAY_INTERNAL_SECRET`を設定しない。delegated background accessとの混在は起動時に拒否する。

## MCPの公開範囲

専用Appモードでは次の6 toolを公開する。

- `get_review_threads`
- `reply_to_review_thread`
- `resolve_review_thread`
- `reply_and_resolve_review_thread`
- `list_check_runs_for_sha`
- `get_trusted_comment_authors`

Copilot系8 tool、`diagnose_github_token`、watch resource templateは登録せず、watch managerも起動しない。旧watchのURIは取得・購読できない。`gateway`モードでは従来の15 tool・watch・delegated background accessを維持する。

スレッド取得の既存契約は変えない。投稿者ゲートはmetadata全ページ取得→許可リスト照合→本文取得の順に行う。`include_bodies=false`ではGraphQL queryにも本文を含めない。本文取得tool自体に新しい投稿者ゲートを追加する変更ではない。

reader/Q4、PR conversation投稿tool、PR本文更新/Q3は今回追加しない。現行skillが`{GH}`で行う投稿はgateway bot名義のままなので、Q8全体の完了は後続の経路置換まで保留する。

## 配備とV4検証

1. 専用Appを組織のAll repositoriesへインストールし、必要権限を承認する。秘密鍵を専用の保管庫項目へ登録する。gateway用の鍵は流用しない。
2. 上記の専用App認証に対応するreview-ravenイメージを配備する。Mcp-Docker #386の恒久対応では標準Composeから専用環境変数を渡し、`make pull`・`make restart`（main運用では`make pull-main`・`make restart-main`）で配備する。手動の`COMPOSE_FILE`指定は不要。具体的な設定・運用方法は[Mcp-Dockerの手順書](https://github.com/scottlz0310/Mcp-Docker/blob/main/docs/review-raven-app-auth.md)を参照する。
3. gatewayのreview-raven routeから`upstream_provider_token=true`を外す。`upstream_github_app=true`も付けない（gatewayのAppが使われるため）。routeは次の形とする。

   ```text
   /mcp/review-raven|http://review-raven:8083/mcp|upstream_bearer_token_env=REVIEW_RAVEN_PROXY_SECRET
   ```

   gatewayは受信identityヘッダー・Bearerを上書きし、検証済みidentityと専用の共有Bearerを転送する。review-ravenは共有Bearerが一致した場合だけMCP操作を受け付ける。このBearerはGitHubへ送らない。gatewayのOAuth App設定やGitHub MCP routeは変更しない。共有鍵の未設定・短すぎる値は起動失敗とする。

4. active watchと実行中のレビューがない時間に切り替える。起動ログの照合成功、6 tool、watch非公開、許可リストに`review-raven`があることを確認する。
5. 同じ専用App tokenを使うprobe PRで次を検証し、PR URL・thread ID・comment ID・対象HEAD・結果を証跡として記録する。鍵・tokenは記録しない。

   | 項目 | 合格条件 |
   |---|---|
   | metadataと本文取得 | `include_bodies=false`の本文不在、全ページ取得、ゲート通過後の本文取得が成立 |
   | 返信 | 成功し、GitHub上の投稿者が`review-raven[bot]` |
   | resolve | 実在する未解決threadを解決できる |
   | 返信後resolve | 両操作が成功し、再取得で解決状態を確認できる |
   | private checks | private repoの固定SHAでcheck-runsを取得できる |
   | 対象範囲 | installation権限は組織内に限定する。組織外の公開repo読み取りは許容する（2026-10-09合意） |
   | token更新 | 新規発行・期限前の更新後も読み取りが成立 |
   | 投稿者ゲート | 次サイクルで専用botの返信を信頼できる |
   | client接続 | 使用中の各CLIから認証・tool呼び出しが成立 |

6. probe検証と運用一巡が成立した後に、gatewayのContents write依存を棚卸しする。gatewayをContents readへ戻す操作は別段階とし、新規tokenで専用Appのresolve、gatewayのログイン・GitHub MCP・残存投稿経路を再確認する。

2026-10-09に6 toolの公開範囲、不正Bearerの拒否、metadata・本文取得、専用botの返信・resolve、組織内private repoのChecks取得を実機で確認した。検証用PR #140は未マージでクローズした。[実測証跡](https://github.com/scottlz0310/Mcp-Docker/blob/feat/review-raven-app-default/docs/review-raven-app-v4.md)を参照する。期限越えtoken更新と他クライアントの実操作は未実測で、gateway Contents縮小は後続とする。

## 切り戻し

`REVIEW_RAVEN_AUTH_MODE=gateway`に戻し、routeの`upstream_provider_token=true`と、必要なら既存のdelegated background access設定を戻してreview-ravenを再作成する。DB・既存コメント・許可リストは維持する。専用botが投稿済みなら`review-raven`を許可リストから外さない。gateway Contents縮小前なら従来のresolve経路に戻せる。縮小後は従来のresolveを使えると仮定せず、権限の再承認が必要になる可能性を確認する。

## 公式資料

- [installation tokenの発行・権限・有効期限](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app)
- [review commentの権限](https://docs.github.com/en/rest/pulls/comments)
- [check-runsの権限](https://docs.github.com/en/rest/checks/runs)
