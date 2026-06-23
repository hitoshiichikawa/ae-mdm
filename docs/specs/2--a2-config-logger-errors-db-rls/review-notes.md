# Review Notes

<!-- idd-claude:review round=1 model=claude-sonnet-4-5 timestamp=2026-06-23T00:00:00Z -->

## Reviewed Scope

- Branch: claude/issue-2-impl--a2-config-logger-errors-db-rls
- HEAD commit: 29adca6861a3bea00a40cd1169ecee5e49a04a29
- Compared to: develop..HEAD（41 commits / 65 files changed / +6,474 -119）

## Verified Requirements

### Req 1: Config Loader

- 1.1 — `backend/internal/config/config.go` の `Config` struct と `Load()` / `config_test.TestLoad_AllRequiredPresent_ReturnsConfig`
- 1.2 — `backend/internal/config/env.go` で DB / OIDC×2 / Pub-Sub / AMAPI / SessionSecret / AuditLogRetentionDays / DeviceSyncDelayThresholdHours を含む 13 required env を `requiredStr` で読込（`TestLoad_AllRequiredPresent_ReturnsConfig` の `validEnv` fixture で網羅）
- 1.3 — `env.go` の `buildConfigError` / `TestLoad_MissingRequired_ReturnsConfigInvalid` / `TestLoad_InvalidIntFormat_ReturnsConfigInvalid` / `TestLoad_SessionSecretTooShort_ReturnsConfigInvalid`
- 1.4 — 値型 Config を返す（pointer 共有なし / 公開セッターなし）
- 1.5 — `cmd/api/main.go` と `cmd/worker/main.go` の両方が `internal/config` を import

### Req 2: Logger

- 2.1 — `backend/internal/logger/logger.go` の `NewLogger(cfg)` / `TestNewLogger_AppliesConfigDefaults`
- 2.2 — `TenantID` / `RequestID` / `MessageID` / `ActorID` field helpers / `TestLogger_AddsFieldsHelpers`
- 2.3 — `internal/errors/http_mapping.go` の `emitHTTPLog` が 5xx で ERROR、4xx で WARN を発行 / `worker_mapping.go` の `emitWorkerLog` / `TestWriteHTTP_DomainError` / `TestShouldAck_*`
- 2.4 — `parseLevel` / `newEncoder` / `newWriteSyncer` が cfg から構築 / `TestLogger_LevelThreshold_FiltersBelow` / `TestNewLogger_RejectsInvalidLevel` / `TestNewLogger_RejectsInvalidFormat` / `TestNewLogger_FileOutput`
- 2.5 — `redact.go` の `redactKeySubstrings` allowlist（session_secret / id_token / access_token / refresh_token / cookie / google_application_credentials / sa_json / private_key / password）/ `TestRedactFields_RedactsSecretsBySubstring`

### Req 3: Domain Errors

- 3.1 — `backend/internal/errors/errors.go` の `Error{Code, Message, HTTPStatus, IsTransient, Cause}` / `TestError_ErrorMessage`
- 3.2 — `Unwrap()` 実装 / `TestError_IsAsCompatibility`
- 3.3 — `codes.go` の `defaultHTTPStatus(code)` で 11 Code を 4xx/5xx に写像 / `http_mapping.go` の `WriteHTTP` / `TestDefaultHTTPStatus_Mapping`（12 ケース）
- 3.4 — `worker_mapping.go` の `ShouldAck` が nil / Transient / 非 Transient / 独自型外 error の 4 経路を判定 / `TestShouldAck_*`（4 ケースマトリクス）
- 3.5 — `toDomainError` が独自型外 error を `CodeInternal/IsTransient=true` で wrap し ERROR ログ / `TestWriteHTTP_NonDomainError_DefaultsTo500`

### Req 4: DB Pool + Tenant Context

