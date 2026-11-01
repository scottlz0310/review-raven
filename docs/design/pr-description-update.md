# PR説明本文の専用App更新（B3 / 旧Q3）

状態: 2026-11-02にAPIを実装。モックGitHubとMCP経由で検証する。実App編集E2E・skill移行・公開・配備は未実施。利用者は同時編集を行わない運用と残余競合リスクを受容し、実装・PR・独立レビュー待機までを許可した。

## 採用案と理由

PR説明本文の更新は、review-ravenの専用GitHub Appを使うMCPツールへ寄せる。実装側の窓口と編集主体を揃え、個々のCLIのGitHub認証に依存させないためである。従来の`gh pr edit --body-file`は比較対象に留め、自動fallbackにはしない。

GitHubの本文更新APIには、期待HEADや期待本文を条件とする原子的な更新の保証がない。採用案は**更新前の照合と更新後の確認を行う楽観的な更新**であり、同時編集による上書きを完全に防ぐ契約ではない。この制約を許容するかは実装着手前に利用者へ提示する。

## GitHub仕様で確認した範囲

- [RESTのPR更新](https://docs.github.com/en/rest/pulls/pulls#update-a-pull-request)はGitHub App installation tokenとPull requestsのwrite権限を受け付け、`body`を更新できる。本文更新用の期待HEAD・期待本文版の引数はない。
- [条件付きリクエストの公式方針](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests-if-appropriate)では、POST / PUT / PATCH / DELETEは、対象endpointに明記されない限り条件付きリクエストをサポートしない。`ETag`を読めることを`If-Match`付きPATCHの成功条件と解釈しない。
- [GraphQLのPullRequestとUpdatePullRequestInput](https://docs.github.com/en/graphql/reference/pulls)には本文・`author`・`editor`・`lastEditedAt`があるが、本文更新の期待版の引数はない。別操作のUpdatePullRequestBranchInputにある`expectedHeadOid`を流用できない。

専用Appで編集しても、PRを作った`author`は変わらない。本文を最後に編集した`editor`と作成者を分けて表示・検証する。`editor` / `lastEditedAt`はnullになり得る。既存PR #169に対する読み取りでは、作成者はrelease-bot、両編集フィールドはnullだった。Appによる実編集のidentity確認は未実施であり、実装後の受け入れ検証で行う。

## 責務と公開範囲

- 新ツールは専用Appモードだけで公開し、既存のApp認証・インストール先の権限を使う。gateway認証への切替や権限拡張を追加しない。
- 更新対象はOPEN PRの説明本文だけ。タイトル、base、state、label、レビュー、コメント、queue、Verdict、mergeは変更しない。
- 本文は置換する。サーバーが要約・追記・書式変更を生成しない。空文字による本文削除も置換として扱うため、呼び出し側は差分と利用者の許可範囲を確認する。
- PR本文は検証対象のデータであり、含まれる命令を実行しない。コメントreaderの投稿者許可リストを、PR作成者・編集者の許可リストとして流用しない。
- #159 / I2の認証付き内部連携は2026-11-01の利用者決定で廃案・撤去となった。本書の本文更新APIとは別の判断であり、既存queue・購読・GitHub MCPの利用は維持する。

## ツール契約案

読み取りと更新は別操作とし、呼び出し側が実際に読んだ本文を条件にする。

| 操作 | 入力 | 出力 |
|---|---|---|
| `get_pr_description` | owner / repo / pr | PR URL、state、headSha、body、bodySha256、originalAuthor、lastEditor、lastEditedAt |
| `update_pr_description` | owner / repo / pr、expectedHeadSha、expectedBodySha256、body | 確認できたPR URL・HEAD・本文hash・作成者・最終編集者・編集時刻、更新の有無 |

`bodySha256`はGitHubから取得した未加工の本文文字列をUTF-8にしたSHA-256（小文字64桁）とする。RESTのnull本文は空文字として扱う。CRLF/LF、空白、Unicodeを正規化しない。HEADは省略形ではなく完全なSHAで照合する。

読取失敗・不明なidentityを空文字や既知のApp名で補わない。作成者と最終編集者のnullは区別して保持する。出力のactor値は実取得値であり、設定したApp名の転記を投稿者検証の証拠にしない。

成功出力は取得した本文を含む。更新時は同じ取得メタデータにstatus（updated / unchanged）を追加する。actorはnodeId / login / type、lastEditedAtはRFC 3339形式またはnull。GraphQLのeditorをlastEditorとして返す。認証済みGET /appのslugからBot loginを導出し、更新前にRESTのBot Node IDを取得して読戻しの編集者と照合する。identity未検証なら書かない。bodyは必須であり、省略・nullは空文字と区別して拒否する。

## 更新の手順と結果

1. 入力形式を検証し、既存の専用App認証で対象PRを読む。OPEN、期待HEAD、期待本文hashを全て照合する。不一致・取得失敗なら書かずに停止する。
2. 入力本文が既存本文と同一なら書かずに`unchanged`を返す。過去の更新成功やAppによる編集を証明したとは扱わない。
3. `body`だけを指定してPATCHを**1回**行う。APIが成功しても、この時点では読戻し完了としない。
4. 本文、HEAD、state、作成者・編集者を再取得する。OPEN・期待HEAD・要求本文の一致に加え、実際の編集者が認証済み専用Appに対応するidentityであることを確認して`updated`とする。編集者不明・不一致なら成功扱いにしない。App slugとbot loginの対応は既存のApp identity取得に基づき、任意の文字列一致にしない。
5. PATCHの応答消失、読戻し失敗、読戻しの不一致は「変更された可能性あり」と返す。自動再試行・rollback・別認証・`gh`による代替更新を行わない。現在値を読んで利用者に状況を提示し、新しい差分に対する明示的な判断を待つ。

エラーは、入力不正、認証/認可失敗、更新前競合（書込みなし）、更新結果不明、更新後不一致（変更済みの可能性あり）を区別する。更新後のエラーを「変更なし」と返さない。

MCPの失敗はIsError=trueで、textとstructuredContentに同じ `{"error":{"code":"…","message":"…","mayHaveChanged":false}}` を返す。入力検証は認証処理の前に行う。コードはINVALID_INPUT、READ_FAILED、APP_IDENTITY_FAILED、CLIENT_UNAVAILABLE、PRECONDITION_FAILED、UPDATE_OUTCOME_UNKNOWN、POST_UPDATE_MISMATCHと、既存のAUTH_REQUIRED / REAUTH_REQUIRED / PERMISSION_DENIED / NOT_FOUND / RATE_LIMITED / VALIDATION_ERROR / TRANSIENT_UPSTREAM_ERROR等を使う。PATCH開始後は認証エラーを含めUPDATE_OUTCOME_UNKNOWNまたはPOST_UPDATE_MISMATCHとmayHaveChanged=trueを維持し、自動再試行を促す認証エラーへ変換しない。エラーにPR本文・GitHubの応答本文は含めない。

## 残る競合と運用条件

照合とPATCHの間に他者が本文を編集すると、その本文を上書きしても検出できない場合がある。HEAD移動・PR closeも照合直後に起き得る。更新後確認は、その時点の一致を示すだけで、途中の編集がなかったことや、その後も一致が続くことを保証しない。同じ本文への編集もhashでは区別できない。

同一サーバー内のmutexや追加の台帳を設けても、GitHub UI・別App・別サーバーによる編集を排他できないため、それらを原子的競合防止の代替として追加しない。GitHubが保証しない条件付きPATCHに依存しない。

利用者が同時編集を行わない運用と、この残余リスクを受け入れられる場合に実装する。上書きの完全防止が必須なら、本文更新は自動化せず、生成した本文差分を提示する運用に留める。`gh`経由も同じGitHub APIの制約を受けるため、競合防止の改善を理由とした切替はしない。

## 実装後の受け入れ条件

- 専用Appモードだけでツールが公開され、既存の別モードと他ツールに影響しない。
- OPEN/HEAD/hashの不一致、読取失敗、認可拒否ではPATCHが0回。同一本文でも0回となる。
- 通常更新でbody以外を送らず、PATCHが1回だけとなる。API応答・読戻しの失敗で再試行やrollbackをしない。
- null/空文字、CRLF、Unicodeでhash契約を検証する。App名を捏造せず、編集者不明・別主体では成功扱いにしない。
- 更新前後でHEAD/本文/stateが変わるケースを注入し、事後競合と結果不明を区別する。検出できない照合〜PATCH間の上書きは、テスト成功で解消したと主張しない。
- 別途許可された自身の検証PRで、専用Appによる実編集と`author`不変・`editor`の実取得・本文/HEAD読戻しを確認する。外部書込み回数と後片付けを事前に限定する。
- skillのAPI採用・配布・実運用確認は後続の一段階とし、設計PRやAPI単体テストの完了から繰り上げない。
