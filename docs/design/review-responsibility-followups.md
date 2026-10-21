# #144 後続実装Issue案

状態: 未起票。承認された本作業の範囲は設計PRとIssue案まで。以下はIssue本文に転記できる単位であり、作成済みとは扱わない。
契約の根拠は[責務設計](review-responsibility-boundaries.md)。実装開始前に本設計のレビュー結果を反映する。

## R1 — review-raven: 依頼台帳・公開受付・進捗Resource

目的: 初回・再依頼を同じ窓口で扱い、対象HEADを固定した依頼を永続化する。

受け入れ条件:

- `request_review` / `get_review_request` / `review-raven://requests/{cycleId}` とschema・エラーcodeを追加する。
- 専用Appモードの公開面、利用者→repo ACL、read/list/listenの認可を追加し、未設定・他PRを拒否する。
- SQLite台帳・transactional outbox・round・冪等性・30日保持・失効URIを実装する。
- O1への配送ACKまでpendingとし、失敗を握りつぶさず構造化して公開する。
- 同HEAD再依頼、新HEAD、競合payload、再起動、配送重複、権限拒否を検証する。
- 認可された `cancel_review_request`、revision競合検知、取消outboxとrequested/confirmedを実装する。owlの無効化ACKまではterminal=falseを維持する。
- 結果なし異常終了・通常配送の再送上限到達を起点に、明示取消確定→同PR/HEAD/reason・新key→新cycle/roundの復旧を検証する。旧依頼key・旧webhook sourceIdは旧cycleを返す。
- 取消ACK消失・再起動・再送上限時の同取消keyによる明示的な配送再開、取消中の再依頼結合、権限拒否、結果投稿との競合を検証する。

依存: O1・I1とschemaを先に確定し、相互接続の完了後opt-inで有効化。

## O1 — thread-owl: 内部候補受付・結果参照・webhook相関

目的: reviewer内部queueを維持し、依頼cycleと結果を再起動後も対応付ける。

受け入れ条件:

- 内部面をagent公開面から分離し、candidateのcycle/round/HEADを固定する。
- 永続受理台帳、結果索引とoutbox、投稿成功直後の停止からの回復を実装する。
- webhookのsourceIdをraven共通受付へ配送し、二重queue投入・ループを防ぐ。
- reviewer起動時から結果投稿までcycleを束縛し、投稿時の現在round取得に頼らない。
- queue再依頼優先とstatusリセットの不整合、古い実行の遅着、HEAD移動、配送再送を検証する。
- 旧enqueue/queue/statusを維持し、新旧を混同しないcapabilityを公開する。
- 内部 `cancel_review_candidate` に永続取消記録を追加し、未受理cycleを含めて旧候補の起動・新規投稿を拒否する。GitHub書込みが進行中・結果不明なら、照合前に取消ACKを返さない。
- 取消先着後の旧受付、ACK消失・重複・再起動、旧candidateの遅延起動/投稿、結果投稿先着との競合を検証する。Squirrelのbusy制御と既存起動契約は維持する。

依存: R1・I1と同じ契約fixtureを使う。旧API撤去は含めない。

## S1 — squirrel-notifier#497: 観測publisherとリモート途中報告

既存Issueを拡張する案であり、重複Issueは作らない。

受け入れ条件:

- Squirrelからravenへ状態変化・60秒heartbeatを送信するpublisherを追加する。
- candidateId/cycleId/HEADとproducerEpoch/sequenceを送信用envelopeで対応付ける。公開JSON v1と内部storeの契約を維持する。
- raven側で認証主体・割当PR・順序・鮮度を検証し、unknown/stale/stopped/finishedを区別する。
- 起動失敗・exitCode=0かつ結果なし・結果先着・再接続・旧epoch・別PR偽装を検証する。
- R2のCLIとI2のskillで、CI待ち→起動待ち→実行中→結果を同一セッションへ途中報告する。
- reviewerの選択変更・外部起動APIを追加しない。

依存: R1/O1/I1の受付・相関・認証、R2の逐次配送。結果の正本はGitHub上のthread-owl投稿とし、owlが対応索引を持つ。別サービスは追加しない。

## R2 — mcp-resource-subscriber: 汎用逐次購読出力

目的: 最初の途中更新で終了せず、呼び出し元へ更新を配送する。

受け入れ条件:

- opt-inのJSONLとJSON Pointerによる明示終了条件を追加し、オプション名と行schemaを確定する。
- 既存`--json`単一出力、call、認証キャッシュ、終了コードを維持する。
- ack・URI受理・直後read・通知read・flush・切断・timeout・既完了を実HTTP/サブプロセスで検証する。
- PR/HEAD/Verdictの業務判断や独立reviewer起動をCLIに追加しない。
- 4段階更新で最初の行が終了前に届き、terminalまでプロセスが残ることを検証する。

依存: R1のsnapshotサンプル。待機API内製化やCLI廃止は含めない。

## I1 — Mcp-Docker: 内部連携の配備・サービス主体・永続volume

受け入れ条件:

- agent面と内部面を分離したroute、主体→操作→repo scope、secret注入・rotation・失効手順を用意する。
- raven/ owlの永続volume、R1/O1のmode選択、切り戻し手順を追加する。
- gatewayの既存能力でサービス主体を表現できるかを検証し、不足時だけgatewayの低レベル認証Issueを分離する。レビュー業務状態はgatewayに追加しない。
- 偽装identity、対象外repo、古いcredential、agentから内部面へのアクセスを拒否する相互接続テストを行う。

依存: R1/O1の内部schema。現在のApp権限を勝手に拡張しない。

## I2 — Mcp-Docker: skill・reviewgate・登録プロファイルの段階切替

受け入れ条件:

- reviewed側の受付・購読先をravenへ切替え、reviewer側にcycle束縛を追加する。
- catalog revision・reviewgateと両skillの完了規則を同期する。
- CI逆依存を記録して維持し、恒久配置はD3再開時の別判断とする。
- 4agent・各PCの配布と実PRの初回→修正→再依頼、途中報告、旧経路切り戻しを確認する。
- profile/registerはdry-run差分を示し、未移行consumerのrouteを削除しない。

依存: R1/O1/S1/R2/I1。自己レビュー・自動マージは追加しない。

## B1 — review-raven / thread-owl: role別readerの本文取得ゲート

受け入れ条件:

- review thread/body/issue commentのmetadata全ページ→投稿者検証→本文取得をtoolで強制する。
- 検証後のコメント追加・編集を検出し、allowlistを迂回しない。
- 両roleで同じ信頼境界を保ち、skillと本文APIの利用先を置換する。

依存: #144の公開面分担。両repoの実装Issueに分け、契約fixtureは共有する。進捗購読の公開とは独立して検証可能。

## Z1 — 各repo: 旧公開経路の撤去

受け入れ条件:

- 段階3の配布と全consumer監査、30日間の旧入口利用なしを証跡化する。
- 旧enqueue/status、旧gateway/Copilot、GitHub MCP依存をそれぞれ棚卸し、依存解消が確認できた経路だけを別PRで廃止する。
- reviewer内部queue、旧CLI単一JSON、既存Squirrel公開JSONは一括撤去しない。

依存: I2完了。マージ・公開・配備・権限変更の許可は実施時に別途必要。