- 4.1 — `backend/internal/platform/db/pool.go` の `NewPool` が pgxpool + Ping / `TestNewPool_InvalidConnString_ReturnsServiceUnavailable`
- 4.2 — `txmanager.go` の `BeginTxFunc(ctx, pool, fn)` が tx begin / commit / rollback を一元化 / `TestBeginTxFunc_*`（4 経路: 正常 / fn error / commit fail / BeginTx fail）
- 4.3 — `rls.go` の `SetLocalTenant` が `SELECT set_config('app.tenant_id', $1, true)` を発行 / `TestSetLocalTenant_NormalTenant_EmitsSetConfigForTenantID`
- 4.4 — SuperAdmin 時に `app.is_superadmin = 'true'` を併発行（`TenantID==uuid.Nil` の cross-tenant 経路では `app.tenant_id` を set せず default deny に倒れる）/ `TestSetLocalTenant_SuperAdminWithTenantID_EmitsBothSetConfigs` / `TestSetLocalTenant_SuperAdminCrossTenant_OnlyIsSuperAdminEmitted`
- 4.5 — `BeginTxFunc` が ctx に TenantContext なしで panic / `TestBeginTxFunc_TenantContextMissing_Panics` + 実 DB 経由 `integration.TestDBTenantIsolation_NoTenantContext_Panics`
- 4.6 — `backend/internal/platform/db/sqlcgen/.gitkeep` で sqlc 出力配置を確保（既存 `backend/sqlc.yaml` は #1 で配置済み）

### Req 5: HTTP Subrouters

- 5.1 — `httpserver/middleware.go` の `TenantContextMiddleware(log)` / `middleware_test.go`
- 5.2 — `TenantContextMiddleware` が `authClaimsFromContext` → `db.WithTenantContext(ctx, tc)` で TenantID / AdminUserID / Roles / IsSuperAdmin を ctx に格納
- 5.3 — `httpserver/server.go` の `r.Mount("/api/admin", adminRouter)` + `r.Mount("/api", apiRouter)` で 2 サブルータ並列 mount / `integration.TestHTTPSubrouterMount_*`
- 5.4 — `admin_middleware.go` の `RequireSuperAdmin(log)` を adminRouter で `Use` 固定
- 5.5 — `RequireSuperAdmin` が IsSuperAdmin=false で 403 / body にリソース ID を含めない / `TestRequireSuperAdmin_NonSuperAdmin_Returns403` / `integration.TestHTTPSubrouterMount_AdminReturns403_WhenNonSuperAdminContextInjected`
- 5.6 — `Recoverer(log)` + `RequestID()` + `AccessLog(log)` を root chain で `Use` / `middleware_test.TestRecoverer_*` / `TestRequestID_*` / `TestAccessLog_*`

### Req 6: Migrations + RLS

- 6.1 — `backend/db/migrations/0001_create_tenants` 〜 `0010_create_notification_dedupe_and_unassigned` の 10 ペアで 12 テーブル（tenants / admin_users / admin_role_assignments / sessions / enrollment_tokens / policies / devices / device_commands / tenant_apps / audit_logs / notification_dedupe / unassigned_notifications）を作成
- 6.2 — 全マイグレーションに `.up.sql` / `.down.sql` のペアを提供 / `integration.TestMigrationsReversible_DownDropsTablesUpRecreates`
- 6.3 — `0011_enable_rls.up.sql` が tenant_id 列を持つ 7 テーブル + `tenants`（SuperAdmin only）+ `sessions`（admin_users 経由 subselect）+ infra 2 テーブル（SuperAdmin only）に `ENABLE ROW LEVEL SECURITY` + tenant_isolation_* ポリシー / `integration.TestDBTenantIsolation_SuperAdminSeesAllTenants`
- 6.4 — RLS USING / WITH CHECK で `current_setting('app.tenant_id', true)::uuid OR current_setting('app.is_superadmin', true)::boolean` を発行 / `integration.TestDBTenantIsolation_DevicesSelectExcludesOtherTenant` / `TestDBTenantIsolation_DevicesUpdateDeleteOtherTenant_ZeroRows` / `TestDBSessions_TenantIsolation_SubselectPolicy`
- 6.5 — `backend/db/roles/0001_create_app_and_migration_roles.sql` で `migration_user` / `app_user` の 2 ロール定義 + `Makefile` `db-init-roles` target + `.env.example` の DATABASE_URL=app_user / MIGRATE_DATABASE_URL=migration_user 更新

### Req 7: Audit Logs Append-only

