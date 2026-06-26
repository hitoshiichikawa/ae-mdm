# Review Notes

<!-- idd-claude:review round=1 model=claude-sonnet-4-5 timestamp=2026-06-26T20:30:00Z -->

## Reviewed Scope

- Branch: claude/issue-37-impl--a3b-rbac-authorizer-api-admin
- HEAD commit: ee6164bf726d027e22d4e797f8f54295f4acf195
- Compared to: develop..HEAD
- 単一実装パス（tasks.md / design.md は不在 / Architect 未起動）

## Verified Requirements

### Req 1: 表駆動の許可マトリクス
- 1.1 — `backend/internal/platform/authz/permissions.go` の `permissionMatrix` が 4 ロール × action × resource を単一定数テーブルで保持
- 1.2 — `isAllowedByMatrix` + `Authorize` が allow/deny を一意に返す
- 1.3 / 1.4 — `TestAuthorize_Matrix_AllRoles_PrimaryActions` が 35 ケース表駆動で allow/deny の両側を網羅
- 1.5 — `TestAuthorize_UnknownRole_FailsClosed` + `isKnownRole`
- 1.6 — `TestAuthorize_UnknownAction_FailsClosed` / `TestAuthorize_UnknownResource_FailsClosed`
- 1.7 — design.md Permission Matrix と整合する許可関係を matrix-test の個別ケース（policy:create/update/delete=TenantAdmin、wipe=TenantAdmin、lock/reboot=TenantAdmin+Operator、tenant:create/delete=SuperAdmin、audit_log cross-tenant=SuperAdmin）で verify

### Req 2: `/api/admin/*` ガード
- 2.1 / 2.2 — `RequireAdminConsoleAndSuperAdmin` の 2 段判定（Console / IsSuperAdmin）
- 2.3 — `TestRequireAdminConsoleAndSuperAdmin_Success_PassesToNext`
- 2.4 — `TestRequireAdminConsoleAndSuperAdmin_TenantConsole_Returns403`
- 2.5 — `TestRequireAdminConsoleAndSuperAdmin_AdminConsole_NotSuperAdmin_Returns403`
- 2.6 — `TestRequireAdminConsoleAndSuperAdmin_NoSession_Returns401`
- 2.7 — `containsTenantPath` で body に対象リソース ID 露出が無いことを verify

### Req 3: cross-tenant 操作
- 3.1 — `authz.Request` 6 フィールド構造体（Roles / SessionTenantID / Audience / Action / Resource / TargetTenantID）
- 3.2 — `TestAuthorize_SameTenant_NoSuperAdmin_StillAllowedByMatrix`
- 3.3 — `TestAuthorize_CrossTenant_AdminConsoleSuperAdmin_Allowed`
- 3.4 — `TestAuthorize_CrossTenant_NotAdminConsole_Denied`
- 3.5 — `TestAuthorize_CrossTenant_NotSuperAdmin_Denied`
- 3.6 — `DenyReasonCrossTenant` enum + middleware の body 検査で対象リソース存在有無を区別しない

### Req 4: targetTenantID fail-closed
- 4.1 / 4.2 / 4.3 — `TestAuthorize_TargetTenantID_EmptyOrInvalid_FailsClosed`（empty / malformed / partial-uuid / uuid-nil-string の 4 sub case）+ `parseTargetTenantID` + `uuid.Nil` チェック
- 4.4 — `TestAuthorize_TargetTenantID_FailClosed_PreemptsRoleAndAudience`
- 4.5 — `DenyReasonTargetTenantInvalid` enum が他の区分から distinct（`TestAuthorize_DenyReasonsAreDistinct`）

