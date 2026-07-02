# Implementation Plan

> 本 Issue（B1 エンロール）は enrollment domain 一式 + notification ENROLLMENT ハンドラ + cmd/api 配線 + 結合テストを
> 6 タスクに分解する（design.md「File Structure Plan」/「Components and Interfaces」と 1:1）。
> `internal/policy` を手本に consumer-defines-interface（primitive 型ポート）で cross-domain import を回避する。
> 各タスクは独立コミット可能。並列可能タスクには `(P)` を付し `_Boundary:_` で担当 Components を明示する。
> cmd/worker は変更しない（design.md リスク 7）。

- [x] 1. Enrollment domain 型・token repository 基盤
  - `internal/enrollment/doc.go`: package doc + 依存方向規約（許可: platform/amapi, platform/db, audit, logger, errors, config, authz/httpserver は handler のみ。禁止: 他 domain / cmd への直接 import。`policy/doc.go` を手本）
  - `internal/enrollment/types.go`: `Mode` enum（`fully_managed` / `dedicated` + `ParseMode`/`Valid`）、DTO（`IssueRequest{Mode, PolicyID *uuid, Duration}` / `TokenView{ID, Mode, ExpiresAt, Value, QRCodeData}` / `TokenSummary{ID, Mode, PolicyID, ExpiresAt, Status}` / `TokenRow`）、`AdditionalData{TenantID, IssuedBy, Mode}` 値オブジェクト + marshal helper、sentinel error（`ErrInvalidMode` / `ErrPolicyRequired` / `ErrTokenPersist`）、port IF（`enterpriseResolver` / `policyChecker` / `eventRecorder`）
  - `internal/enrollment/repository.go`: `TokenRepository`（`Insert` / `List`）の pgxpool 実装。tenant-scoped context のまま `db.BeginTxFunc` で RLS 分離（`policy.Repository` を手本、SuperAdmin 昇格しない）。`Value`/`QRCode` 列は持たず bind しない（NFR 3.1）。DB 失敗は `CodeUnavailable` へ wrap
  - 単体テスト: additionalData の JSON marshal（tenant_id/issued_by/mode を含む / Req 1.3）、`expires_at` からの `Status`（active/expired）派生（Req 4.1）、`TokenRow` に秘密値 field が無いこと（NFR 3.1）を pure helper で検証
  - _Requirements: 1.3, 4.1, NFR 3.1_

- [x] 2. Enrollment Service（トークン発行 + 一覧）
  - `internal/enrollment/service.go`: `Service`（`IssueToken` / `ListTokens`）実装。手順: validateMode（不正/未指定→`ErrInvalidMode` で AMAPI 非呼出 / Req 1.4）→ DEDICATED は `policyChecker.ResolveOwnedPolicy` で自テナント policy 検証（未指定/不在→エラー / Req 1.5）→ `enterpriseResolver.EnterpriseNameForTenant`（bound のみ / Req 2.3 前提）→ additionalData 組立 → `amapi.Client.CreateEnrollmentToken`（`AllowPersonalUsage=PERSONAL_USAGE_DISALLOWED` 固定、DEDICATED は `PolicyName`=amapi policy id / Req 1.1 / 1.2）→ AMAPI エラー時は snapshot 非永続化で伝達（Req 1.6）→ `TokenRepository.Insert`（AMAPI-first、永続化失敗は inconsistency ERROR ログ）→ `eventRecorder.Record`（`enrollment_token_issue`、Detail は `{mode, expires_at, result}` のみ / Req 5.1 / 5.2）→ `TokenView`（Value/QRCode を一度だけ返却）
  - `ListTokens`: `TokenRepository.List` → `TokenSummary`（`expires_at` から status 派生 / Req 4.1）
  - 単体テスト（`amapi.StubClient` + fake ports）: (1) FULLY_MANAGED で AllowPersonalUsage=DISALLOWED + additionalData 検証（Req 1.1 / 1.3）、(2) DEDICATED で PolicyName 設定（Req 1.2）、(3) 不正モード→ErrInvalidMode・AMAPI 非呼出（Req 1.4）、(4) DEDICATED policy 不在→発行せずエラー（Req 1.5）、(5) AMAPI エラー→Insert 非呼出 + failure 監査（Req 1.6 / 5.1）、(6) 監査 Detail / TokenView 以外に Value/QRCode 非混入（Req 5.2 / NFR 3.1）
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 5.1, 5.2, NFR 3.1_
  - _Depends: 1_