- 7.1 — `0012_audit_log_immutability.up.sql` で `FOR INSERT WITH CHECK` ポリシーのみ定義（UPDATE / DELETE 用ポリシー未定義 = 拒否）+ `REVOKE UPDATE, DELETE ON audit_logs FROM app_user`
- 7.2 — `integration.TestDBAuditLogs_AppUserUpdateDelete_Rejected` が SQLSTATE 42501（permission denied）を確認
- 7.3 — `audit_logs_select` ポリシー（`tenant_id = current_setting('app.tenant_id', true)::uuid OR current_setting('app.is_superadmin', true)::boolean`）/ `integration.TestDBAuditLogs_InsertWithCheck_SuperAdminAllowsCrossTenantAndNull`
- 7.4 — `ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY` + `audit_logs_insert` ポリシー WITH CHECK 二重防御 / `integration.TestDBAuditLogs_InsertWithCheck_NormalTenantContext`（テナント A 文脈で tenant_id=B / NULL の INSERT を拒否）

### NFRs

- NFR 1.1 — アプリ層 panic ガード（`BeginTxFunc`）+ PostgreSQL RLS の二重防御を物理化（実装 + 統合テスト）
- NFR 1.2 — `integration.TestDBTenantIsolation_DevicesSelectExcludesOtherTenant` / `TestDBSessions_TenantIsolation_SubselectPolicy` で `tenants` を除く全テーブルの cross-tenant 不可視を確認
- NFR 2.1 — `integration.TestMigrationsReversible_RepeatedUpIsNoop` が `migrate.ErrNoChange` を確認
- NFR 2.2 — `integration.TestMigrationsReversible_DownDropsTablesUpRecreates` が down → up サイクルで 12 テーブルの復活を確認
- NFR 3.1 — `cmd/api/main.go` `runBootstrap` が config / logger / pool / httpserver 各失敗で exit code 1 + 構造化 ERROR / `cmd/worker/main.go` 同パターン
- NFR 3.2 — `db.NewPool` の Ping 成功後に `httpserver.NewServer` → `ListenAndServe` の順序で起動
- NFR 4.1 — `cmd/api` / `cmd/worker` が同じ `internal/config` / `internal/logger` を import（`go build ./...` PASS で間接 verify）
- NFR 4.2 — config は env のみから読込（ファイルシステム永続データなし）

### 実行確認

reviewer 側で再実行:

- `cd backend && GOTOOLCHAIN=local go build ./...`: PASS
- `cd backend && GOTOOLCHAIN=local go vet ./...`: PASS
- `cd backend && GOTOOLCHAIN=local go test ./...`: PASS（全 6 package: config / errors / logger / platform/db / platform/httpserver / test/integration。DB env 未設定環境のため integration test は内部 skip で fail せず）

## Boundary 検証

tasks.md の `_Boundary:_` で許可されたコンポーネント（Config / Logger / Errors / DBPool / TxManager / RLSHelper / TenantContext / HTTPServer / TenantContextMiddleware / AdminRouteGuard / MiddlewareChain / cmd-api / cmd-worker / depspin / Migrations）の範囲内に変更が収まっている。各 commit のスコープは Conventional Commits の `feat(config)` / `feat(logger)` / `feat(errors)` / `feat(db)` / `feat(httpserver)` / `feat(cmd-api)` / `feat(cmd-worker)` / `test(integration)` / `chore(makefile)` / `docs(*)` で適切に分離されており、無関係な領域（frontend / ドメイン handler 等）への変更は含まれていない。

## Feature Flag Protocol

CLAUDE.md の `## Feature Flag Protocol` の `**採否**:` は `opt-out`。flag 観点の細目（旧パス温存 / `if (flag)` 分岐 / flag-off 挙動不変 / flag 命名規約）は本 Issue の判定対象外。

## Findings

なし

## Summary

requirements.md の Req 1〜7 と NFR 1〜4 のすべての numeric ID が、対応する実装ファイル / unit test / integration test で裏付けられている。Feature Flag Protocol は opt-out のため細目チェック対象外。go build / go vet / go test がすべて PASS し、tasks.md の `_Boundary:_` 範囲も逸脱していない。

RESULT: approve
