# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4.8 timestamp=2026-06-30T00:00:00Z -->

## Reviewed Scope

- Branch: claude/issue-39-impl--a6b-notification-dispatcher
- HEAD commit: 88d032da9074883a83a62e5bd0d6339aac3e1b8f
- Compared to: develop..HEAD
- Feature Flag Protocol: opt-out（CLAUDE.md `## Feature Flag Protocol` の採否=opt-out のため
  通常の 3 カテゴリ判定のみを適用し、flag 観点は確認しない）
- verify: `go build ./...` / `go vet ./internal/notification/... ./internal/tenant/...` /
  `go test ./internal/notification/... ./internal/tenant/...` をローカル再実行し green を確認。
  `go vet ./test/integration/...` も pass（実 DB 結合テストは DATABASE_URL 未設定環境で t.Skip）。

## Verified Requirements

- 1.1 — `dispatcher.go:dispatchToHandler`（handler 成功後に `dedupe.MarkProcessed`）/
  test `TestDispatcherHandle`「成功時に handler 1 回呼び dedupe 記録して ack する」
- 1.2 — `dispatcher.go:Handle`（`if processed { return nil }`）/ test「既処理 MessageID のとき
  dispatch せず即 ack する」+ integration `TestNotificationDispatch_DuplicateMessageID_DispatchedOnce`
- 1.3 — `dedupe.go:markProcessedSQL`（`ON CONFLICT (message_id) DO NOTHING`）/
  test `TestMarkProcessedSQL_ContainsOnConflictDoNothing` + integration 重複排除テスト
- 1.4 — `dedupe.go:wrapDedupePersistErr` / `unassigned.go:wrapUnassignedPersistErr`
  （`CodeUnavailable, IsTransient:true`）/ test `TestWrapDedupePersistErr_MapsToTransientUnavailable`、
  dispatcher「dedupe 判定の DB 失敗のとき nack 保持」「成功後の dedupe 記録失敗のとき nack 保持」
- 2.1 — `dispatcher.go:dispatchToHandler`（`handlers[env.NotificationType]`）/
  test `TestDispatcherHandle_RoutesByNotificationType`（ENROLLMENT）+ `TestParse_EachNotificationTypeProducesEnvelope`
- 2.2 — 同上（STATUS_REPORT）/ test 同上
- 2.3 — 同上（COMMAND）/ test 同上
- 2.4 — `dispatcher.go:dispatchToHandler`（`if !ok { log.Warn; return nil }`）/
  test「未登録種別のとき取りこぼさず ack 完了扱いにする」
- 2.5 — `verifier.go:Parse`（nil/JSON 不正 → `discardError` IsTransient=false）/
  test `TestParse_InvalidJSONPayloadFailsVerification` / `TestParse_NilMessageFailsVerification` +
  dispatcher「検証失敗（恒常的）のとき dispatch せず ack」
- 2.6 — `verifier.go:Parse`（空 payload / 種別 attribute 欠落・空）/
  test `TestParse_EmptyPayloadIsDiscarded` / `TestParse_UndeterminableTypeIsDiscarded`
- 3.1 — `dispatcher.go:dispatchToHandler`（`db.WithTenantContext` 確立後に handler 呼出）/
  test `TestDispatcherHandle_EstablishesTenantContextBeforeHandler` + integration TypeRouting / Duplicate
- 3.2 — `dispatcher.go:enqueueUnassigned`（found=false → Enqueue）/
  test「tenant 未解決のとき退避して ack」+ integration `..._UnresolvableEnterprise_Quarantined`
- 3.3 — `dispatcher.go:enqueueUnassigned`（退避後 MarkProcessed → nil 返却=ack）/
  test 同上 + integration 退避テスト
- 3.4 — `dispatcher.go:resolveTenant`（`env.EnterpriseName == ""` で逆引きせず found=false）/
  `service.go:TenantIDByEnterpriseName`（空入力で DB 非アクセス）/ test dispatcher「enterprise_name
  空のとき逆引きせず退避」+ `verifier_test`「空 enterprise_name 通過」+ `service_test` 空/空白入力
