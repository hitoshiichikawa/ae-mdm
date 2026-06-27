# Implementation Plan

依存順: ドメイン型/監査ポート → マイグレーション → Repository（結合テスト含む） → Service
（Create/参照 → Bind/Disable） → Handler → 認可ガード結合テスト。各実装タスクは対応する
テストを同タスク内に含める（per-task Reviewer 運用での `missing test` reject 回避）。

- [x] 1. ドメイン型と監査記録ポート（`internal/tenant` パッケージ scaffold）
- [x] 1.1 `internal/tenant/types.go` と `internal/tenant/audit_log.go` を追加 (P)
  - `Status` enum（`pending_bind` / `bound` / `disabled`）と 3 値以外を弾く `ParseStatus`/`Valid` を定義（NFR 1.1）
  - `TenantRow`（DB 行）/ `TenantView`（API 応答）/ `CreateInput` / `BindInput` / `DisableInput` / `SignupURL` DTO を定義
  - `EventRecorder` interface（`Record(ctx, Event)`）と `Event{Actor,TenantID,Operation,Result,ConfirmationCompleted,DenyReason}` を定義（NFR 2.1）
  - `audit_log.go`: `EventRecorder` の `internal/logger` 実装（interim binding）。Event のみ構造化出力し、サインアップ URL 秘密値 / SA 資格情報 / OAuth トークンを field に含めない（NFR 2.3）
  - sentinel error（`ErrConflict` / `ErrInvalidState` / `ErrConfirmationRequired` 等）を `*errors.Error` の Code 付きで定義（`internal/auth/service_failure_kinds.go` の慣習に倣う）
  - 単体テスト（in-package）: `Status` の正常/異常値判定、logger EventRecorder が秘密値を出力しないこと（redact 観点）を検証
  - _Requirements: NFR 1.1, NFR 2.1, NFR 2.3_
  - _Boundary: tenant.types, tenant.EventRecorder_

- [ ] 2. マイグレーション 0016（enterprise_name 一意制約 + 無効化監査列）
- [ ] 2.1 `db/migrations/0016_tenants_bind_disable_metadata.{up,down}.sql` を追加 (P)
  - up: `tenants` に `disabled_at timestamptz NULL` / `disabled_by uuid NULL` を `ADD COLUMN IF NOT EXISTS` で追加
  - up: `CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_enterprise_name ON tenants (enterprise_name) WHERE enterprise_name IS NOT NULL`（同一 Enterprise の二重バインド防止 / Req 2.1 invariant 補強）
  - down: 上記 index と 2 列を `DROP ... IF EXISTS` で逆操作（既存 `migrations_reversible_test.go` の up→down 往復検証に乗る）
  - 既存 0001（tenants 本体）/ 0011（RLS）は変更しない（ALTER のみ）
  - _Requirements: 2.1, NFR 1.1, NFR 3.1_
  - _Boundary: db.migrations_

- [ ] 3. Tenant Repository（raw SQL + SuperAdmin ctx + 競合制御）
- [ ] 3.1 `internal/tenant/repository.go` を実装
  - `Repository` interface（`Insert` / `Get` / `List` / `UpdateBound` / `UpdateDisabled`）と pgxpool 実装
  - 全メソッドで `superAdminContext(ctx)` + `db.BeginTxFunc` を使う（`internal/auth/repository.go:81` と同型）
  - `Insert`: status=`pending_bind` で 1 行 INSERT（NFR 1.1 / 3.1）
  - `Get`: `pgx.ErrNoRows` を `CodeNotFound` に写像（Req 4.3）/ `List`: 0 件で空 slice（Req 4.4）
  - `UpdateBound`: `UPDATE ... SET status='bound', enterprise_name=$ WHERE id=$ AND status='pending_bind'` の affected 行数を返す。部分一意 index 違反（pgerrcode 23505）を `CodeConflict` に写像（Req 2.1 / 2.3）
  - `UpdateDisabled`: `UPDATE ... SET status='disabled', disabled_at=now(), disabled_by=$ WHERE id=$ AND status!='disabled'` の affected 行数を返す（Req 3.1）
  - 結合テスト `backend/test/integration/tenant_repository_test.go`（既存 `helpers_test.go` 流用、DB env 未設定なら t.Skip）: SuperAdmin ctx での CRUD（NFR 3.1）/ 非 SuperAdmin ctx で 0 行のテナント分離（Req 6.5 / 1.4 系）/ 同一 id 二重 UpdateBound で 2 回目 affected=0（Req 2.5）/ 別 tenant への同一 enterprise_name で 23505（Req 2.1）/ 二重 UpdateDisabled で 2 回目 affected=0（Req 3.4）
  - _Requirements: 1.1, 2.1, 2.3, 3.1, 3.4, 4.1, 4.2, 4.3, 4.4, 6.5, NFR 1.1, NFR 3.1_
  - _Depends: 1.1, 2.1_

