# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4-8 timestamp=2026-07-03T02:00:00Z -->

## Reviewed Scope

- Branch: claude/issue-9-impl--c1
- HEAD commit: 05899717275fd93e87ddf050c0a47bf906928bdb
- Compared to: develop..HEAD（HEAD 全体レビュー / 全 numeric AC を対象）
- Feature Flag Protocol: opt-out（CLAUDE.md `## Feature Flag Protocol` 採否=opt-out）→ 通常の 3 カテゴリ判定のみ実施
- Verify: `go build ./...`（exit 0）+ `go test ./internal/device/... ./internal/notification/... ./cmd/api/...`（全 ok）を reviewer 側で再実行し green を確認

## Verified Requirements

- 1.1 — `service.List` + `repository.ListByTenant`（`WHERE tenant_id=$1` + RLS）/ `handler.list` が `claims.TenantID` を渡す。test: `TestHandler_List_TenantAdmin_Returns200OwnTenant`（svc.listTenant==tenantID）
- 1.2 — `parseComplianceFilter` + `ListByTenant` の `compliance_status=$::compliance_status`。test: `TestHandler_List_ParsesFilterQuery_Returns200`（compliance=non_compliant）+ integration `device_repository_test.go`
- 1.3 — `parseModeFilter` + `mode=$::device_mode`。test: 同 filter query（mode=dedicated）
- 1.4 — `parseSyncStateFilter` + `last_status_at < / >= cutoff`。test: 同 filter query（sync_state=delayed）
- 1.5 — `parsePage`/`parsePageSize` + `LIMIT/OFFSET`。test: 同 filter query（page=3&page_size=20）
- 1.6 — `parseListFilter` 未定義 enum/非数値→400。test: `TestHandler_List_InvalidFilterValue_Returns400`（7 ケース）
- 1.7 — `service.List` が非 nil 空 slice。test: `TestService_List_空でも非nil空sliceを返す`
- 2.1 — `rowToDetail`（mode/applied_policy_name/HW/SW/最終同期/compliance 写像）。test: `TestService_Get_全属性を詳細へ写像する`
- 2.2 — `DeviceDetail.InstalledApps` 返却。test: 同上（InstalledApps 検証）
- 2.3 — `DeviceDetail.SyncDelayed`（cutoff 比較）。test: 同上（SyncDelayed=true）
- 2.4 — `ErrDeviceNotFound`→404。test: `TestHandler_Get_NotFound_Returns404`
- 2.5 — `rawOrDefault`（空 jsonb→{}/[]）+ applied_policy_name NULL→""。test: `TestService_Get_空属性はデフォルト値で返す`
- 3.1 — stored `compliance_status` を 4 分類でそのまま返却。test: `TestService_Get_compliance分類をそのまま返す`（4 値）
- 3.2 — `NonComplianceDetails` 併記 + `deriveComplianceStatus`。test: 全属性写像 + `TestStatusApplier_{Empty,NonEmpty}NonComplianceDetails...`
- 3.3 — 未観測 unknown をそのまま返す。test: `TestService_Get_空属性はデフォルト値で返す`（unknown）
- 3.4 — `ComplianceStatusUnsupported` を read の第 4 分類として parse/返却。test: `TestHandler_List_ComplianceUnsupported_Returns200` + compliance 分類そのまま（unsupported）
- 4.1 — `isSyncDelayed`（NULL 非遅延含む）。test: `TestIsSyncDelayed`
- 4.2 — `NewService` 閾値注入。test: `TestService_同期遅延の閾値が設定値で境界を動かす`（24h/48h で境界移動）
- 4.3 — strict `Before`（閾値ちょうど非遅延）。test: `TestIsSyncDelayed`（ちょうど→false）
- 5.1 — `GetByID` 0 行→存在秘匿 `ErrDeviceNotFound`。test: `TestHandler_Get_NotFound_Returns404`（body に device id 非露出）+ integration
- 5.2 — 不在と越境で同一応答。test: `TestHandler_Get_NonExistentAndCrossTenant_IdenticalResponse`
- 6.1 — `AggregateOverview` + `foldOverview`（4 分類 0 埋め）。test: `TestService_Overview_flat件数行をtenant単位に畳み込む` + `TestAdminHandler_Overview_SuperAdmin_NoTenantID_Returns200AndEstablishesSuperAdminContext`
- 6.2 — 非 SuperAdmin を probe-tenant authz で 403。test: `TestAdminHandler_Overview_NonSuperAdmin_Returns403`（admin aud/tenant-console 双方）+ `..._NoClaims_Returns401`
- 6.3 — `parseTenantIDQuery` 絞り込み（不正書式 400）。test: `TestAdminHandler_Overview_TenantIDQuery_NarrowsToTenant_Returns200` + `..._InvalidTenantID_Returns400`
- 6.4 — 全体 0 件→非 nil 空 slice / `[]` 200。test: `TestService_Overview_全体0件...` + `TestAdminHandler_Overview_EmptyResult_Returns200EmptyArray`
- 7.1 — `StatusHandler.Handle`→`StatusApplier.ApplyStatusReport`→`UpdateFromStatusReport`。test: `TestHandle_FullPayloadMapsAllFields` + integration `device_test.go`（read-after-write）
- 7.2 — pointer nil + `COALESCE($n, col)` 部分更新。test: `TestHandle_PartialPayloadKeepsMissingFieldsNil` + `TestStatusApplier_PartialPayloadKeepsMissingFieldsNil`
- 7.3 — `Service` に write 無 + route が GET のみ。test: `TestHandler_WriteMethods_NotRouted`（POST/PUT/DELETE→405/404）
- NFR 1.1 — migration 0019 の複合 index `idx_devices_tenant_compliance(tenant_id, compliance_status)` を実装。task 1 が `_Requirements_partial:_` で EXPLAIN 検証を deferrable task 8.1（`- [ ]*`）へ委譲。実装成果物（index）は存在し、性能 KPI 検証は optional task 側で担保
- NFR 2.1 — 同期 UPDATE による read-after-write。test: integration `device_test.go`（`DATABASE_URL` 未設定時 Skip / compile 通過確認済み）
- NFR 3.1 — RLS 全 read 経路 + `WHERE tenant_id` 二重防御 / 拒否経路の構造化 WARN。test: repository integration + handler deny WARN 検証

## Findings

なし

## Summary

全 numeric AC（1.1〜7.3 / NFR 1.1・2.1・3.1）が最新差分の実装と近傍テストで裏打ちされている。
変更は `internal/device/*`・`notification/status_handler.go`・migration 0019・`cmd/api/main.go(+test)`・
spec 進捗マーキングに閉じ、tasks.md の `_Boundary:_`（device.*, notification.StatusHandler,
device.Repository）と design.md Modified Files に整合する。`cmd/worker/main.go` は未変更で境界を守り、
tasks.md 差分は `- [ ]`→`- [x]` の進捗マーキングのみで spec 本文の書き換えは無い。build 成功・対象テスト
全 green を reviewer 側で再確認した。boundary 逸脱・AC 未カバー・missing test いずれも検出されず。

RESULT: approve