- [x] 3. Enrollment Handler + cmd/api 配線
  - `internal/enrollment/handler.go`: `/api/enrollment-tokens` の `POST`（発行）/ `GET`（一覧）。`httpserver.AuthClaimsFromContext` + `authz.AuthorizeAndLog`（`ResourceEnrollment` × `ActionCreate`(POST) / `ActionRead`(GET)、`TargetTenantID=claims.TenantID`）で own-tenant RBAC。deny→403 / claims 不在→401（`policy.Handler` を手本）。Service エラーは `pkgerrors.WriteHTTP` で写像。`chi.Router` 内包で Mount 互換
  - `cmd/api/main.go`: `policySvc` を `buildPolicyHandler` 内部から main レベルへ引き上げ、enrollment の `policyChecker` アダプタ（`policy.Service.Get` で存在検証 + `policyID.String()` を amapi policy id として返す / design「Existing Architecture Analysis」の命名不変条件）と共有。`enrollment.NewTokenRepository` / `NewService`（amapiClient / auditSvc / tenantSvc / policyChecker 再利用）/ `NewHandler` を構築し `routers.API.Mount("/enrollment-tokens", handler)`
  - 単体テスト: (1) Viewer の POST→403（Req 2.2）、(2) TenantAdmin / Operator の POST→200（Req 2.1）、(3) 越境（他テナント policy 指定）→非露出 404 / 自テナント外アクセス不可（Req 2.3）、(4) GET が `expires_at` 由来 status（active/expired）を返す（Req 4.1）、(5) 不正 JSON→400。`cmd/api` main_test で enrollment 配線の型レベル回帰（`buildPolicyHandler` の testability 方針を踏襲）
  - 4.2（使用済み）は列不在のため能動追跡せず、observable「未登録」は notification path が担保する（design リスク 2 / 検証は task 6 が所有）。GET は expired のみ表示する旨をコメントで明示
  - _Requirements: 2.1, 2.2, 2.3, 4.1_
  - _Depends: 2_

- [ ] 4. Enrollment Registrar（devices 冪等 upsert） (P)
  - `internal/enrollment/registration.go`: `Registrar.UpsertEnrolledDevice(ctx, amapiDeviceName, mode, complianceStatus string) error`（`notification.EnrollmentRegistrar` を structural typing で満たす / primitive 型）。ctx の `TenantContext.TenantID` を bind し `INSERT INTO devices (id, tenant_id, amapi_device_name, mode, compliance_status) VALUES (uuid.New(), <tenant>, ...) ON CONFLICT (tenant_id, amapi_device_name) DO UPDATE SET mode=EXCLUDED.mode, compliance_status=EXCLUDED.compliance_status`（冪等 / Req 3.4）。`enrolled_at`/`id` は初回値保持。tenant-scoped RLS で越境更新不可（NFR 2.1）。DB 失敗は `CodeUnavailable`+`IsTransient=true`
  - 単体テスト: mode / compliance_status の bind 値写像（`fully_managed`/`dedicated`、`unknown`/`unsupported`）と ctx 未確立時のガードを pure/mock で検証（実 upsert の冪等性は task 6 の結合テストで固定）
  - _Requirements: 3.1, 3.4, NFR 2.1_
  - _Boundary: EnrollmentRegistrar_
  - _Depends: 1_