- [ ] 4. Tenant Service: 作成・参照・前提ガード
- [ ] 4.1 `internal/tenant/service.go` に Create / Get / List / EnterpriseNameForTenant を実装
  - `Service` interface 定義 + 本番実装 struct（deps: `Repository` / `amapi.Client` / `EventRecorder` / `config.Config`）
  - `Create`: name 空白 trim 後空なら `CodeInvalidRequest`（Req 1.3）→ `amapi.CreateSignupURL` → `Repository.Insert`(pending_bind) → `signup_url` 返却（Req 1.1 / 1.2）。`CreateSignupURL` が非 transient error なら永続化せずエラー伝達（Req 1.4）。成功/失敗を `EventRecorder.Record`（Req 1.5 / NFR 2.1）。拒否経路（不正入力等）は構造化ログを出力（NFR 2.2）
  - `Get`/`List`: Repository へ委譲し `TenantView` に変換（Req 4.1 / 4.2 / 4.3 / 4.4）
  - `EnterpriseNameForTenant`: bound→enterprise_name + nil、pending_bind→`CodeBusinessRule`（Req 5.2）、disabled→`CodeBusinessRule`（Req 5.3）、不在→`CodeNotFound`（Req 5.1 の未バインド判定可能性を含む）。拒否時は構造化ログ（NFR 2.2）
  - 単体テスト（in-package、`amapi.StubClient` + fake Repository/Recorder）: Req 1.1〜1.5 / 4.1〜4.4 / 5.1〜5.3 の正常・異常・空入力ケース（StubClient.CallCount / fake Repository 呼出記録で副作用検証）+ 拒否経路で構造化ログ field が出ること（NFR 2.2）
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 4.1, 4.2, 4.3, 4.4, 5.1, 5.2, 5.3, NFR 2.1, NFR 2.2_
  - _Boundary: tenant.Service_
  - _Depends: 3.1_

