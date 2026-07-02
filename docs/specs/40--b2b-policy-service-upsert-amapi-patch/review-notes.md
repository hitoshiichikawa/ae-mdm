# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4.8 timestamp=2026-06-30T00:00:00Z -->

## Reviewed Scope

- Branch: claude/issue-40-impl--b2b-policy-service-upsert-amapi-patch
- HEAD commit: e49dad3
- Compared to: develop..HEAD（実評価は `develop...HEAD`（three-dot）で本ブランチ固有差分を分離）

> 注: `develop..HEAD`（two-dot）には `notification/` / `tenant/` / `errors/` /
> `39--a6b-notification-dispatcher` spec の **削除**が混入するが、これは本ブランチが
> `dec415a`（spec #40）起点で、develop が後から #39 をマージしたことによる **既知の乖離**
> （impl-notes task 6 に記録済み）。本ブランチ自身の変更ではなく、PR 時 rebase（別 stage の
> 責務）で解消される。レビュー判定は merge-base からの three-dot 差分（policy package +
> cmd/api 配線 + spec 進捗ファイルのみ）に対して実施した。

## Verified Requirements

- 1.1 — `service.go` Create（検証→enterprise解決→`UpsertPolicy`）/ `TestService_Create`
- 1.2 — `service.go` Update（既存行確認→`UpsertPolicy` 反映）/ `TestService_Update`
- 1.3 — Create/Update が AMAPI 成功後に `repo.Insert/Update` で snapshot 永続化 / `TestService_Create`（insert called once）
- 1.4 — AMAPI 失敗時に snapshot を永続化せず伝達 / `TestService_Create`（insert NOT called on AMAPI failure, l.400）/ `TestService_Update`（l.700）
- 1.5 — 「AMAPI 反映成功後に永続化」の順序不変条件で中間状態を残さない / Create・Update テストの順序検証（true な連続更新結合は task 7 optional に委譲）
- 2.1 — Service が AMAPI/保存前に `validate()`（mapper+`Validate`）を実行 / `TestService_Create_Validation`
- 2.2 — アプリ 3000 件超→422 拒否+提示 / `TestService_Create_Validation`（CodeBusinessRule）/ `TestHandler_Create_BusinessRuleViolation_Returns422`
- 2.3 — 必須項目不正→400 拒否+不正項目提示 / `mapper.go` invalidField / `TestRawToPolicyInput_TypeMismatch` / `TestHandler_Create_InvalidField_Returns400WithAllDetails`
- 2.4 — 複数不正の全件提示（mapper 変換不能 + Validator 検証不正の結合）/ `service.go` validate / `TestService_Create_Validation`（l.562 both kinds）
- 2.5 — 1 件以上不正→AMAPI/保存を一切行わず早期 return / `TestService_Create_Validation`（amapi=0 insert=0, l.356/359/522）
- 3.1 — `Assign`→`AssignPolicyToDevice`（applied_policy_id UPDATE）/ `TestService_Assign`
- 3.2 — 割当 policy 自テナント不在→複合 FK 違反を NotFound 写像 / `mapAssignError` / `TestMapAssignError_FKViolation` / `TestService_Assign`
- 3.3 — 割当先 device 自テナント不在→affected=0 を NotFound 写像 / `TestService_Assign`
- 3.4 — 割当済み端末への更新配信前提（applied_policy_id 確定 + upsert 配信前提）/ Assign + Update 実装
- 4.1 — 他テナント policy 更新拒否（Handler own-tenant authz + Update の Get NotFound + RLS）/ `TestHandler_Create_Viewer_Returns403`（RBAC 経路）
- 4.2 — 他テナント policy を割当対象指定→拒否 / `mapAssignError`（FK→NotFound）/ `TestService_Assign`
- 4.3 — 他テナント device を割当先指定→拒否 / affected=0→NotFound / `TestService_Assign`
- 4.4 — 一覧/参照は自テナントのみ（`WHERE tenant_id` + RLS）/ `TestService_Get` / `TestService_List` / `TestHandler_List_TenantAdmin_Returns200OwnTenant`
- 4.5 — 拒否時に存在差非露出（`ErrPolicyNotFound` 汎用 message）/ `TestErrPolicyNotFound_NoExistenceLeak` / `TestHandler_Get_NotFound_Returns404`
- 5.1 — 作成イベントを監査記録 / `service.go` record / `TestService_Create`（audit event）
- 5.2 — 更新イベントを監査記録 / `TestService_Update`
- 5.3 — 削除イベントを監査記録（割当済み端末ありは 409 伝達）/ `service.go` Delete / `TestService_Delete` / `TestHandler_Delete_Conflict_Returns409`
- 5.4 — 監査 Detail / ログに機密値を載せない / `TestService_NoSecretLeak`
- NFR 1.1 — 追加フィールド（minimumApiLevel 等）の pass-through 透過 / `BuildPolicyBody` / `TestBuildPolicyBody_PassThrough`
- NFR 2.1 — 列ごと型付き scan / `scanPolicyRow` / `TestScanPolicyRow_AllColumns` / `TestScanPolicyRow_NullUpdatedBy`
- NFR 2.2 — AMAPI 反映は共有ラッパ経由のみ（`upsertClient`=amapi.Client / main.go で共有 client 再利用）/ `TestBuildPolicyHandler_WiresPolicyDomainNotStub`
- NFR 3.1 — 拒否操作の原因属性を構造化ログ / `logDeny` + `AuthorizeAndLog` / `TestHandler_Create_Viewer_Returns403` / `TestHandler_List_NoClaims_Returns401`
- NFR 3.2 — ログ/監査に raw body 機密値を載せない / `TestService_NoSecretLeak`

## Findings

なし

## Summary

全 numeric AC（Req 1〜5）+ NFR 1〜3 が policy package（Service/Repository/Handler/mapper/
service_types）+ cmd/api 配線の実装と対応テストで裏打ちされており、安全側 AC（1.4 AMAPI
失敗→非永続化 / 2.5 検証失敗→AMAPI・保存非呼出 / 5.4 機密非露出）の assertion も実体を伴う。
`go build ./...` と `go test ./internal/policy/... ./cmd/api/...` は green。`_Boundary:_`
逸脱なし（Validator 占有の types.go/validator.go 不変、amapi/audit/tenant は呼び出しのみ）。
tasks.md の差分は checkbox 進捗マークのみで本文不変。two-dot 差分に現れる notification 等の
削除は #39 マージ前起点による既知の乖離で本ブランチの変更ではなく、PR 時 rebase で解消される。

RESULT: approve
