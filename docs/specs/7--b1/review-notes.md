# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4 timestamp=2026-07-02T17:22:41Z -->

## Reviewed Scope

- Branch: claude/issue-7-impl--b1
- HEAD commit: d204c6b012859bc6147e945cf5e856bf227193e2
- Compared to: develop..HEAD

Feature Flag Protocol: CLAUDE.md `## Feature Flag Protocol` の `**採否**: opt-out` のため、
通常の 3 カテゴリ判定のみを適用（flag 観点は不適用）。本レビューは prompt に per-task range
（range_start_sha / range_end_sha）を含まない HEAD 全体レビューとして実施した。

verify: `go build ./...` PASS / `go vet ./internal/enrollment/... ./internal/notification/... ./cmd/api/...` PASS /
`go test ./internal/enrollment/... ./internal/notification/... ./cmd/api/...` PASS（結合テストは DB 未接続で t.Skip）。

## Verified Requirements

- 1.1 — `TestService_IssueToken_FullyManaged_UsesDisallowedPersonalUsage` / `_ReturnsSecretQRDataInViewOnly`（service.go: AllowPersonalUsage=PERSONAL_USAGE_DISALLOWED 固定 + TokenView で Value/QRCode 返却）
- 1.2 — `TestService_IssueToken_Dedicated_SetsResolvedPolicyName`（DEDICATED で PolicyName=amapi policy id 設定）
- 1.3 — `TestService_IssueToken_FullyManaged_EmbedsTenantIssuerModeInAdditionalData` / `TestAdditionalData_Marshal_WithValues_ContainsTenantIssuerMode`（additionalData に tenant_id/issued_by/mode）
- 1.4 — `TestService_IssueToken_InvalidMode_ReturnsErrorWithoutCallingAMAPI`（不正/未指定モード → ErrInvalidMode・AMAPI 非呼出）
- 1.5 — `TestService_IssueToken_DedicatedWithoutPolicy_ReturnsErrRequiredWithoutCallingAMAPI` / `_DedicatedPolicyNotFound_PropagatesWithoutCallingAMAPI`（policy 未指定/不在 → 発行せずエラー）
- 1.6 — `TestService_IssueToken_AMAPIError_SkipsInsertAndRecordsFailureAudit`（AMAPI エラー → Insert 非呼出で伝達）
- 2.1 — `TestHandler_Issue_TenantAdmin_Returns200` / `TestHandler_Issue_Operator_Returns200`（TenantAdmin/Operator 許可）
- 2.2 — `TestHandler_Issue_Viewer_Returns403`（Viewer 拒否 + deny WARN）
- 2.3 — `TestHandler_Issue_CrossTenantPolicy_Returns404NonExposing`（越境 → 非露出 404 / TargetTenantID=own-tenant 固定）
- 3.1 — `TestEnrollmentHandler_Handle_TenantMatch_RegistersDeviceWithoutQuarantine` / `TestRegistrar_UpsertEnrolledDevice_BindsTenantFromContext` / integration `TestEnrollmentFlow_IssueThenEnroll_...`（突合一致 → 発行元テナントへ登録）
- 3.2 — `TestEnrollmentHandler_Handle_TenantMismatch_QuarantinesWithoutRegister` / integration `TestEnrollmentFlow_TenantMismatchNotification_...`（不一致 → 退避のみ・無更新）
- 3.3 — `TestEnrollmentHandler_Handle_TenantMissing_Quarantines`（tenant_id 欠落/parse 不能 → 退避）
- 3.4 — `TestRegistrar_UpsertEnrolledDevice_EmitsIdempotentOnConflictSQL` / integration `TestEnrollmentFlow_DuplicateDeviceNotification_IdempotentSingleDevice`（ON CONFLICT 冪等 upsert）
- 3.5 — `TestEnrollmentHandler_Handle_AndroidVersion_MapsCompliance`（Android<10 → compliance=unsupported）
- 3.6 — `TestEnrollmentHandler_Handle_RegistrarTransientError_ReturnsNack` / `TestRegistrar_UpsertEnrolledDevice_ExecError_ReturnsTransientUnavailable`（transient 失敗 → 非 ack で再処理保持）
- 4.1 — `TestDeriveStatus_*`（active/expired 境界）/ `TestHandler_List_ReturnsExpiresAtDerivedStatus` / `TestService_ListTokens_DerivesStatusFromExpiresAt`（expires_at 由来 status 伝達）
- 4.2 — integration `TestEnrollmentFlow_InvalidNotification_NoDeviceCreatedForTenant`（observable「未登録」）。used 能動追跡は既存 0004 に消費列が無く design リスク 2 / requirements 確認事項で人間判断へ scope out 済み（設計 PR ゲート通過済み）。observable 部分（未登録）はカバー・テスト済み
- 5.1 — `TestService_IssueToken_Success_RecordsAuditWithSafeFields` / `_AMAPIError_SkipsInsertAndRecordsFailureAudit`（成否いずれも actor/tenant/mode/expires_at/result を Record）
- 5.2 — `TestService_IssueToken_DoesNotLeakSecretToAuditOrLogs`（監査 Detail に Value/QRCode 非混入）
- 6.1 — `TestEnrollmentFlow_IssueThenEnroll_RegistersDeviceForIssuingTenant`（実 PostgreSQL 発行→通知→登録）
- 6.2 — `TestEnrollmentFlow_TenantMismatchNotification_QuarantinedWithoutDeviceRegistration`（不一致 → unassigned 退避）
- 6.3 — `TestEnrollmentFlow_DuplicateDeviceNotification_IdempotentSingleDevice`（重複通知 → devices 件数 1）
- NFR 1.1 — `TestEnrollmentHandler_Handle_AndroidVersion_MapsCompliance`（10 以上サポート / <10 は unsupported 分類）
- NFR 2.1 — `TestRegistrar_UpsertEnrolledDevice_BindsTenantFromContext`（tenant_id を ctx 由来 bind）/ tenant-scoped RLS + 突合不一致テスト
- NFR 2.2 — `TestEnrollmentHandler_Handle_MissingTenantContext_Quarantines` / `_TenantMismatch_...`（一意特定不能時は無更新）
- NFR 3.1 — `TestTokenRow_HasNoSecretFields` / `TestAdditionalData_Marshal_DoesNotLeakSecretKeys` / `TestService_IssueToken_DoesNotLeakSecretToAuditOrLogs` / `TestEnrollmentHandler_Handle_WarnLogsDoNotLeakSecrets`
- NFR 4.1 — `TestEnrollmentHandler_Handle_WarnLogsDoNotLeakSecrets`（退避/サポート対象外/失敗の非機密 field 構造化 WARN）

## Findings

なし

## Summary

全 numeric AC（Req 1.x〜6.x + NFR 1.1/2.1/2.2/3.1/4.1）に観測可能な実装と対応テストを確認。
変更ファイルは design.md File Structure Plan / tasks.md（task 4=EnrollmentRegistrar, task 5=EnrollmentNotificationHandler）の境界内に収まり、
cmd/worker 無変更（design リスク 7）・既存 notification/policy 実装への破壊的変更なし・既存テスト（main_test.go はシグネチャ追従 + assertion 追加のみ）の弱体化なし。
build/vet/unit test いずれも green。Req 4.2 の used 能動追跡未実装は human-gated な設計判断で observable 部分はテスト済みのため reject 対象外。

RESULT: approve
