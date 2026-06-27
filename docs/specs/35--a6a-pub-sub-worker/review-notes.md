# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4.8 timestamp=2026-06-27T14:39:54Z -->

## Reviewed Scope

- Branch: claude/issue-35-impl--a6a-pub-sub-worker
- HEAD commit: 69a04aea35a244b0633d13c6e41033613cca50ea
- Compared to: develop..HEAD
- Feature Flag Protocol: 採否 = opt-out（flag 観点の確認は行わない / 通常 3 カテゴリ判定）
- 実行確認: `go test -count=1 ./internal/platform/pubsub/ ./cmd/worker/ ./internal/config/` → 全 PASS

## Verified Requirements

- 1.1 — `TestNewClient_ConstructsWithProjectID`（`NewClient` が `cfg.PubSubProjectID` で構築、`ProjectID()` で参照可能）
- 1.2 — `TestNewClient_EmulatorVsProductionOptions`（emulator host 非空 → endpoint+no-auth+insecure の 3 option / `buildClientOptions`）
- 1.3 — 同上（emulator host 空 → ADC=0 option = 本番接続）
- 1.4 — `TestNewClient_BuildFailure_ReturnsStructuredError`（`CodeUnavailable`/transient、部分初期化 client を返さない）
- 1.5 / 1.6 — `TestClient_Close_Idempotent`（`sync.Once` による多重 Close 安全、2 回目以降 no-op で nil）
- 2.1 / 2.2 — `TestSubscriber_ReceivesAndHandsToHandler`（pull 受信 → handler へ payload 引き渡し）
- 2.3 — `handleOne` が `logger.MessageID(raw.ID)` で受信ログを記録（受信系テストで経路実行。observability AC）
- 2.4 — `TestSubscriber_SubscriptionMissing_ReturnsErrorWithoutLoop`（`CodeNotFound`、受信ループ未開始＝handler 未呼び出し）
- 2.5 — `TestSubscriber_EmptyPayload_StillHandedToHandler`（空 payload も破棄せず handler へ）
- 3.1 — `TestSubscriber_HandlerSuccess_Acks`（成功時 ack、再配信なし＝1 回）
- 3.2 / 3.3 — `TestSubscriber_TransientError_Nacks_Redelivered`（`errors.ShouldAck` 経由で transient は nack→再配信）
- 3.4 — `TestSubscriber_PermanentError_Acks_NotRedelivered`（非 transient は ack、無限再配信を防止）
- 3.5 — `handleOne` が ack/nack 結果を `logger.MessageID` 付きで記録（3.x テストで経路実行。observability AC）
- 4.1 — `TestNewSubscriber_MaxOutstandingAccepted`（1/5/1000 受理、`maxOutstandingMessages` に保持）
- 4.2 — `Run` が `ReceiveSettings.MaxOutstandingMessages` に設定値を反映。実 pull 抑制は SDK 責務（外部 SDK 挙動の二重実装回避 / CLAUDE.md モック方針と整合）
- 4.3 — `TestNewSubscriber_MaxOutstandingBelowOne_Rejected`（負値 → `CodeConfigInvalid`）
- 5.1 / 5.2 — `TestSubscriber_DeadLetterPublish_Success`（payload + `original_message_id` が DL topic に publish）
- 5.3 — DL publish 成功時に `dead_letter_message_id` / `message_id` 付きで記録（5.1/5.2 テストで経路実行）
- 5.4 — `TestSubscriber_DeadLetterPublish_TopicUnset_ReturnsError`（topic 未設定 → `CodeConfigInvalid`、ack しない）
- 6.1 — `runBootstrap`→`runWorker`（config.Load→logger→NewClient→NewSubscriber→subscriber 起動の順序）。subscriber 起動は `superviseWorker` 系テストでカバー
- 6.2 — `superviseWorker(... handler pubsub.MessageHandler)` への DI、`TestSuperviseWorker_GracefulShutdownOnSignal`（handler に受信が届く）
- 6.3 — `newHealthzMux` で :8090 /healthz を温存（既存 scaffold 挙動の保持）
- 6.4 — `TestSuperviseWorker_GracefulShutdownOnSignal`（ctx キャンセル → subscriber 停止 + healthz graceful shutdown + code 0）
- 6.5 — `TestSuperviseWorker_SubscriberFatalError_ReturnsNonZero`（subscription 不在 → code 1 / fail-fast）
- NFR 1.1 — `TestNewClient_BuildFailure_*` + `runWorker` の client 構築失敗 → ERROR ログ + 非ゼロ終了
- NFR 1.2 — client/subscriber 各操作で `logger.Err` 付き構造化ログを 1 件出力（経路実行）
- NFR 2.1 — `shutdownTimeout=5s` の `shutdownHealthz` + graceful shutdown テスト
- NFR 2.2 — `shutdownHealthz` が Shutdown 失敗時に ERROR ログ + 非ゼロ終了（コード経路として実装）
- NFR 3.1 — `-healthcheck` / `runHealthcheck` / :8090 /healthz を温存（既存 scaffold 整合）
- 補足（config 追加） — `PubSubDeadLetterTopic`（env `PUBSUB_DEAD_LETTER_TOPIC` / optional / default `""`）は `TestLoad_PubSubDeadLetterTopic_OptionalDefaultEmpty` でカバー

## Findings

なし

## Summary

requirements.md の Requirement 1〜6 / NFR 1〜3 の全 numeric ID について、土台スコープ（dead-letter
送出抽象・ack/nack 抽象）に沿った実装と pstest ベースの AAA テストを確認した。境界は tasks 6.1
の `_Boundary: PubSubClient, PubSubSubscriber_` と一致し、`cmd/worker/main.go` は task 6.1 詳細項目で
明示された対象、`config`（`PubSubDeadLetterTopic`）追加は Requirement 5 を満たすための最小 additive
変更（requirements.md「確認事項」で予見済み・専用テストあり・既存 config 挙動を不変）で逸脱に当たらない。
ack/nack/再配信/dead-letter/設定拒否/graceful shutdown の機能系は直接テストで担保され、観測系ログ AC
（2.3/3.5/5.3/NFR1.2）は経路実行で、pull 抑制（4.2）は SDK 委譲で妥当にカバーされている。対象 3
パッケージのテストを再実行し全 PASS を確認した。

RESULT: approve