- [ ] 5. notification.EnrollmentNotificationHandler (P)
  - `internal/notification/enrollment_handler.go`: `NotificationHandler` 実装 + `EnrollmentRegistrar` port 宣言（consumer-defined / primitive 型 / `TenantResolver` を手本）。`Handle` 手順: `env.Payload` parse（`name`→amapi_device_name / `enrollmentTokenData`→additionalData / `softwareInfo.androidVersion`）→ additionalData parse で `tenant_id`/`mode` 抽出（欠落/parse 不能→退避 / Req 3.3）→ `db.FromContext(ctx).TenantID` と突合（不一致/ctx 未確立→退避 / Req 3.2 / NFR 2.2）→ 一致時 androidVersion<10 なら `unsupported` else `unknown` を決定（Req 3.5 / NFR 1.1）→ `registrar.UpsertEnrolledDevice`（Req 3.1）。退避は `UnassignedQueue.Enqueue(ctx, env)` を自ら呼び nil 返却（design 採用案 / Dispatcher 無改変）。transient 失敗はそのまま返し nack 保持（Req 3.6）。退避理由 / サポート対象外 / 失敗を非機密 field で構造化 WARN（NFR 4.1 / NFR 3.1）
  - 単体テスト（fake registrar + fake `UnassignedQueue`）: (1) 突合一致→Registrar 呼出・Enqueue 非呼出（Req 3.1）、(2) tenant_id 不一致→Enqueue のみ・Registrar 非呼出（Req 3.2 / NFR 2.1 / NFR 2.2）、(3) tenant_id 欠落/additionalData parse 不能→Enqueue（Req 3.3）、(4) androidVersion<10→compliance=`unsupported`（Req 3.5 / NFR 1.1）、(5) Registrar transient 失敗→非 ack error 返却（Req 3.6）、(6) 退避/サポート対象外/失敗の WARN に payload 生値非混入（NFR 4.1 / NFR 3.1）
  - _Requirements: 3.1, 3.2, 3.3, 3.5, 3.6, NFR 1.1, NFR 2.1, NFR 2.2, NFR 4.1_
  - _Boundary: EnrollmentNotificationHandler_

- [ ] 6. エンロールフロー結合テスト（実 PostgreSQL / Req 6）
  - `backend/test/integration/enrollment_flow_test.go`: `notification_dispatch_test.go` の setup 作法（migrate→truncate→app pool→bound tenant seed→`newMessage`）を踏襲し、`notification.NewDispatcher` に本 Issue の ENROLLMENT handler（`enrollment.NewRegistrar(pool)` を registrar に注入）を登録した in-test Dispatcher を組む（cmd/worker は無変更 / design リスク 7）
  - シナリオ: (1) `enrollment.Service.IssueToken` で発行（snapshot 確認）→ additionalData に発行元 tenant_id を載せた模擬 ENROLLMENT 通知を `Dispatcher.Handle` → `devices` に該当 amapi_device_name の行が 1 件登録（Req 6.1 / 3.1）、(2) additionalData.tenant_id を enterprise 由来と不一致にした通知→`unassigned_notifications` に退避・`devices` 無変化（Req 6.2 / 3.2 / NFR 2.2）、(3) 同一 amapi_device_name の通知を 2 回 Handle→`devices` 件数 1 のまま冪等更新（Req 6.3 / 3.4）、(4) 無効（非一致）通知で当該テナントに端末が作られないこと（Req 4.2 の observable「未登録」）
  - `DATABASE_URL` 未設定環境は `requireDBURLs` が `t.Skip`（helpers_test.go 規約）
  - _Requirements: 6.1, 6.2, 6.3, 3.4, 4.2, NFR 2.2_
  - _Depends: 3, 4, 5_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを構造化ブロックで宣言する。
`go test ./...` は実 DB 未接続時に結合テストが `t.Skip` するため単体テストのみ実行される（DB 不在でも false-fail しない）。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go vet ./... && go test ./...
```
