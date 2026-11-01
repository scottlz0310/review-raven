# レビュー基盤の責務と内部連携の廃止（#144 / #159）

2026-11-01の利用者決定により、新アカウントによるサービス主体を前提にした認証付き内部連携は廃案・撤去とする。凍結を解除して有効化する予定はない。

## 現行の経路

- 身元・結果: thread-owl / review-ravenがそれぞれのGitHub AppでPRへ投稿し、投稿者検証済みreaderで取得する。Verdictは対象HEADと照合する。
- 進捗: 同一PCのSquirrelが公開するreview-status.jsonをresource-bridge-cli watch-review-statusで読む。プロセス終了はVerdictではない。
- 依頼・完了待機: 既存thread-owl enqueue_reviewとreview://statusを維持する。queue登録だけで独立reviewer起動済みとは扱わない。
- 修正対応: review-ravenが取得・返信・resolve・定型サマリ/再依頼投稿を担当する。実装セッションで自己レビューしない。
- 配布・認証: Mcp-Dockerがskill/環境を管理し、gatewayは既存クライアント認証・identity伝達・接続を担う。新しいGitHubユーザーアカウントやサービス用資格情報を要求しない。

## 撤去範囲と維持する範囲

review-ravenの依頼台帳・outbox・request_review/get_review_request/cancel_review_request・requests resource、thread-owl/Squirrel向け内部APIと配送worker、専用ACL/設定・共通契約を撤去する。利用者/インストール先に対する既存App認証、コメントreaderの契約、SHA固定CI取得、定型投稿は維持する。

既存gatewayモードのCopilot/watch機能や共有SQLiteのtrigger_log/review_watchは別機能なので削除しない。既存DBのデータを自動削除するmigrationも追加しない。

## 判断の根拠と未確認

通常の実レビューで、App名義・本文・HEADの読戻し、CI待ち/行列待ち/実行/終了の観測、Verdictと同HEAD CI・未解決threadの独立確認ができている。同じPCで完結することが要件であり、別ホストの観測を必須にしない。

20分超のround・自然quota解除・他CLIの通知制約は運用品質の未確認として残す。それらが認証付き内部連携の必要性を示す証拠にはなっていない。判定と撤去の追跡は[#159](https://github.com/scottlz0310/review-raven/issues/159)、横断進捗の正本はDropboxのreviewers-forest/docs/hand-off/cross.md。

旧API・保持/取消/配送案は[撤去前の設計履歴](https://github.com/scottlz0310/review-raven/blob/68332ad02e4ef9f6b837aca3ad83b8eea7b1316d/docs/design/review-responsibility-boundaries.md)に残す。実装予定として再利用しない。PR本文更新のB3は[別設計](pr-description-update.md)であり、内部連携廃止と混同しない。