- [ ] 5. Tenant Service: Enterprise バインド・無効化（状態機械）
- [ ] 5.1 `internal/tenant/service.go` に Bind / Disable を追加実装
  - `Bind`: `Repository.Get` で現状態判定 → pending_bind 以外は 409(bound 重複 / Req 2.5) / 422(disabled / Req 2.6) / 404(不在) → pending_bind なら `amapi.CreateEnterprise(signupURLName, cfg.AMAPIProjectID)` → 失敗時は永続化せずエラー伝達 + 行を pending_bind 維持（Req 2.4 / NFR 1.3）→ 成功時 `Repository.UpdateBound`、affected=0 は 409（Req 2.1 / 2.2 / 2.5）。成否を Record（Req 2.7）
  - `Disable`: 二段階確認テキスト（`DisableInput.Confirmation` が対象 `tenants.name` と完全一致）を検証、不一致は `CodeBusinessRule`(確認未完了 / Req 3.2)。確認 OK で `Repository.UpdateDisabled`、affected=0 は 409(二重無効化 / Req 3.4)。disabled は終端で再有効化遷移を持たない。成否を Record（Req 3.1 / 3.3 / 3.5）
  - 未定義状態遷移は前提判定で拒否し `CodeBusinessRule` を返す（NFR 1.2）。全拒否経路（不正遷移 / 確認未完了 / 競合）で構造化ログを出力（NFR 2.2）
  - 単体テスト（in-package）: bind 失敗で bound に進まず `UpdateBound` 未呼出（Req 2.4 / NFR 1.3）/ bound 再 bind で `CreateEnterprise` 未呼出（Req 2.5、CallCount==0）/ disabled へ bind で 422（Req 2.6）/ 確認不一致で 422（Req 3.2）/ 二重無効化で 409（Req 3.4）/ NFR 1.2 未定義遷移拒否 / 拒否経路の構造化ログ field 検証（NFR 2.2）
  - _Requirements: 2.1, 2.2, 2.3, 2.4, 2.5, 2.6, 2.7, 3.1, 3.2, 3.3, 3.4, 3.5, NFR 1.2, NFR 1.3, NFR 2.1, NFR 2.2_
  - _Boundary: tenant.Service_
  - _Depends: 4.1_

- [ ] 6. Tenant Handler（`/api/admin/tenants` 5 endpoint + Mount）
- [ ] 6.1 `internal/tenant/handler.go` を実装
  - `Handler` struct（deps: `Service` / `logger.Logger`）+ `NewHandler` + `Mount(r chi.Router)`（`internal/auth/handler.go:52` と同方式で `Routers.Admin` 配下へ sub-route 登録 / Req 6.1）
  - 5 endpoint: `POST /tenants`（name 空 JSON → 400 / Req 1.3）/ `POST /tenants/{id}/bind` / `DELETE /tenants/{id}`（確認テキスト欠落 → Service で 422 / Req 3.2）/ `GET /tenants`（Req 4.1）/ `GET /tenants/{id}`（Req 4.2 / 4.3）
  - actor_id を `httpserver.AuthClaimsFromContext`（`middleware.go:104`）で取得し Service に渡す
  - error は `errors.WriteHTTP` で写像。404/競合 body は固定 message で対象テナントの存在差を露出しない（Req 6.5）
  - 単体テスト（in-package、httptest + fake Service）: name 空 → 400（Req 1.3）/ 不在 ID GET → 404 で存在露出なし（Req 6.5）/ Create 正常で status=pending_bind + signup_url を含む JSON（Req 1.1 シリアライズ）/ Mount された route が解決されること
  - _Requirements: 1.3, 3.2, 4.1, 4.2, 4.3, 6.1, 6.5_
  - _Boundary: tenant.Handler_
  - _Depends: 5.1_

- [ ] 7. `/api/admin` 認可ガード継承の結合テスト
- [ ] 7.1 `backend/test/integration/` に Tenant エンドポイントのガード継承テストを追加
  - 既存 `http_subrouter_mount_test.go` / admin chain 構築（`server.go` の `NewServer` + `Routers.Admin`）を流用し、`tenant.Handler.Mount` 済みルータを構築
  - 未認証（AuthClaims 不在）で `/api/admin/tenants` 系 → 401（Req 6.4）
  - tenant-console aud の AuthClaims → 403（Req 6.2）
  - admin-console aud + 非 SuperAdmin → 403（Req 6.3）
  - admin-console aud + SuperAdmin → 200/2xx（ガード通過、fake Service で正常応答）
  - _Requirements: 6.2, 6.3, 6.4_
  - _Depends: 6.1_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを宣言する。
`go test ./...` は結合テスト（`test/integration`）を含むが、DB env 未設定環境では各テストが
自身で `t.Skip` するため DB 不在でも false-fail しない（`helpers_test.go` の `requireDBURLs` 規約）。

<!-- stage-a-verify -->
```sh
cd backend && GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go vet ./... && GOTOOLCHAIN=local go test ./...
```
