# 再レビュー依頼・対応サマリの投稿（B2）

`post_review_cycle_comment` は、proxy検証済みの専用GitHub Appモードだけで公開する。既存のApp installation権限を使い、gatewayモードや内部連携の設定は変更しない。

## 入出力

入力は `owner`・`repo`・正の `pr`・40桁の小文字 `headSha`・`kind`・`body`。`kind` は次の2値だけ。

| kind | 本文先頭の固定行 |
|---|---|
| `re_review_request` | `@thread-owl re-review requested` |
| `cycle_summary` | `## レビュー対応サマリ（thread-owl）` |

固定行の後に空行と対応内容を置き、末尾には既存skillと同じサイクル状態を1組だけ置く。本文のCRLFはLFへ揃える。

```text
### サイクル状態
- cycles_done: 1
- max_cycles: 3
- expected_head: `<headShaと同じ40桁SHA>`
- handled_comments: 123, 456
```

`cycles_done`は非負整数、`max_cycles`は正整数。処理済みIDは数値のカンマ+空白区切りで、空の場合は値なし・空白・`なし`を受け付ける。上限・修正採否・未解決数・再依頼の可否はskillが判断する。toolがサイクルを進めたり、判断を上書きしたりはしない。

不明なkind、用途と異なる見出し、状態の欠落/重複、状態HEADの不一致、状態後の追記、`Review Verdict`の混入、サマリ中の再依頼トリガーは認証・GitHub通信の前に拒否する。reviewerのVerdictや任意のPRコメントを投稿する入口として使わない。

出力は `commentId`（数値文字列）・`url`・GitHubが返した実投稿者の `author`・確認に用いた `headSha`。返却authorだけで完了とせず、`read_pr_comments`で同じPR・HEAD・返却ID・正規化author・本文を読戻して確認する。今回APIだけを追加しても、Mcp-Dockerのskill利用先移行・配布・実投稿確認が終わったことにはならない。

## 投稿と失敗の境界

- POST前にPull Requests APIで対象がOPEN PRであることとHEADの完全一致を確認する。通常Issue、closed/merged PR、HEAD移動、読取失敗では投稿しない。
- 確認と投稿は不可分ではなく、投稿後にHEADが動く可能性は残る。`headSha`は投稿直前の確認対象であり、投稿後のHEAD一致保証ではない。
- POSTは1回だけ。通信失敗・不完全な投稿応答では投稿済みか不明なので自動再試行しない。呼出側も別経路で再投稿せず、既存readerで投稿有無を確認し、未確定なら停止する。
- tool自身はqueue登録もreviewer起動も行わない。webhook構成では再依頼コメントの配信によってowlがenqueueする場合がある。`--mcp-http`構成では投稿成功・読戻し確認の後、skillが別途enqueueと購読を行う。webhook構成で重ねてenqueueしない。
- コメント投稿者の検証ゲート、自己レビュー禁止、マージ許可の独立は維持する。PR本文更新、汎用の指摘返信、ランナー障害通知は別の用途であり、このAPIに追加しない。
