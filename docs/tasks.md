# review-raven タスク

- [x] Q8・V4: 専用Appの秘密鍵注入・配備・probe PRの主要実機検証（`docs/github-app-auth.md`）。
- [ ] V4残件: 期限越えtoken更新・他クライアント実操作。gateway Contents縮小は後続。

## 実装済み（未マージ）

- [x] Q8・V4: 専用Appのinstallation token認証、起動時の所有先・全repo・権限照合、期限前更新、reviewed用6 toolへの公開限定、既存gatewayモードの回帰検証を追加する。

- [x] [#134](https://github.com/scottlz0310/review-raven/issues/134): 必須コメント投稿者の許可リストを、環境変数 `TRUSTED_COMMENT_AUTHORS` から read-only のツール `get_trusted_comment_authors` で公開する（reviewed skill の直書きの廃止の段 1）。未設定は `TRUSTED_AUTHORS_NOT_CONFIGURED` で fail-closed、不正な要素は起動時に fail-fast。

- [x] [#129](https://github.com/scottlz0310/review-raven/issues/129): コミット SHA を入力に check runs（`head_sha` / `status` / `conclusion` / `app`）を返す read-only ツール `list_check_runs_for_sha` を追加し、skill の CI 判定で SHA 照合を MCP から行えるようにした。

- [x] [#124](https://github.com/scottlz0310/review-raven/issues/124): `get_review_threads` に本文を選択しないメタデータ射影 API を追加し、投稿者ゲートの事前検査を MCP から利用できるようにした。

- [x] #123 一周目レビュー対応: 英語版ドキュメントの日本語混入を修正し、stale-guard バグ報告の既存参照切れ5件を解消。

- [x] [#122](https://github.com/scottlz0310/review-raven/issues/122): skill の収蔵・配置案内を Mcp-Docker へ統一。`review-raven-thread-owl-cycle` 日英版と未使用の `pr-review-cycle` 日英版を削除し、英語版を廃止。

## 過去の記録

- [tasks-legacy-2026_06_08.md](archive/tasks-legacy-2026_06_08.md) — Mcp-Docker 時代の旧タスク一覧（async watch redesign 完了済み）
- [tasks-doc-generalization-2026_06_08.md](archive/tasks-doc-generalization-2026_06_08.md) — ドキュメント一般化計画（PR#65 で完了）
