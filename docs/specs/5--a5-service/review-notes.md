# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4-20250514 timestamp=2026-06-27T21:09:10Z -->

## Reviewed Scope

- Branch: claude/issue-5-impl--a5-service
- HEAD commit: 0002fffd4f0a2a5a4dd939d09850375eb4207f0f
- Compared to: develop..HEAD

差分は `backend/internal/audit/*`（types/clock/failure_kinds/doc/service/repository/handler/admin_handler
+ 各単体テスト）、`backend/cmd/api/main.go`（DI 配線 + Mount）、`backend/test/integration/audit_test.go`、
および spec docs（`tasks.md` の checkbox 進捗、`impl-notes.md`）に限定。Feature Flag Protocol は
`opt-out` のため flag 観点の確認は不要（通常の 3 カテゴリ判定のみ実施）。

## Verified Requirements

- 1.1 — `service.Record` → `repository.Insert`（全フィールド追記 / `insertAuditLogSQL`）/ `TestService_Record` / integration `SuperAdminSeesAllDesc`
- 1.2 — NULL テナント: `nullableTenantID`（`uuid.Nil`→NULL bind）/ `scanEvent`（NULL→`uuid.Nil`）/ `TestNullableTenantID_NilUUIDMapsToNullBind` / integration `AppendOnly_NullTenantRowImmutable`
- 1.3 — `ResultSuccess` 写像 / `TestService_Record`
- 1.4 — `ResultFailure` 写像 / `TestService_Record`（結果失敗ケース）
- 1.5 — `Service` / `Repository` interface は Record/List/Insert/Select のみ（update/delete 非公開、コンパイル時担保）
- 1.6 — INSERT 失敗を `CodeUnavailable` で wrap し成功扱いしない / `TestService_List_PropagatesSelectError` + `TestService_Record`（Repository error 伝播）
- 1.7 — `Event.Detail` 素通し + 機密 sanitize は呼び出し側責務を `types.go`/`doc.go`/`service.go` godoc に明記
- 2.1 — own-tenant `ORDER BY occurred_at DESC` + RLS / `TestHandler_List_TenantAdmin_OwnTenant_Returns200`
- 2.2 — `event_type` 絞り込み / `TestHandler_List_MapsAllFilterFields` / `TestBuildSelectQuery_AppendsOnlySpecifiedFilters`
- 2.3 — `actor_id` 絞り込み / 同上
- 2.4 — `resource_id` 絞り込み / 同上
- 2.5 — `from`/`to` 期間 / `TestHandler_List_MapsAllFilterFields`
- 2.6 — `from` のみ / `TestHandler_List_FromOnly_ReflectedInFilter`
- 2.7 — `to` のみ / `TestHandler_List_ToOnly_ReflectedInFilter`
- 2.8 — 0 件は `[]` で 200 / `TestHandler_List_EmptyResult_Returns200EmptyArray` / `TestService_List_EmptyResult`
- 2.9 — `Filter.TenantID=nil` 固定 + RLS / integration `TenantContextIsolation_OtherTenantAndNullInvisible`
- 3.1 — SuperAdmin TenantContext で全テナント+NULL 降順 / `TestAdminHandler_List_SuperAdmin_NoTenantID_...` / integration `SuperAdminSeesAllDesc`
- 3.2 — `tenant_id` query→`Filter.TenantID` / `TestAdminHandler_List_TenantIDQuery_SetsFilterTenantID` / integration `SuperAdminFilterByTenant`
- 3.3 — event_type/actor/resource/period 絞り込み / `TestAdminHandler_List_MapsAllFilterFields`
- 3.4 — 0 件は `[]` で 200 / `TestAdminHandler_List_EmptyResult_Returns200EmptyArray`
- 3.5 — tenant 無指定で全テナント+NULL 対象（SuperAdmin context）/ integration `SuperAdminSeesAllDesc`
- 4.1 — Operator は own-tenant 経路で 403 / `TestHandler_List_Operator_Returns403`
- 4.2 — 未認証 401 / `TestHandler_List_NoClaims_Returns401` / integration `RoutingSmoke_UnauthenticatedReturns401`
- 4.3 — 他テナント閲覧拒否 + 存在非露出（RLS 0 行）/ integration `TenantContextIsolation_...`
- 4.4 — 横断経路の SuperAdmin 強制（`RequireAdminConsoleAndSuperAdmin` 固定ガード配下に Mount）/ integration `RoutingSmoke_...`
- 4.5 — Viewer は両経路で拒否 / `TestHandler_List_Viewer_Returns403` + admin 固定ガード
- 4.6 — 実 `authz.New()` の `audit_log read` matrix（`ResourceAuditLog`+`ActionRead`）/ handler/admin_handler テスト
- 5.1 — `effectiveFrom`（retentionFloor 算出）/ `TestService_List_RetentionFloor` / `TestService_List_DefaultRetentionDiffersFrom365`
- 5.2 — `occurred_at >= effectiveFrom` を常に付与 / `TestBuildSelectQuery_AlwaysAppliesRetentionFloor` / integration `RetentionFloor_ExcludesOlderRows`
- 5.3 — `max(retentionFloor, from)` 丸め / `TestService_List_RetentionFloor`（前/後両ケース）
- 5.4 — `cfg.AuditLogRetentionDays` をリクエスト毎参照 / `TestService_List_DefaultRetentionDiffersFrom365`
- 6.1 — UPDATE/DELETE メソッド非公開 + integration `AppendOnly_UpdateDeleteRejected`
- 6.2 — UPDATE 拒否 / integration `AppendOnly_UpdateDeleteRejected`
- 6.3 — DELETE 拒否 / 同上
- 6.4 — NULL テナント行の UPDATE/DELETE 拒否 / integration `AppendOnly_NullTenantRowImmutable`
- NFR 1.1 — 保持期間設定可 / `TestService_List_DefaultRetentionDiffersFrom365`
- NFR 1.2 — 保持期間内レコード欠損なし / integration `NormalTenant_CrossTenantInsertRejectedAndInTenantRetained`
- NFR 2.1 — テナント分離の恒常性 / integration `TenantContextIsolation_...`
- NFR 2.2 — テナント不一致 INSERT 拒否（WITH CHECK）/ integration `NormalTenant_...`
- NFR 3.1 — 機密値非格納: DTO 固定 field + `warnFailure` 固定 field のみ / handler テスト
- NFR 3.2 — `failure_kind`（`failure_kinds.go`）構造化 WARN / `TestHandler_List_InvalidQuery_Returns400` / `TestAdminHandler_List_InvalidQuery_Returns400`

## Findings

なし

## Summary

全 numeric AC（Req 1.1〜6.4 / NFR 1.1〜3.2）に観測可能な実装と対応テスト（service/repository/handler/
admin_handler 単体 + 実 DB integration）が揃っており、変更は宣言済み `_Boundary:_` 内（spec docs は
tasks.md の checkbox 進捗と impl-notes.md のみ）に収まる。`go build ./...` / `go vet` / `internal/audit`
単体テストの green を再実行で確認。admin handler の authz↔design 不整合（tenant_id 指定時のみ matrix
判定し、無指定時は固定ガードに依拠）は impl-notes.md「確認事項」で明示され、Req 4.4 は固定ガード Mount で
担保されているため AC ギャップなし。spec 本文は書き換えられておらず Developer の責務境界も遵守。

RESULT: approve
