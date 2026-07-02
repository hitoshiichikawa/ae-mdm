# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4.8 timestamp=2026-07-02T21:55:32Z -->

## Reviewed Scope

- Branch: claude/issue-11-impl--d1-backend
- HEAD commit: e5cabce3b3efa0cb1d3c1b8e36841b67a25a13ce
- Compared to: develop..HEAD
- 実効 diff: merge-base `beb99bc`...HEAD（三点比較）。branch は develop の older fork 点から派生しており、
  `develop..HEAD`（二点）には develop 前進分（enrollment / notification / tenant / migration 0018 の削除）が
  混入するため、本 Issue の実変更のみを表す三点比較で AC 判定した。三点 diff は
  `backend/internal/app/*`・`backend/cmd/api/main.go`・`main_test.go`・spec 3 ファイルの 12 files のみ。

## Verified Requirements

- 1.1 — `service.CreatePlayToken`（service.go:156）が `webTokenClient.CreateWebToken` の Value を
  `PlayTokenView` で返す / `TestService_CreatePlayToken/正常発行...` + `TestHandler_CreatePlayToken_Viewer_Returns200`
- 1.2 — parent_frame_url 空/空白を TrimSpace で 400（service.go:159）/ `parent_frame_url が空のとき 400...`・
  `空白のみのとき 400` + `TestHandler_CreatePlayToken_MalformedJSON_Returns400`
- 1.3 — 未バインドは `enterpriseResolver` の error を伝達（service.go:166）/ `未バインドテナントのとき resolver の error を伝達...`
  + `TestHandler_CreatePlayToken_NotBound_Returns422`
- 2.1 — `service.ListApps`（service.go:184）→ Repository.List 委譲 + rowToView 写像 /
  `TestService_ListApps/Repository.List の結果を...写像` + `TestHandler_ListApps_Viewer_Returns200OwnTenant`
- 2.2 — List/collect が非 nil 空 slice（repository.go:128, service.go:192）/ `0 件のとき非 nil の空 slice` +
  `TestCollectTenantAppRows_EmptyReturnsNonNilSlice`
- 2.3 — tenant_id 述語 + RLS 非昇格（repository.go:82）/ `TestListSQL_TenantPredicate`・
  `TestAllSQL_NoPrivilegeEscalation` + handler own-tenant 委譲
- 3.1 — `service.SyncApps`（service.go:200）→ Repository.Upsert アトミック反映（client-relayed 承認結果 /
  design Option A で確定）/ `正常同期のとき反映件数...Upsert へ委譲`
- 3.2 — `INSERT ... ON CONFLICT (tenant_id, package_name) DO UPDATE`（repository.go:91）/
  `TestUpsertSQL_OnConflictDoUpdate`（real-DB 冪等は 6.1 で deferred）
- 3.3 — `SyncResult{SyncedAt, Count}` 返却（service.go:235）/ `正常同期のとき反映件数と非 zero の同期時刻` +
  `TestHandler_SyncApps_TenantAdmin_Returns200`
- 3.4 — 空リストは count 0（repository.go:191, service.go）/ `空リスト同期のとき count 0` +
  `TestUpsert_EmptyList_ReturnsZeroWithoutPool`
- 3.5 — bind gate で未バインドは Upsert 未呼び出し + error 伝達（service.go:204）/
  `未バインドテナントのとき Upsert を呼ばず error を伝達...`
- 3.6 — Repository error は count 0 のまま伝達 + 単一 tx rollback（service.go:224, repository.go:210）/
  `Repository が error を返すとき count 0 で伝達...`
- 4.1 — own-tenant RBAC（`AuthorizeAndLog` / TargetTenantID=claims.TenantID / handler.go:177）+ cmd 配線
  `appHandler.Mount(routers.API)` / `TestHandler_SyncApps_Operator_Returns403`・`_Viewer_Returns403`・
  `_ListApps_NoClaims_Returns401` + `TestBuildAppHandler_WiresAppDomainNotStub`
- 4.2 — deny は汎用 message（"app operation forbidden"）+ RLS 分離で存在有無を露出しない（handler.go:191）/
  handler own-tenant 委譲テスト
- 5.1 — `service.CheckAppsApproved`（service.go:239）→ Repository.ApprovedPackages 集合取得 /
  `全 package が承認済みのとき nil...ApprovedPackages へ委譲` + `TestApprovedPackagesSQL_TenantPredicate`
- 5.2 — 未承認 1 件で `ErrAppNotApproved`(422)（service.go:258）/ `1 件が未承認のとき ErrAppNotApproved(422)` +
  `越境 package...未承認扱いで拒否`
- NFR 1.1 — Value をログに出さない（service.go 成功パス無ログ）/ `TestService_CreatePlayToken_NoTokenLeak`
- NFR 1.2 — Handler も Value 非ログ + RLS 恒常分離 / `TestHandler_CreatePlayToken_DoesNotLogTokenValue`
- NFR 2.1 — AMAPI は共有ラッパ経由（`webTokenClient`=amapi.Client / cmd で共有 amapiClient 再利用）/
  `TestBuildAppHandler_WiresAppDomainNotStub`
- NFR 3.1 — 成否両経路で `record`（app_sync / Detail={count,result}）（service.go:294）/
  `正常同期のとき成功監査...` + `監査 Record が失敗しても...WARN`
- NFR 3.2 — Detail/ログに enterprise_name・資格情報を載せない / `同期監査 Detail に enterprise_name 等の機密値を含めない`

## Findings

なし

## Summary

全 numeric AC（1.1–5.2）+ NFR 1.1/1.2/2.1/3.1/3.2 に観測可能な実装と co-located テストが対応し、
`_Boundary:_`（App{Repository,Service,Handler} + task 6 の cmd/api 配線）内に収まる。tasks.md 差分は
sanctioned な checkbox 進捗マーク（`- [ ]`→`- [x]`）のみでタスク本文・アノテーションは不変。
`go build ./...` / `go vet` / `go test ./internal/app/... ./cmd/api/...` を再実行し全て green を確認。
補足（informational / reject 対象外）: branch は develop 前進分より前で fork した stale base のため
merge 前に rebase が必要になり得るが、3 カテゴリ判定の対象外であり PjM / rebase 経路の領分。

RESULT: approve
