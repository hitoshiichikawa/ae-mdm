# 実装ノート（Issue #35 / A6a Pub/Sub クライアント + Worker エントリ）

## 実装概要

tasks 6.1（PubSubClient / PubSubSubscriber）に範囲を限定し、以下を実装した。

- `internal/config`: 新規 optional config `PubSubDeadLetterTopic`（env `PUBSUB_DEAD_LETTER_TOPIC` /
  default `""`）。`.env.example` / `docker-compose.yml`（api / worker）に既定値
  `amapi-notifications-deadletter` を追加。
- `internal/platform/pubsub/client.go`: emulator / 本番 GCP 切替の `*pubsub.Client` 構築、
  多重 Close 安全（`sync.Once`）、emulator モード時の topic / subscription / dead-letter topic
  冪等 ensure。
- `internal/platform/pubsub/subscriber.go`: pull 受信ループ、`Message` / `MessageHandler` /
  `HandlerFunc` 抽象、`errors.ShouldAck` 経由の ack/nack、dead-letter 送出 IF
  （`PublishToDeadLetter`）、`MaxOutstandingMessages < 1` 拒否。
- `cmd/worker/main.go`: 既存 bootstrap（config → logger → :8090 /healthz 常駐 + `-healthcheck`）を
  温存したまま subscriber を配線。`superviseWorker` が受信ループと healthz を並走監視し、
  subscriber 致命的エラーは fail-fast、SIGINT/SIGTERM は 5s graceful shutdown。

テストは `cloud.google.com/go/pubsub/pstest`（in-process fake / docker 不要）で全 AC を AAA 検証。

## テスト結果（サマリ）

| コマンド | 結果 |
|---|---|
| `gofmt -l`（変更ファイル） | 差分ゼロ（後述の既存 unformatted 群は本 Issue 対象外） |
| `go vet ./...` | PASS |
| `go build ./...` | PASS |
| `go test ./...` | 全 PASS（14 パッケージ） |
| `go test -race ./internal/platform/pubsub/ ./cmd/worker/` | PASS（data race なし） |

新規テストは実装前に対象パッケージが存在せず（Red 相当）、実装投入で Green になった。
pstest 配線で受信が動かない初期失敗（複数 SDK client が 1 本の gRPC 接続を共有し相互 Close で
殺し合う / nack 再配信が SDK lease 管理で数秒かかる）を観測し、`1 SDK client = 1 接続`へ
修正・redelivery 観測の deadline を 15s に調整して Green 化した。

## AC とテストの対応表（requirements 全 ID トレーサビリティ）

| AC | 担保テスト |
|---|---|
| 1.1 | `TestNewClient_ConstructsWithProjectID`（pubsub） |
| 1.2 / 1.3 | `TestNewClient_EmulatorVsProductionOptions`（emulator=3 opt / 本番=0 opt の切替） |
| 1.4 | `TestNewClient_BuildFailure_ReturnsStructuredError`（CodeUnavailable / transient / 部分初期化なし） |
| 1.5 / 1.6 | `TestClient_Close_Idempotent`（多重 Close で追加エラーなし） |
| 2.1 / 2.2 | `TestSubscriber_ReceivesAndHandsToHandler` |
| 2.3 | `handleOne` が `logger.MessageID` で受信ログを記録（受信系テスト全体で経路通過） |
| 2.4 | `TestSubscriber_SubscriptionMissing_ReturnsErrorWithoutLoop`（CodeNotFound / handler 未呼び出し） |
| 2.5 | `TestSubscriber_EmptyPayload_StillHandedToHandler` |
| 3.1 | `TestSubscriber_HandlerSuccess_Acks`（再配信されない=1 回） |
| 3.2 / 3.3 | `TestSubscriber_TransientError_Nacks_Redelivered`（再配信される=≥2 回 / ShouldAck 経由） |
| 3.4 | `TestSubscriber_PermanentError_Acks_NotRedelivered`（非 transient は ack=1 回） |
| 3.5 | ack/nack 結果を message_id 付きでログ（上記 3.x テストで経路通過） |
| 4.1 | `TestNewSubscriber_MaxOutstandingAccepted`（1/5/1000 受理 + ReceiveSettings 反映値検証） |
| 4.2 | `MaxOutstandingMessages` を `ReceiveSettings` に反映（4.1 テストで設定値が Subscriber に保持されることを検証 / pull 抑制は SDK 実装） |
| 4.3 | `TestNewSubscriber_MaxOutstandingBelowOne_Rejected`（負値=CodeConfigInvalid） |
| 5.1 / 5.2 / 5.3 | `TestSubscriber_DeadLetterPublish_Success`（payload + original_message_id が DL topic に届く） |
| 5.4 | `TestSubscriber_DeadLetterPublish_TopicUnset_ReturnsError`（未設定=CodeConfigInvalid / ack しない） |
| 6.1 / 6.2 | `cmd/worker` 配線 + `TestSuperviseWorker_GracefulShutdownOnSignal`（handler に受信が届く） |
| 6.3 | healthz server 常駐は既存挙動温存（`superviseWorker` が :8090 /healthz を継続） |
| 6.4 | `TestSuperviseWorker_GracefulShutdownOnSignal`（ctx キャンセルで停止 + 終了コード 0） |
| 6.5 | `TestSuperviseWorker_SubscriberFatalError_ReturnsNonZero`（subscription 不在=code 1） |
| 8.x（design） | 本 Issue は「土台」のみ（dead-letter 送出抽象 5 / ack-nack 抽象 3）。dispatch/dedupe/verifier は #36 |

