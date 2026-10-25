# レビュー状態の共有と進捗観測の見直し（#144 の改訂案）

状態: 設計案。2026-10-25。実装・配備・廃止は未実施。利用者の方針（本文の「利用者の方針」）に基づく案で、確定していない部分は「未確認・決めること」に分けた。
対象: [review-raven#144](https://github.com/scottlz0310/review-raven/issues/144)、[squirrel-notifier#497](https://github.com/scottlz0310/squirrel-notifier/issues/497)。
関連: [review-responsibility-boundaries.md](review-responsibility-boundaries.md)（本書が見直す、内部連携を含む責務設計）。

## 決定案（要約）

1. **完了・指摘・Verdict の共有は、これまでどおり GitHub の PR で行う。** 各 MCP サーバーが自分の GitHub App（`thread-owl[bot]`、`review-raven[bot]` など）として投稿し、skill が許可リスト（`get_trusted_comment_authors`）で投稿者を信頼する。身元の証明に、新しいアカウントも資格情報も要らない。
2. **起動前から実行中までの進捗（CI 待ち・別 PR の行列待ち・レビュー中）は、Squirrel Notifier が公開する `review-status.json` を、同じ PC 上で購読して得る。** 購読は `resource-bridge-cli`（案 C）に置く。
3. **サービス間認証を要する内部連携（review-raven ⇔ thread-owl ⇔ Squirrel の認証付き通信）は、これ以上進めない。** GitHub ユーザー（machine user）を増やさない。廃止するかは、案 C を実際のレビューで確認した後に決める。
4. 公開 webhook など、新しい公開面は、「どうしても必要」になるまで保留する。

## 背景と訂正

- #144 の設計（review-raven#146）は、内部連携の通信相手を「gateway で登録したサービス主体」と書いた。**GitHub のユーザーアカウントを新しく作ることは書いていない。**
- その具体化として、Mcp-Docker#394 の手順書は、サービス主体を「専用の GitHub アカウント（machine user）」とした。これは実装時の判断で、利用者へ選択肢としても、費用・規約の確認としても示していない。
- 理由は、現行の mcp-gateway にサービス用の認証がないことである。`/token` が受け付ける grant は `authorization_code`、`urn:ietf:params:oauth:grant-type:device_code`、`refresh_token` の 3 つだけで、いずれも GitHub のユーザーとしてのログインが前提になる（`internal/auth/handler.go` の `Token`）。ACL のキーも、gateway が付与するユーザーのログイン名である。
- GitHub の利用規約は、無料の machine account を、個人アカウントに加えて 1 つまでとしている（[GitHub Terms of Service](https://docs.github.com/en/site-policy/github-terms/github-terms-of-service)、Account Terms。規約全文の確認と、有料席の扱いは未確認）。組織 `scottlz0310` は Team プラン（席 1）で、主体ごとに別アカウントを作る前提は、規約と費用の両面で成り立たない。
- 利用者の意図は、各 App が GitHub 上で責務を分離している既存の仕組みで足りる、というものだった（本書「利用者の方針」）。

## 利用者の方針

2026-10-25 の発言に基づく。

- 目的は、PR を媒体にした、実装側とレビュー側の往復を、状態機械として扱えるようにすること。
- 身元の証明は、各 MCP サーバーが自分の GitHub App で PR に投稿することと、skill の許可リストで足りる。新しいユーザーアカウントは作らない。
- CI 待ち・別 PR の行列待ち・レビュー中・完了は、Squirrel が公開する JSON を、MCP サーバーか `resource-bridge-cli` が購読して得る。案 C だけで足りる見込みだが、LLM エージェントでの使い心地は未検証。
- ローカルで実装し、同じローカルでレビューする。リモートは `/rc` などで、指示だけを出す。したがって、別 PC をまたぐ観測は要件にしない。
- 公開 webhook などは「どうしても必要」になるまで保留し、不要なら廃案にする。
- リリースは、必要になった時点で、6 repo すべてで行ってよい。従前のリリースの作法に従う。skill の編集（PR）も、使い心地の結果に応じて行ってよい。通知方法は最適化する。ハートビートは、あってもよい。

## 要件

| ID | 要件 | 備考 |
|---|---|---|
| Q1 | 追加のアカウント・資格情報・公開面を要求しない | machine user、共有 Bearer、公開 webhook を含む |
| Q2 | 完了の判定は PR の記録に基づく | Verdict の照合、`Reviewed HEAD SHA` と PR HEAD の一致 |
| Q3 | 進捗（CI 待ち・行列待ち・実行中・終了）を、同一セッションへ途中報告できる | 途中報告は表示用。完了の根拠にしない |
| Q4 | 古い round・別 PR・停止した観測を、現在の状態として扱わない | PR・HEAD・round の対応付け、`enqueued_after` |
| Q5 | 静かなまま詰まらない | 観測不能・手動待ち・未受信・タイムアウトを必ず通知する |
| Q6 | 同じ PC 上で完結する | 別 PC の観測は要件にしない |

## 現行の実装（内部連携）の整理

内部連携は、スイッチ `REVIEW_INTERNAL_LINK_ENABLED` が未設定の間、無効である。**稼働中の挙動は変わっていない。**

| 構成要素 | 実装（マージ済み） | 本書の扱い |
|---|---|---|
| review-raven の依頼台帳・配送・内部面（R1） | review-raven#149〜#155 | サービス間認証が前提。凍結。廃止は後で判断 |
| thread-owl の候補受付・台帳・結果送信（O1） | thread-owl#281〜#289 | 同上 |
| Mcp-Docker の内部連携のスイッチ・compose・手順書（I1） | Mcp-Docker#394 | 同上。手順書は machine user 前提のため、見直す |
| 内部連携のサービス主体のトークン取得スクリプト | Mcp-Docker#395 | machine user 前提。使わない |
| Squirrel の観測の組み立てと送信（S1） | squirrel-notifier#500、#501（既定無効） | 送信は凍結。**公開 JSON は維持する（案 C の入力）** |
| `resource-bridge-cli` の逐次出力（R2） | mcp-resource-subscriber#226 | **維持して使う。** v0.7.0 のヘルプには表示されない（未リリースの可能性） |
| reviewed 側 reader（B1）`read_pr_comments` | review-raven#156、#157、thread-owl#290 | **サービス間認証と独立。維持する** |
| reviewgate の必須 check 未定義の扱い | Mcp-Docker#396 | **独立。維持する** |

## 案の比較

| 案 | 内容 | 利点 | 欠点・リスク |
|---|---|---|---|
| A 現行の内部連携 | 認証付きの内部面で、台帳・配送・観測を中継する | 別ホストでも観測できる。状態を一箇所へ集約できる | サービス間認証がなく、machine user が前提になる（規約・費用）。構成要素が 6 repo に及ぶ。運用の負担が大きい |
| B PR / Check Run | thread-owl が、HEAD に紐づく Check Run（queued / in_progress / completed）を投稿する | GitHub 上で見える。別ホストでも使える。身元は App で足りる | thread-owl の App に Check Run の書き込み権限が要る（未確認）。Squirrel は App を持たず、起動状態は thread-owl を経由して渡すことになる。GitHub API の呼び出しが増える |
| **C ローカル JSON の購読（推奨）** | `resource-bridge-cli` が、Squirrel の `review-status.json` を監視し、状態の変化を 1 行ずつ出す | 新しい身元・公開面・サーバーが要らない。公開契約が既にある。逐次出力（R2）を活かせる | 同じ PC に限られる（本書では許容）。他の CLI の逐次通知は未確認 |
| D ローカル MCP サーバー | その JSON を MCP の resource として出す小さなサーバーを置く | CLI 以外の MCP クライアントも、同じ作法で購読できる | 部品が 1 つ増える。C で不足が出てから検討する |

別 PC をまたぐ観測が将来必要になったら、B を検討する。そのときも A は選ばない。

## 案 C の設計

### 入力

Squirrel Notifier が公開する `%LocalAppData%\SquirrelNotifier\review-status.json`（公開契約 v1、[Squirrel の契約](https://github.com/scottlz0310/squirrel-notifier/blob/main/docs/review-status-contract.md)）。

- 状態が変わったときと、60 秒ごと（heartbeat）に更新される。`updatedAt` が 3 分より古ければ、観測できない。
- `items[]` は起動待ち・実行中、`recent[]` は終了（24 時間・最大 50 件）。`holdReason` は `ciPending` / `busy` / `manual` / `autoPause`。
- 書き込みは `.tmp` への書き出し後の `File.Move`（上書き）で、原子的に置換される。読み取り側が開いている間の置換の共有違反は、Squirrel 側が短く待って再試行する（`ReviewStatusService.cs`）。**短時間の読み取りなら、監視が Squirrel の書き込みを妨げない。**
- Squirrel v0.16.0 以降。ファイルが無い、または読めないときは、観測できない。

### 出力（イベント）

標準出力へ、1 イベント 1 行。Claude Code の `Monitor` は、標準出力の 1 行を 1 通知として扱う。

| event | 出すとき | 備考 |
|---|---|---|
| `state` | 対象の `(state, holdReason, round, eventId, queuePosition)` が変わったとき | `updatedAt` だけの更新（heartbeat）では出さない。`detail` に日本語の短い説明を付ける |
| `heartbeat` | 状態が変わらないまま、一定時間（既定 5 分）が過ぎたとき | 生存確認。通知の量を抑えるため、状態の変化の通知とは別にする |
| `not_received` | 対象の項目が一定時間（既定 2 分）現れないとき | queue event を受信していない可能性。1 回だけ |
| `unobservable` | ファイルが無い、読めない、`updatedAt` が 3 分より古い | `reason=no_file` / `unreadable` / `stale`。回復したら `recovered` |
| `action_required` | `holdReason = manual` のとき | 「レビューする」か別 CLI での起動を案内して終了 |
| `timeout` | 全体の上限（既定 20 分）に達したとき | 再購読するかは、呼び出し側が判断する |

対象は、小文字の `owner/repo#N` と、`enqueued_after`（`enqueue_review` の直前の時刻）より後に受信した項目に限る。**時刻は小数秒まで保持して比較する。** 秒に切り捨てると、同じ秒に起きた enqueue と受信の前後関係を失い、今回の round を永久に拾えなくなる（`--after` も小数秒つきで渡す）。**前の round の `finished` を、今回のものとして拾わない。**

終了コードは、`0` が終了（`finished`）、`3` が手動待ち、`4` がタイムアウトである。終了すると、`Monitor` は完了を通知する。

### 通知の方針

- **状態の変化だけ**を通知する。heartbeat のたびに通知しない。
- **静かなまま詰まることを許さない。** 観測不能・未受信・手動待ち・タイムアウトを、すべてイベントとして出す。無通知は成功を意味しない。
- ハートビートは、長く待つときの生存確認として置く。間隔は引数で変えられる。
- `finished` の `outcome` / `exitCode` は、reviewer の**プロセスの終了結果**で、Verdict ではない。完了の判定は、PR の Verdict と `review://status` の `headSha` で行う（Q2）。

### skill への反映案

reviewed 側 skill の Phase W（`references/review-wait.md`）を、次のように見直す。

1. `enqueue_review` の直後に、`review://status` の購読（完了の判定）と、案 C の監視（進捗の途中報告）を並べて起動する。2 つの責務は混ぜない。
2. 逐次通知を受け取れる CLI（Claude Code の `Monitor` など）では、監視の出力をそのまま途中報告に使う。受け取れない CLI では、終了時に 1 回だけ最終状態を読む現行の形にフォールバックする。
3. `action_required` / `unobservable` / `timeout` を、現行の `REVIEW_WAIT_TIMEOUT` の判断（公開状態の確認）に結び付ける。
   完了の判定は PR の Verdict と `review://status` の `headSha` で行うので、Verdict が先に届いたら、`finished` の通知を待たずに完了の判定へ進んでよい（実 round で、Verdict が `finished` より先に届くことを確認した）。
4. 反映は、使い心地の確認の結果を踏まえて、別の PR で行う。

### 実装の置き場所

- 実装は `resource-bridge-cli`（mcp-resource-subscriber）のサブコマンドとする（購読・更新受信・構造化出力が、その責務である）。サブコマンド名は未定。
- リリースは、必要になった時点で、従前の作法（各 repo の既存の手順）に従って行う。

## 試作と確認結果

bash と jq の試作で、模擬の `review-status.json` を使い、実物の `Monitor` で受け取った（付録に全文）。

| 確認した経路 | 結果 |
|---|---|
| 受信前 → CI 確定待ち → 行列待ち → 実行中 → 終了 | 状態の変化と、設定した間隔の heartbeat だけが通知された。updatedAt のみの更新では通知されない |
| 終了 | `state=finished outcome=completed exit=0` を出し、終了コード 0 で終了 |
| 手動待ち | `action_required` を出し、終了コード 3 |
| 更新の停止（3 分以上） | `unobservable reason=stale`。タイムアウトで終了コード 4 |
| 前の round の終了だけがある | 拾わない。`not_received` |
| ファイルが無い | 試作の初版は無通知だった。`unobservable reason=no_file` を出すよう修正した |
| 実物のファイル（対象 PR なし） | `not_received` からタイムアウトまで、想定どおり |
| **実物の round（thread-owl#292、約 2 分）** | 通知は 3 件だけだった: `waiting ciPending`（CI 確定待ち）→ `running` → `finished outcome=completed exit=0`。`finished` で終了コード 0 で自動終了。情報量は十分で、頻度も適切。**reviewer の Verdict の投稿（10:57:07）が、`finished` の通知より先に届いた**（プロセスの終了は Verdict の後）。設計の「`finished` は Verdict ではない」を裏付ける |
| 同じ秒内の順序（`enqueued_after` と `receivedAt` が同じ秒） | 初版は小数秒を切り捨てて比較しており、受信が後でも今回の round を除外した（レビューで指摘）。小数秒を保持する比較に直した。確認: 同一秒の受信が後（0.900 > 0.100）は拾う、前（0.050 < 0.100）と同時刻（0.100）は拾わない、小数秒の桁数が違っても（0.9 と 0.1000000）、`--after` が秒精度でも、翌秒でも期待どおり |

## 未確認・リスク・決めること

- **実際のレビュー round での使い心地。** 1 回（thread-owl#292、約 2 分）は上のとおり確認できた。**長い round（ハートビートが出る長さ）、行列待ち（`busy`）を挟む round、再 round は未確認**。
- **クライアントの出力の省略。** Codex は、大きなツール出力の中間を省略する（`Warning: truncated output` と `…N tokens truncated…`。review-raven#158 のレビューが、`get_pr` の末尾の `origin` を失って止まった事例。原因と修正は thread-owl#291、#292）。案 C の監視も、1 行を短く保ち、重要な情報を行の先頭側に置く。出力量を抑える設計（状態の変化だけ、ハートビートは低頻度）は、この点でも妥当。
- **他の CLI。** Codex・Copilot・Antigravity が、`Monitor` に相当する逐次通知を受け取れるかは未確認。
- **同じ PR で前の round が走っている間。** 契約上、前の round の項目が `items[]` に残る場合、今回の round が見えず、`not_received` になる可能性がある。
- **`autoPause` の詳細。** 原因・使用率・再開時刻が公開 JSON に載らない（ハンドオフの A3）。表示は `autoPause` までにとどまる。
- **内部連携の扱い。** 凍結のあと、廃止（コードの削除）にするか、残すかは、案 C の確認後に決める。廃止は、repo ごとに分けた PR で行う。
- **Check Run（案 B）。** 別 PC をまたぐ観測が必要になった場合の候補。thread-owl の App の権限は未確認。

## 進め方

1. 本書の合意（利用者）。
2. 実際のレビュー round で、試作を動かして使い心地を確認する。
3. `resource-bridge-cli` のサブコマンドとして実装し、必要ならリリースする。
4. reviewed 側 skill の Phase W を見直す（別 PR）。
5. 内部連携の凍結を、ハンドオフと各 repo の文書へ反映し、廃止の要否を決める。

## 付録: 試作スクリプト

bash と jq。模擬データでの確認に使った。製品化では `resource-bridge-cli` のサブコマンドとして作り直す。

```bash
#!/bin/bash
# 試作: Squirrel の review-status.json を監視し、対象 PR の「状態の変化」だけを1行ずつ標準出力へ出す。
# Monitor（1行=1通知）で受け取る前提。updatedAt だけの更新（60秒の heartbeat）では出力しない。
#
# 使い方: watch-review-status.sh --key owner/repo#N --after <ISO8601> [--file P] [--interval S]
#                                [--heartbeat S] [--max-stale S] [--not-received S] [--timeout S]
# 終了コード: 0=finished, 3=manual（利用者の操作待ち）, 4=timeout
set -uo pipefail

key=; after=; file="${LOCALAPPDATA:-}/SquirrelNotifier/review-status.json"
interval=1; heartbeat=300; max_stale=180; not_received=120; timeout=1200
while [ $# -gt 0 ]; do
  case $1 in
    --key) key=$2; shift 2 ;;
    --after) after=$2; shift 2 ;;
    --file) file=$2; shift 2 ;;
    --interval) interval=$2; shift 2 ;;
    --heartbeat) heartbeat=$2; shift 2 ;;
    --max-stale) max_stale=$2; shift 2 ;;
    --not-received) not_received=$2; shift 2 ;;
    --timeout) timeout=$2; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$key" ] && [ -n "$after" ] || { echo "usage: --key owner/repo#N --after ISO8601" >&2; exit 2; }
key=$(printf '%s' "$key" | tr 'A-Z' 'a-z')

start=$(date -u +%s)
last_emit=$start
prev=; stale=0; none_reported=0; nofile_reported=0

emit() { # 1行=1イベント
  printf 'review-watch %s %s\n' "$key" "$*"
  last_emit=$(date -u +%s)
}

# 状態を「state|hold|round|eventId|queue|updatedAt」の1行にする。
observe() {
  jq -r --arg key "$key" --arg after "$after" '
    # 小数秒を保持して比較する。[秒, 小数部(9桁固定の文字列)] の配列は、辞書順で時刻の順になる。
    # 秒に切り捨てると、同じ秒に起きた enqueue と受信の前後関係を失う。
    def ts: (sub("\\.[0-9]+Z$";"Z") | fromdateiso8601) as $s
      | ((capture("\\.(?<f>[0-9]+)Z$").f // "") + "000000000" | .[0:9]) as $f
      | [$s, $f];
    ([.items[]? | select(.key==$key and (.receivedAt|ts) > ($after|ts))] | first) as $it
    | ([.recent[]? | select(.key==$key and (.receivedAt|ts) > ($after|ts))] | first) as $rc
    | if $it != null then
        "\($it.state)|\($it.holdReason // "")|\($it.round)|\($it.eventId)|\($it.queuePosition // "")|\(.updatedAt)"
      elif $rc != null then
        "finished|\($rc.outcome // "")|\($rc.round)|\($rc.eventId)|exit=\($rc.exitCode // "")|\(.updatedAt)"
      else "none|||||\(.updatedAt)" end' "$file" 2>/dev/null
}

age_of() { # updatedAt からの経過秒
  local u=$1
  u=$(printf '%s' "$u" | sed -E 's/\.[0-9]+Z$/Z/')
  local t; t=$(date -u -d "$u" +%s 2>/dev/null) || { echo 0; return; }
  echo $(( $(date -u +%s) - t ))
}

describe() { # state hold -> 日本語の短い説明
  case "$1:$2" in
    waiting:ciPending) echo "CI確定待ち(起動前)" ;;
    waiting:busy)      echo "別のレビュー実行中のため待機" ;;
    waiting:manual)    echo "利用者の操作待ち(自動起動off)" ;;
    waiting:autoPause) echo "自動起動が一時停止中" ;;
    waiting:*)         echo "起動待ち(評価中)" ;;
    running:*)         echo "レビュー実行中" ;;
    finished:*)        echo "プロセス終了(Verdictは別途PRで確認)" ;;
    *)                 echo "$1" ;;
  esac
}

while :; do
  now=$(date -u +%s)
  if [ $((now - start)) -ge "$timeout" ]; then emit "event=timeout elapsed=$((now - start))s"; exit 4; fi

  line=
  for _ in 1 2 3; do # 書き込み途中の読み込みに備え、数回やり直す
    line=$(observe) && [ -n "$line" ] && break
    line=; sleep 0.2
  done
  if [ -z "$line" ]; then
    # ファイルが無い(Squirrel 未起動・v0.16.0 未満)／読めない。静かに待たず、1回だけ知らせる
    if [ "$nofile_reported" -eq 0 ]; then
      if [ ! -f "$file" ]; then emit "event=unobservable reason=no_file"; else emit "event=unobservable reason=unreadable"; fi
      nofile_reported=1
    fi
    sleep "$interval"; continue
  fi
  nofile_reported=0

  IFS='|' read -r state hold round event queue updated <<<"$line"

  # 観測不能(更新が止まっている)の検出。状態の変化として1回だけ出す。
  age=$(age_of "$updated")
  if [ "$age" -gt "$max_stale" ]; then
    if [ "$stale" -eq 0 ]; then emit "event=unobservable reason=stale age=${age}s"; stale=1; fi
    sleep "$interval"; continue
  elif [ "$stale" -eq 1 ]; then
    emit "event=recovered"; stale=0
  fi

  cur="$state|$hold|$round|$event|$queue"
  if [ "$state" = none ]; then
    if [ $((now - start)) -ge "$not_received" ] && [ "$none_reported" -eq 0 ]; then
      emit "event=not_received waited=$((now - start))s"; none_reported=1
    fi
  elif [ "$cur" != "$prev" ]; then
    msg=$(describe "$state" "$hold")
    if [ "$state" = finished ]; then # finished は hold/queue 欄に outcome/exitCode が入っている
      emit "event=state state=finished round=$round outcome=${hold:--} ${queue:-exit=-} detail=\"$msg\""
    else
      emit "event=state state=$state hold=${hold:--} round=$round queue=${queue:--} detail=\"$msg\""
    fi
    prev=$cur
    [ "$state" = finished ] && exit 0
    if [ "$state" = waiting ] && [ "$hold" = manual ]; then emit "event=action_required detail=\"Squirrelの「レビューする」か別CLIでreviewerを起動\""; exit 3; fi
  elif [ $((now - last_emit)) -ge "$heartbeat" ]; then
    emit "event=heartbeat state=$state hold=${hold:--} elapsed=$((now - start))s"
  fi
  sleep "$interval"
done
```
