# レビュー状態の観測に関する比較履歴

進捗観測はSquirrelの公開JSONと外部CLIで行い、完了は既存review statusと対象HEADの独立Verdictで確認する。プロセス終了とレビュー完了を同一視しない。

横断文書の正本は利用者のreviewers-forestワークスペースへ統合した。`~/Dropbox/src/reviewers-forest/docs/guidebook/review-infra-operations.md`を参照する。現行の進捗・未完了事項は同ワークスペースの`docs/hand-off/cross.md`だけに保持し、本書へ複製しない。

このrepoのAPI/認証仕様は[reader契約](contracts/reader/README.md)、[専用App認証](../github-app-auth.md)、[PR context](../pr-context.md)に維持する。横断の比較履歴・移管対応表はreviewers-forestの`docs/reference/review-infra-document-map.md`に集約した。

[移管前の履歴](https://github.com/scottlz0310/review-raven/blob/b3437d84fb81693c1f2b74786111a138d974f3a2/docs/design/review-progress-local-status.md)は参照できる。そこにある未決・凍結・旧版・過去の許可を現在へ適用しない。
