# #144の関連Issueと廃案決定

2026-11-01、利用者が認証付き内部連携の廃案・撤去を決定した。新受付への段階移行は行わない。進捗はDropboxのcross.md、判断と撤去は[review-raven#159](https://github.com/scottlz0310/review-raven/issues/159)で追跡する。

| 対象 | 扱い |
|---|---|
| R1 / review-raven#147 | 専用受付・台帳・内部面・配送を撤去する |
| O1 / thread-owl#279 | 専用内部候補・台帳・結果配送を撤去する。既存review queue/statusは維持 |
| S1 / squirrel-notifier#497 | 内部送信を撤去し、公開JSONによる同一PC観測を維持 |
| I1 / Mcp-Docker#392 | 専用route/設定/サービス主体スクリプトを撤去する |
| I2 / Mcp-Docker#393 | 新受付・購読先への移行は中止する |
| B1 / review-raven#148・thread-owl#280 | 投稿者検証済みreaderは別機能であり維持する |

Issueのcloseは各repoの撤去・検証結果に基づき行い、廃案決定だけで実装の撤去完了へ繰り上げない。[旧後続計画](https://github.com/scottlz0310/review-raven/blob/68332ad02e4ef9f6b837aca3ad83b8eea7b1316d/docs/design/review-responsibility-followups.md)は履歴として参照できる。
