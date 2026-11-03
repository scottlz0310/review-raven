# レビュー基盤の責務と内部連携廃止

review-ravenは実装側の指摘取得・返信・resolve・定型投稿を担当する。認証付き内部連携の専用受付・台帳・配送・進捗中継は廃案とした。既存App認証・投稿者検証済みreader・thread-owlのqueue/status・Squirrel公開JSONを維持する。

旧gatewayモードのCopilot/watchと共有SQLiteのtrigger_log/review_watchは別機能として維持する。既存DBのデータを削除するmigrationは追加しない。

横断文書の正本は利用者のreviewers-forestワークスペースへ統合した。`~/Dropbox/src/reviewers-forest/project_review-infra-architecture.md`を参照する。現行の進捗・未完了事項は同ワークスペースの`docs/hand-off/cross.md`だけに保持し、本書へ複製しない。

このrepoのAPI/認証仕様は[reader契約](contracts/reader/README.md)、[専用App認証](../github-app-auth.md)、[PR context](../pr-context.md)に維持する。横断の比較履歴・移管対応表はreviewers-forestの`docs/reference/review-infra-document-map.md`に集約した。

[移管前の履歴](https://github.com/scottlz0310/review-raven/blob/b3437d84fb81693c1f2b74786111a138d974f3a2/docs/design/review-responsibility-boundaries.md)は参照できる。そこにある未決・凍結・旧版・過去の許可を現在へ適用しない。
