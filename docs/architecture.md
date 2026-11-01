# レビュー基盤のアーキテクチャ

review-ravenはレビューを受けて修正する側の窓口であり、独立レビューとVerdictはthread-owlが担当する。Squirrelは起動・CI待ち・保留・再開・通知と公開状態の観測を担う。resource-bridge-cliは購読・read・tool呼出しと同一セッションへの構造化配送、gatewayはクライアント認証・identity伝達・接続、Mcp-Dockerはskillと環境の管理・配布を担当する。

2026-11-01の利用者決定により、認証付き内部連携は廃案とし専用の受付・台帳・配送・中継を撤去する。[責務と廃止範囲](design/review-responsibility-boundaries.md)、[関連Issue](design/review-responsibility-followups.md)を参照する。

## 維持する現行運用

- Appモード: 各GitHub Appのidentityを使う。コメントreaderが投稿者を検証し、thread取得・返信・resolve・SHA固定check runs・定型投稿を公開する。[認証](github-app-auth.md)、[reader契約](design/contracts/reader/README.md)。
- 依頼: thread-owlのenqueue_reviewと既存queueを維持する。webhook構成では二重enqueueしない。queue登録と独立reviewer起動は別操作である。
- 待機: resource-bridge-cliでreview://statusのack/要求URI/PR/最終status/HEADを確認し、未解決threadと独立Verdictを別途確認する。
- 進捗: 同一PCのSquirrel公開review-status.jsonをwatch-review-statusで観測する。finished/exitCodeはVerdictの代わりにならない。
- 既存gatewayモード: Copilot要求・cycle・wait・watch等とSQLiteのtrigger_log/review_watchは共有機能として残す。本撤去に便乗して削除しない。

新しいユーザーアカウント・サービス用token・公開webhookの追加は不要。PR説明本文の更新APIは[別のB3設計](design/pr-description-update.md)であり、まだ実装されていない。GitHub MCPの撤去も本作業の対象にしない。
