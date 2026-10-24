# review-raven タスク

## #144 責務整理

- [x] 現行6repoのtool/resource/webhook/storeとconsumerを実査し、越境・正本・認証・目標データフロー・段階移行を設計文書へ記録する。
- [x] architectureを現行方針へ更新し、repo別の後続実装Issue案を整理する。
- [x] 英語版architectureを独立した英語の説明として維持し、日本語版との内容・相互リンクと今回の英語CHANGELOG記載を揃える。
- [x] PR #146のquestion: 結果なし失敗cycleの明示取消・無効化確認・同HEAD新round受付と、R1/O1の受け入れテストを定義する。
- [x] 設計レビュー後に後続Issueを実起票し、squirrel-notifier#497へ依存契約を反映する（2026-10-24、Z1を除く7件を起票し#497へ設計コメントを追記）。
- [x] R1/O1/I1共通の契約v1（JSON Schema・golden fixture・共通シナリオ・整合テスト）を[docs/design/contracts](design/contracts/README.md)に確定する。
- [x] R1段階1（[#147](https://github.com/scottlz0310/review-raven/issues/147)）：台帳コア（SQLite・冪等性・結合・superseded・取消・結果参照・観測・snapshot射影）を`internal/reviewrequest`に実装する。MCPには公開しない。
- [x] R1段階2：公開MCP（`request_review` / `get_review_request` / `cancel_review_request`）・`review-raven://requests/{cycleId}` Resource（読取・購読）・repo ACLによる認可を、専用Appモード限定のopt-inで追加する。
- [x] R1段階3a：内部面（`ingest_review_candidate` / `publish_review_result` / `publish_review_observation`）を、別MCPサーバー・専用proxy秘密・サービス主体ACL（subject→操作→repo、producer割当）で追加する。
- [x] R1段階3b-1：outboxの配送worker（`DeliverDue` / `RunDelivery`。再送・バックオフ・上限、`RecordDispatch*` / `ConfirmCancel` / `RecordCancelRejected`への接続、`Deliverer`インターフェース）を追加する。
- [ ] R1段階3b-2：thread-owl向けのMCPクライアント（`accept_review_candidate` / `cancel_review_candidate`）とサーバーへの配線（opt-in、設定のfail-fast）を追加し、O1（thread-owl#279）とシナリオで相互接続テストを行う。
- [ ] 新API・永続化・publisher・CLI逐次出力・skill切替を後続Issueで実装・配布・実機検証する。

設計: [責務境界](design/review-responsibility-boundaries.md)、[後続Issue案](design/review-responsibility-followups.md)。#144全体の完了と製品移行完了を混同しない。

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
