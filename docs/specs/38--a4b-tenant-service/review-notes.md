# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4.8 timestamp=2026-06-27T22:36:22Z -->

## Reviewed Scope

- Branch: claude/issue-38-impl--a4b-tenant-service
- HEAD commit: 53c41a9a2e254fc6c50f070c2e2249e577da66ff
- Compared to: develop..HEAD

> 注記: 二点間 `develop..HEAD` の diff には `internal/audit/*` 等の大量削除と
> `docs/specs/5--a5-service/*` の変更が現れるが、これは base ブランチ `develop` が本ブランチの
> merge-base（8e4e3bc）より進んでいることによる artifact（develop に #5 の audit 作業が後から
> merge された結果）。三点間 `develop...HEAD`（merge-base 基準）で確認したところ #38 の commit は
> それらのパスを一切変更しておらず、実レビュー対象は `internal/tenant` パッケージ・migration
> 0016・2 本の integration test・impl-notes / tasks.md（checkbox 進捗）に限定される。

## Verified Requirements

- 1.1 — service.go `Create` が pending_bind 行を Insert / `TestService_Create`「正常入力のとき pending_bind...」/ `TestTenantRepository_CRUD_SuperAdminContext`
- 1.2 — `Create`→`amapi.CreateSignupURL`→SignupURL 返却 / `TestService_Create`（su.URL/Name 検証）/ `TestCreate_Success_ReturnsPendingBindAndSignupURL`
- 1.3 — `Create` が name trim 後空で CodeInvalidRequest / `TestService_Create`「name が空白...」/ `TestCreate_EmptyName_Returns400`
- 1.4 — `Create` が CreateSignupURL 失敗時に Insert せず error 伝達 / `TestService_Create`「CreateSignupURL が非 transient error...」（insert==0 検証）
- 1.5 — `Create` の `record(OperationCreate, ...)` / `TestService_Create`（events 検証）
- 2.1 — `Bind`→`CreateEnterprise`→`UpdateBound` + 部分一意 index / `TestService_Bind` success / `TestTenantRepository_UpdateBound_*`
- 2.2 — `UpdateBound` が status='bound' へ遷移 / repository test（Get after bind = bound）
- 2.3 — enterprise_name 保存 + `EnterpriseNameForTenant` 参照 / `TestService_EnterpriseNameForTenant`「bound のとき...」
- 2.4 — `Bind` が CreateEnterprise 失敗時 UpdateBound 未呼出 / `TestService_Bind`「CreateEnterprise が失敗...」（updateBound==0）
- 2.5 — bound 再 bind→ErrConflict + CreateEnterprise 未呼出 / `TestService_Bind`「bound 状態への再 bind...」
- 2.6 — disabled へ bind→ErrInvalidState(422) / `TestService_Bind`「disabled 状態への bind...」
- 2.7 — `Bind` の `record(OperationBind, ...)` / `TestService_Bind`（events 検証）
- 3.1 — `Disable` 確認一致→`UpdateDisabled` で disabled 遷移 / `TestService_Disable`「確認テキストが name と一致...」/ repository test
- 3.2 — confirmation != name→ErrConfirmationRequired(422) / `TestService_Disable`「確認テキストが name と不一致...」/ `TestDisable_ConfirmationMissing_Returns422`
- 3.3 — disabled テナントへの bind/識別子要求/再 disable 拒否 / `TestService_Bind`/`EnterpriseNameForTenant`/`Disable` 各 disabled ケース
- 3.4 — 二重無効化→ErrConflict（already disabled / affected=0） / `TestService_Disable`「既に disabled...」「affected=0...」
- 3.5 — `Disable` の `record(OperationDisable, confirmed=true)` / `TestService_Disable`（ConfirmationCompleted 検証）
- 4.1 — `List` / `TestService_List`「複数テナント...」/ `TestTenantRepository_CRUD_SuperAdminContext`
- 4.2 — `Get` / `TestService_Get`「存在するテナント...」/ `TestGet_Success_ReturnsTenantView`
- 4.3 — `Get` 不在→CodeNotFound / `TestService_Get`「存在しない...」/ `TestTenantRepository_Get_NotFound`
- 4.4 — `List` 0 件で非 nil 空 slice / `TestService_List`「登録 0 件...」/ `TestList_Empty_Returns200WithEmptyArray`
- 5.1 — `EnterpriseNameForTenant` 未バインド/不在判定 / `TestService_EnterpriseNameForTenant`（pending/不在）
- 5.2 — pending_bind→ErrNotBound(422) / `TestService_EnterpriseNameForTenant`「pending_bind のとき...」
- 5.3 — disabled→ErrTenantDisabled(422) / `TestService_EnterpriseNameForTenant`「disabled のとき...」
- 6.1 — `Handler.Mount(Routers.Admin)` で `/api/admin/tenants` 提供 / `TestMount_RegistersAllFiveEndpoints` / `tenant_admin_guard_test.go`
- 6.2 — tenant-console aud→403（ガード継承） / `TestTenantAdminGuard_TenantConsoleAudience_Returns403`
- 6.3 — 非 SuperAdmin→403 / `TestTenantAdminGuard_NonSuperAdmin_Returns403`
- 6.4 — 未認証→401 / `TestTenantAdminGuard_NoClaims_Returns401`
- 6.5 — 固定 message で存在差非露出 / `TestGet_NotFound_Returns404WithoutExistenceLeak` / `TestTenantRepository_NonSuperAdminContext_TenantIsolation`
- NFR 1.1 — Status enum + Valid/ParseStatus（3 値）/ `TestStatusValid` / `TestParseStatus`
- NFR 1.2 — 未定義遷移を fail-closed 拒否 / `TestService_Bind`「定義外 status のとき fail-closed で 422」
- NFR 1.3 — bind 途中失敗で部分遷移なし / `TestService_Bind`「CreateEnterprise が失敗...」（updateBound==0）
- NFR 2.1 — Event 監査項目 emit / `TestLoggerRecorderEmitsSafeFields`
- NFR 2.2 — 拒否経路の構造化ログ（deny_reason）/ 各 service test の `warnWithDenyReason` 検証
- NFR 2.3 — Event に機密値フィールドを持たせず秘密値非出力 / `TestLoggerRecorderDoesNotEmitSecrets` / service test の `containsSecret`
- NFR 3.1 — tenants テーブルへ永続化 / `TestTenantRepository_CRUD_SuperAdminContext` ほか integration test

## Findings

なし

## Summary

全 numeric AC（Req 1〜6 / NFR 1〜3）が `internal/tenant` の実装と単体・結合テストで観測可能にカバーされ、変更は tasks.md の `_Boundary:_`（tenant.types / EventRecorder / Service / Handler / db.migrations / integration test）に収まる。tasks.md の変更は checkbox 進捗のみ、impl-notes は Developer 補足で spec 本文の書き換えなし。build / vet / tenant 単体テスト green、integration test もコンパイル成功（DB 未設定で self-skip）。boundary 逸脱・missing test なし。

RESULT: approve
