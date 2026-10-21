# アーキテクチャ

[English](architecture.md)

## review-raven の位置づけ

review-raven はレビューを依頼し、受けて修正する側のMCP serverである。初回依頼・再依頼、依頼したレビューの進捗／完了の取得・購読、指摘取得・返信・resolveを公開窓口として担う。

この責務は目標を含む。現行APIをすべて実装済みという意味ではない。現行と目標の差、コード調査根拠、API・状態・認証契約と段階移行は [#144の責務設計](design/review-responsibility-boundaries.md) を参照する。[後続Issue案](design/review-responsibility-followups.md)は未起票。

## 6構成の責務

| コンポーネント | 所有する責務 |
|---|---|
| review-raven | 依頼受付・再依頼・依頼状態と進捗の公開・修正対応 |
| thread-owl | reviewer対象検証・内部候補queue・指摘と結果の投稿 |
| squirrel-notifier | 起動・CI待ち・保留・再開・通知・実行状態の観測 |
| mcp-resource-subscriber / resource-bridge-cli | 購読・更新受信・read・tool呼出し・同一セッションへの構造化配送 |
| mcp-gateway | クライアント認証・検証済みidentity伝達・接続・routing |
| Mcp-Docker | skill・instructionの正本と配布・compose・route・CLI登録 |

review-ravenはレビューそのものを行わない。thread-owlのreviewer内部queueは維持する。Squirrelのプロセス終了はレビュー結果ではない。レビュー結果の正本はGitHub上のthread-owl投稿とし、依頼側で独立したVerdictを作らない。

## 現行実装

専用GitHub Appモードはreviewed用6 tool（thread取得・返信・resolve・reply_and_resolve、SHA固定check runs、trusted authors）を公開する。review-ravenがinstallation tokenを発行・更新し、gatewayの検証済みidentityと専用共有Bearerを要求する。詳細は [App認証](github-app-auth.md) を参照する。

既定のgatewayモードにはCopilot要求・状態・cycle・wait・watch・診断も残る。watchのresourceは `review-raven://watch/{id}`、永続状態はSQLiteの `trigger_log` / `review_watch`。専用Appではこれらのtool/resourceを非公開とする。新しいレビュー依頼の永続台帳はまだない。

`get_review_threads(include_bodies=false)`は本文なしmetadataを返す。省略時は本文を返すため、本文取得前の投稿者ゲートは現行skillの責務でもある。toolによる強制readerは後続課題として扱う。

## 現行の依頼・完了待機

- `--mcp-http`では実装側がthread-owlの `enqueue_review` を呼び、外部CLI `resource-bridge-cli` で `review://status/{owner}/{repo}/{prNumber}` を購読する。
- `--webhook-mcp-http`ではGitHubイベントから候補が投入される。手動enqueueを重ねない。webhook投入はstatusのpending初期化を行わないため、手動経路と同じ完了待機契約は前提にしない。
- 再依頼コメントは現在のskillがGitHub MCP等を通じて投稿する。review-ravenに汎用 `add_issue_comment` があるわけではない。コメントだけでqueue登録・reviewer起動・完了を保証しない。
- URI・ack・対象PR・最終status・固定HEADと結果HEADと現在HEADの一致を確認し、完了後は未解決threadを実取得する。timeoutや状態消失を成功扱いにしない。
- queue登録はreviewer起動ではない。独立reviewerは既存Squirrel起動契約または別CLIへの明示指示によって実行する。実装セッションで自己レビューしない。

この経路は#144の段階移行まで維持する。CLIをMCP serverとして登録・探索せず、購読内製化全体やCLI廃止は再開しない。

## 移行先

review-ravenがcycle/HEADを固定して依頼を永続受付し、thread-owlへ内部候補を配送する。Squirrelの実行観測とthread-owlの結果参照を受け取り、依頼ごとのResourceで公開する。初回依頼→途中報告→完了→修正・返信・resolve→再依頼まで同じ窓口を使う。API名・URI・認可・重複と遅延・保持・切り戻し条件は [設計文書](design/review-responsibility-boundaries.md) に定義する。

reviewerが `list_check_runs_for_sha` をreview-ravenから読む逆依存は記録して維持する。恒久CI取得元はD3の保留判断と合わせて別途決める。GitHub MCPの撤去はreader・狭い投稿API・利用先の置換と実機検証後に判断する。

旧 `copilot-review://watch/{id}` と `COPILOT_REVIEW_*` 環境変数は非対応。既存watchやCopilot系APIの廃止は利用者監査後の別PRとする。