- 3.5 — `unassigned.go:enqueueUnassignedSQL`（`unassigned_notifications` のみへ INSERT、tenant
  scoped テーブルに触れない）/ test `TestEnqueueUnassignedSQL_InsertsExpectedColumns` +
  dispatcher `..._UnassignedDoesNotTouchTenantHandler`
- 4.1 — `admin_handler.go:list` / `unassigned.go:List`（0 件は非 nil 空 slice）/
  test `TestAdminHandler_List_ReturnsNotifications` / `..._EmptyResult_Returns200EmptyArray` +
  integration admin API 全件取得
- 4.2 — `admin_handler.go:parseFilter` / `unassigned.go:buildUnassignedListQuery`（from/to/type）/
  test `TestAdminHandler_List_MapsAllFilterFields` / `..._PartialFilters` / `TestBuildUnassignedListQuery`
  + integration type / from 絞り込み
- 4.3 — `cmd/api/main.go`（`routers.Admin.Mount` で `RequireAdminConsoleAndSuperAdmin` ガード継承）。
  401 は既存共有 middleware の責務（design Traceability が (admin middleware) に割当 / impl-notes
  task 4 で linkage 明記）。同ガードは既存 `tenant_admin_guard_test.go` で検証済み
- 4.4 — 同上（非 SuperAdmin → 403）。既存ガード middleware に委譲（design / impl-notes で linkage 明記）
- 4.5 — `admin_handler.go:parseFilter`（不正 from/to/type → 400 + 不正項目文言）/
  test `TestAdminHandler_List_InvalidQuery_Returns400WithInvalidItem`
- 5.1 — `dispatcher.go:dispatchToHandler`（handler error 伝播 → transient は nack）/
  test「transient handler 失敗のとき nack 保持し dedupe 記録しない」
- 5.2 — `dispatcher.go:dispatchToHandler`（成功時 ack）/ test「成功時に handler 1 回呼び dedupe 記録して ack」
- 5.3 — IsTransient=true による nack 保持で喪失防止 / test 5.1 と同経路で検証
- 6.1 — integration `TestNotificationDispatch_DuplicateMessageID_DispatchedOnce`
- 6.2 — integration `TestNotificationDispatch_UnresolvableEnterprise_Quarantined`
- 6.3 — integration `TestNotificationDispatch_TypeRouting_DispatchesToCorrectHandler`
- 6.4 — integration `TestNotificationDispatch_QuarantinedVisibleViaAdminAPI`（admin API 経由取得 + 絞り込み）
- NFR 1.1 — `dispatcher.go` は同期 tx 処理で遅延要素（sleep / 追加 queueing）を持たない。latency
  計測は umbrella scope（design 明記）。処理経路に遅延を入れない設計で担保
- NFR 2.1 / 2.2 — 解決成功時のみ tenant context 確立、未解決・未登録・既処理は tenant context 非確立。
  退避は cross-tenant infra テーブルのみ更新 / test `..._UnassignedDoesNotTouchTenantHandler` +
  `..._EstablishesTenantContextBeforeHandler`
- NFR 3.1 — 各段で `logger.MessageID` 付き構造化ログ、wrap error は機密値非補間の固定文言、
  admin failure は failure_kind / test `TestWrap*PersistErr`（固定文言 assert）+ admin の failure_kind assert

## Findings

なし

## Summary

requirements.md の全 numeric ID（1.1〜6.4 および NFR 1.1 / 2.1 / 2.2 / 3.1）が実装とテストで
カバーされており、tasks.md の `_Boundary:_`（notification 各コンポーネント / tenant 逆引き /
cmd/api 配線）からの逸脱は無い。tenant テストダブルの変更は Service IF 拡張に伴う機械的な
コンパイル整合で、境界違反ではない。build / vet / 単体テストはローカル再実行で green。

RESULT: approve
