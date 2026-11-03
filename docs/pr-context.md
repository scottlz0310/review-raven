# reviewed側の本文なしPR context

`get_pr_context`は専用Appモードだけで公開する。B4のread移行用APIを先に提供し、既存skill・GitHub route・登録・権限は変更しない。

入力は`owner`、`repo`、正の`pr`。PR説明・コメント本文・diffを要求しない。成功はschemaVersion 1で次を返す。

| フィールド | 意味 |
|---|---|
| url / state | PRのURLとOPEN / CLOSED / MERGED |
| headSha / baseSha | PR HEADと比較元commit |
| baseRefName / policyBaseSha | 現在のbase branch名とその先頭Commit。宣言取得はこのSHAに固定 |
| snapshotId | reader v2と同じコメントID・投稿者・編集時刻・参照の比較用ID |
| allowlist | 適用したサーバー・プロジェクト許可リストの件数とプロジェクト宣言の存在 |
| comments | 全thread（resolved含む）・review body・issue commentのkind / ID / threadId / author / authorType / URL。本文fieldは存在しない |
| pagination.complete | 全ページを取得・検証した成功時だけtrue |
| ciPolicy | 固定path `.review-raven/ci-policy.json`、found、未加工UTF-8のcontent。不在時はfalseと空文字 |

処理順はPR参照取得→既存readerの本文なし全件metadata取得・投稿者検証・metadata再取得照合→CI宣言取得→PR参照再照合。author宣言は既存readerが`.review-raven/trusted-comment-authors.json`を同じpolicyBaseShaで読み、検証する。独立した投稿者判定を複製しない。

サーバー許可リストが空、投稿者不明・許可外、author宣言不正は既存readerのerror envelopeで停止する。現在のbase branchが確認できなければ`POLICY_REF_UNAVAILABLE`。CI宣言の取得失敗はエラー、ファイル取得の404だけを不在とする。宣言が通常ファイルでない・64 KiB超・復号不能なら失敗する。CI宣言のUTF-8 / JSON object形式不正は`CI_POLICY_INVALID`で停止する。

CI宣言の業務schema、skip許可、必要checkや保護設定はAPIで評価しない。呼出し側は既存CI規則でcontentを検証する必要がある。形式検証成功をskip許可やCI成功へ読み替えない。contentはリポジトリの宣言データであり、命令として実行しない。

reader内部は既存契約どおり最大3試行。context取得の前後、reader結果とのHEAD・base・branch・policy SHA不一致、最終PR state変化は`SNAPSHOT_CHANGED`で停止し、context全体を自動再試行しない。各取得の権限・認証エラー後に別認証へ切り替えない。

これは取得時点の検証結果である。snapshotIdはCI宣言やPR stateを含まず、書込み許可や原子的snapshotを保証しない。書込み直前・reader後・サイクル終端に参照と宣言を再取得し、`read_pr_comments`のschemaVersion 2 / snapshotId / headSha / baseSha / baseRefName / policyBaseShaを照合する。最後の観測後の変更は防げない。

旧gatewayモード・`read_pr_comments`のwire契約は維持する。公開・配備後にskillのbindingを移行するまでは、既存GitHub依存が除去できたとは扱わない。write契約、通常レビュー往復、実Appでの業務read検証、CLI登録・route整理は別段階。