### Req 5: 入力契約と返り値
- 5.1 — `authz.Request` の 6 フィールド宣言
- 5.2 — `authz.Decision{Allowed, DenyReason}` 構造体
- 5.3 — `TestAuthorize_DenyReasonsAreDistinct` が 7 区分 enum を distinct verify（5 区分要件 + 未知 fail-closed を 3 サブ区分に細分）
- 5.4 — `TestAuthorize_Deterministic_NoExternalIO`（100 回呼び出しで同一結果）
- 5.5 — `TestAuthorize_MultiRole_TakesMostPermissive` / `TestAuthorize_MultiRole_UnknownPlusKnown_KnownWins`

### Req 6: 既存 Session との結合
- 6.1 — `RequireAdminConsoleAndSuperAdmin` が `AuthClaimsFromContext` 経由で claims 取得
- 6.2 — `AuthClaims.Console` / `IsSuperAdmin` / `AdminUserID` 参照経路 + `auth/middleware.go` で `session.Console` を `AuthClaims.Console` に転記
- 6.3 — `TestRequireAdminConsoleAndSuperAdmin_NoSession_Returns401`
- 6.4 — 複数 role 兼務の matrix OR 評価（`TestAuthorize_MultiRole_TakesMostPermissive`）
- 6.5 — `TestAuthorize_UnknownAudience_FailsClosed` + `TestRequireAdminConsoleAndSuperAdmin_EmptyConsole_Returns403`（legacy claims 後方互換）

### Req 7: 監査・観測性
- 7.1 — `RequireAdminConsoleAndSuperAdmin` が 3 拒否経路（session_missing / audience_mismatch / super_admin_not_present）で `log.Warn` を呼ぶ
- 7.2 — `assertLogFieldExists` で `authz_deny_reason` / `console`（audience）field を verify
- 7.3 — middleware が session_hash_prefix を出さず（Issue #33 auth middleware と重複回避）、raw session id も含めない
- 7.4 — `assertLogFieldExists(t, log, "actor_id", claims.AdminUserID.String())` で AdminUserID field を verify
- 7.5 — `assertLogFieldExists(t, log, "request_id", "test-req-id-123")` で request_id field を verify

### Non-Functional Requirements
- NFR 1.1 — 各 fail-closed テスト（unknown role / action / resource / audience / target empty）
- NFR 1.2 — `TestAuthorize_Deterministic_NoExternalIO` + Authorizer 構造体に状態なし
- NFR 2.1 — `permissionMatrix` 単一定数テーブル
- NFR 2.2 — `TestAuthorize_Matrix_AllRoles_PrimaryActions` の 4 ロール × 主要 action × 主要 resource 全組合せ網羅
- NFR 3.1 — `Authorizer.Authorize` は map index のみで完了
- NFR 3.2 — `RequireAdminConsoleAndSuperAdmin` は `AuthClaimsFromContext` + log のみで I/O なし
- NFR 4.1 — `auth.Session` 構造体は変更しない（middleware.go の Console フィールド読み取りのみ追加）
- NFR 4.2 — `oidc.Console` 値「tenant-console」「admin-console」をそのまま流用、独自 audience 値を導入しない

### 境界違反チェック
- 単一実装パスのため `_Boundary:_` アノテーション不在
- 差分は authz パッケージ新規追加 / httpserver の admin_middleware 拡張（既存 `RequireSuperAdmin` を温存） / auth/middleware.go の Console 転記のみで、既存外部公開 API の semantics を破壊する変更なし

### Feature Flag Protocol
- CLAUDE.md の `**採否**: opt-out` のため flag 観点の確認は適用外

## Findings

なし

## Summary

Req 1〜7 + NFR 1〜4 のすべての AC について実装またはテストでカバーが確認できた。
4 ロール × action × resource 表駆動マトリクス、`/api/admin/*` 2 条件 AND ガード、
cross-tenant fail-closed、targetTenantID fail-closed、複数 role OR 評価、決定論性、
構造化拒否ログのすべてが reviewed scope 内で観測可能。境界違反なし、reject 該当カテゴリ
（AC 未カバー / missing test / boundary 逸脱）いずれも未検出。

RESULT: approve
