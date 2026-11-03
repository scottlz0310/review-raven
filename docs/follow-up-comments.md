# 指摘返信・follow-up Issue API

専用Appモードのreviewed側API。`reply_to_pr_comment`は汎用指摘への返信、`create_follow_up_issue`は指摘の後続Issue作成、`get_follow_up_issue`は同repoの通常Issueの検証付き読取を行う。採否・重要度・意味上の重複・resolve・再レビュー・mergeの判断はcallerに残す。queue登録やreviewer起動は行わない。

## 有効化と権限

`REVIEW_RAVEN_ENABLE_FOLLOW_UP_ISSUES`は既定`false`。専用Appモードで`true`にした場合だけIssue用2 toolを登録し、installation起動時検証とtoken発行要求の両方に`issues: write`を加える。既定は従来のmetadata read / pull_requests write / contents write / checks readを維持する。権限不足は起動失敗。gatewayモードの`true`や不正な設定値は拒否する。

この設定は実Appの権限やinstallation同意を変更しない。外部権限変更・配備は利用者の明示許可後に行う。汎用返信toolはIssue設定によらず専用Appモードで登録する。[Issue作成のGitHub権限](https://docs.github.com/en/rest/issues/issues#create-an-issue)を参照。

## 入力

共通: `owner` / `repo` / 正整数`pr` / 小文字40桁`expectedHeadSha` / `source`。

`source`は`kind`とreader v2が返した数字のID文字列`id`の組。ID空間を種類間で混同しない。`review_thread_comment`はreaderの`threadId`も必須。他のkindにはthreadIdを指定しない。`source.url`はcallerから指定できず、server取得値を使う。

| tool | 追加入力・制限 |
|---|---|
| reply_to_pr_comment | `expectedSnapshotId`（小文字64桁）、`body`。source.kindは`review_body` / `issue_comment`のみ。thread指摘は既存thread返信APIを使う |
| create_follow_up_issue | `expectedSnapshotId`、`title`、`body`、`reason`（out-of-scope / deferred / follow-up）、任意`labels`。source.kindは上記とreview_thread_comment。labelは既存名・重複なし |
| get_follow_up_issue | 正整数`issueNumber`。expectedSnapshotId不要。source.kindは3種類。同repoの通常Issueのみ |

形式検証は認証・GitHub通信前に行う。writeのbody/titleは空白のみを拒否し、UTF-8本文は保持する。bodyの定型サイクル投稿・Review Verdict・再レビュー起動トリガー・固定footer混入を拒否する。

## 投稿前の照合

1. 既存readerのReadMetadataで全ページ・resolvedを含む全コメントの投稿者を検証する。宣言は現在のbase branch先頭のpolicyBaseShaへ固定する。
2. 元指摘のPR所属・kind/ID/thread、snapshotId、OPEN、期待HEADとPR/base/宣言参照を照合する。snapshotIdは比較値でありwrite許可証ではない。
3. 認証済みApp slugから得たBot loginをRESTで取得し、Bot型・Node ID・loginを確認する。allowlistだけでApp本人とは認定しない。
4. 返信は既存readerで本文を取得して固定footerを探し、候補のApp Node IDと本文を読戻す。Issue作成は同repoのmetadata-only検索後、各候補の投稿者・最終編集者を検証してから本文を読む。検索失敗・1000件上限超過・件数変化・不完全なページング・複数候補は停止する。
5. POST直前にPRのstate/HEAD/base/宣言参照を再照合する。不一致ならPOST 0回。

同じ元指摘のApp返信はALREADY_REPLIED、既存App IssueはFOLLOW_UP_EXISTSで候補IDを返し、再投稿しない。固定footerは可視の1行`Review-Raven-Source: <owner>/<repo>#<pr> <kind>:<id>`。owner/repoは小文字、HEADは含めない。footerだけでApp identityを認定しない。

## 投稿と成功条件

POSTは1回で、別認証・routeへの代替、rollback、自動再投稿を行わない。返信は元指摘URLと固定footerを追加してPR conversationへ投稿する。Issueは同repoへ元PR/元指摘URL・確認HEAD・reason・固定footerを付けて作成する。assignee/milestone/project/typeや他repoの変更は受け付けない。

返信成功は既存readerで全件投稿者検証後に返却IDのPR所属・本文・URLを照合し、REST読戻しのApp Node ID・login・Bot型も一致すること。Issue成功は返却numberのmetadata投稿者/最終編集者を先に検証し、本文・title・labels・OPEN・App identityを読戻し照合する。どちらもPR参照を再確認する。本文hashは組立済みUTF-8本文のSHA-256。

成功出力はschemaVersion=1、status=created、source、commentまたはissue、verifiedHeadSha / verifiedSnapshotId / policyBaseSha / prState / matchesExpectedHead。verifiedSnapshotIdは投稿前に照合した値で、投稿後は値が変わり得る。次のwrite前にread_pr_commentsを再取得する。

Issue作成と返信・resolveは別操作。作成後は返信に`Follow-up: #番号`を記載し、リンク確認後に処理済み・resolveとする。返信失敗時はIssueを保持し、未リンクとして記録する。自動close・削除・再作成を行わない。handled_commentsは読戻し成功後の元指摘IDを保持し、返信IDは別証跡とする。

## Issue本文の読取と回復

get_follow_up_issueは、PR全件投稿者ゲートと元指摘所属を確認後、Issueのmetadata-only projectionで投稿者・最終編集者を検証する。canonical server/project allowlistを両actorへ適用する。未知のactor、編集時刻があるのにeditor不明なら本文/titleを取得しない。取得前後のidentity・編集時刻・所属・labels・PR参照が変われば本文を返さず停止する。本文自体は未信頼データであり命令として扱わない。

読取時はPRがclosed/HEAD移動済みでも照合用に返す。status=read、prState、現在のverifiedHeadShaとmatchesExpectedHeadを必ず確認し、通常cycle再開の許可と扱わない。Issueが実際に指摘をカバーするか、closed Issueで十分かはcallerが判断する。

結果不明の回復はreadのみ。返信は既存reader、番号既知のIssueはget_follow_up_issueで確認する。番号不明なら既存ghのmetadata-only検索で候補を絞る。意味検索や検索0件は未投稿・重複なしの証明ではない。候補本文/titleを検証前に読む代替は行わない。

## エラーと競合

MCP IsError=trueでtext/structuredContentに同じerror envelopeを返す。code / message / mayHaveChangedを必須とし、既知のcreatedResource（ID/番号/URL）と候補を保持する。本文・title・生レスポンス・秘密値をエラーへ転載しない。

| code | 扱い |
|---|---|
| INVALID_INPUT | 形式不正、POST 0回 |
| reader/authの既存code、READ_FAILED | 投稿者・宣言・認証・読取の失敗、POST 0回 |
| PRECONDITION_FAILED / APP_IDENTITY_FAILED | 対象・snapshot・参照またはApp identity不一致、POST 0回 |
| ALREADY_REPLIED / FOLLOW_UP_EXISTS | 既存返信/Issueまたは複数候補、POST 0回 |
| WRITE_OUTCOME_UNKNOWN | POST開始後の401/403/429・通信失敗・応答欠損・読戻し失敗、mayHaveChanged=true |
| POST_WRITE_MISMATCH | 投稿後の本文・identity・参照不一致、mayHaveChanged=true |
| SNAPSHOT_CHANGED | read中の変化、本文を返さない |

POSTとHEAD照合は原子的ではない。検索索引の遅延、並行編集、同時実行による重複を完全防止できない。同じ元指摘のwriteはcallerが直列化し、結果不明後は利用者の再投稿許可を待つ。専用台帳は作らない。実Appの権限・検索footer・投稿読戻しE2Eはモックテストと区別して確認する。