NFR 対応:
- NFR 1.1（接続失敗 fail-fast）: `TestNewClient_BuildFailure_*` + `runWorker` の client 構築失敗経路
- NFR 1.2（操作ごとの構造化ログ）: client/subscriber の各操作で `logger.Err` 付きログを 1 件出力
- NFR 2.1（5s graceful shutdown）: `shutdownHealthz` が `shutdownTimeout=5s`、`TestSuperviseWorker_GracefulShutdownOnSignal`
- NFR 3.1（既存 scaffold 整合）: `-healthcheck` / :8090 /healthz / `runHealthcheck` を温存

## 暫定確定した 3 点（Reviewer / 人間の追認待ち）

requirements「確認事項」の未決定事項を Stage A で以下に暫定確定した（根拠つき）。

1. **dead-letter topic の設定源** → 新規 optional config `PubSubDeadLetterTopic`
   （env `PUBSUB_DEAD_LETTER_TOPIC` / `optionalStr` / default `""`）を追加。送出 IF が呼ばれた時に
   topic 名が空なら `CodeConfigInvalid` を返し ack しない（requirements 5.4 と整合）。
   根拠: 既存 config の Pub/Sub 群が env 駆動の `requiredStr` / `optionalStr` パターンで統一されて
   おり、命名規約導出（`<topic>-deadletter`）より明示 env の方が IaC / 本番運用で設定源が一意になる。
2. **handler 抽象の境界形** → pubsub パッケージ内 `MessageHandler` interface + `HandlerFunc`
   アダプタ。Envelope パースは #36 スコープ外のため raw `Message`（ID/Data/Attributes/PublishTime）を
   そのまま渡す。worker main は #36 の Dispatcher 未実装のため暫定 handler（受信を message_id 付き
   INFO ログし nil=ack）を注入。根拠: design.md の `NotificationHandler`（Envelope 受け取り）は #36 の
   契約であり、本 Issue では「Dispatcher が無い間も subscriber を起動して受信疎通を成立させる」ことを
   優先（requirements 6.3 の make up 起動要件を満たすため）。
3. **emulator の resource 自動作成** → emulator モード時（`PubSubEmulatorHost` 非空）のみ起動時に
   topic / subscription / dead-letter topic を冪等 ensure。本番モード（emulator host 空）では一切
   自動作成しない（IaC 前提）。根拠: 本番側で requirements 2.4「subscription 不在→受信開始前エラー」を
   成立させつつ、`make up`（emulator）で worker が実際に受信可能になる状態（requirements 6.3）を両立する。

## #36 への引き継ぎ事項

- **暫定 handler の差し替え**: `cmd/worker/main.go` の `runWorker` 内 `HandlerFunc`（INFO ログ + ack）を
  実 Dispatcher（design.md `Dispatcher` / `NotificationHandler`）に差し替える。
- **Envelope パース**: 本 Issue は raw `pubsub.Message`（Data=未パース payload）を渡す。#36 で
  Envelope（MessageID/NotificationType/EnterpriseName/Payload/PublishTime）へパースする。
- **dedupe（冪等処理）**: `notification_dedupe` ベースの MessageID 重複排除は #36。
- **未割当退避**: `unassigned_notifications` への退避・enterprise→tenant 解決失敗時処理は #36。
- **dead-letter 送出の発火点**: `Subscriber.PublishToDeadLetter` は IF を提供するのみ。実際に「何回
  nack したら dead-letter に送るか」の判定（retry 上限・DeliveryAttempt 参照）は #36 の Dispatcher が担う。

## 確認事項（Reviewer / Architect 向け）

- **subscription 側 deadLetterPolicy との関係**: 本 Issue は worker 側 publish 抽象で dead-letter を
  実装した（requirements 5 の文言「dead-letter topic に publish」に従う）。Pub/Sub の subscription
  ネイティブ deadLetterPolicy（DeliveryAttempt 超過で自動転送）は採用していない。両者の使い分け
  （手動 publish vs ネイティブ policy）は #36 の retry/dead-letter 戦略確定時に Architect が再確認する
  余地がある。design.md は書き換えていない（実装 PR では spec 編集禁止のため本欄に記載）。
- **既存 unformatted ファイル群**: `gofmt -l .` が `internal/logger/logger.go` 等の既存ファイルを
  flag するが、これらは本 Issue 着手前（develop）から unformatted であり本 Issue の変更対象外
  （`git show develop:...` で確認済み）。領分外のため reformat していない。
- **4.2（pull 抑制）の検証粒度**: `MaxOutstandingMessages` は `ReceiveSettings` に反映する実装とし、
  設定値が Subscriber に保持されることをテストした。実際の「上限到達時の pull 抑制」挙動は SDK
  （`cloud.google.com/go/pubsub`）の責務であり、SDK 内部挙動の再現テストは行っていない（外部 SDK の
  挙動を二重実装しない方針）。

## Feature Flag Protocol

対象 repo の `CLAUDE.md` の `## Feature Flag Protocol` は `**採否**: opt-out` のため、通常フローで
実装した（flag 分岐なし）。

STATUS: complete
