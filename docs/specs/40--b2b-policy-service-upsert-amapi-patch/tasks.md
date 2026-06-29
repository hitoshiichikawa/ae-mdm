# Implementation Plan

> 既存の Validator (#36) / AMAPI Client (#34) / Audit Service (#5) / tenant.Service (#38) /
> authz (#37) は **呼び出すのみ**で変更しない。新規ファイルは `backend/internal/policy/` 配下に
> 追加し、既存 `types.go` / `validator.go`（Validator 占有）には触れない（命名衝突回避）。
> 新規マイグレーション / sqlc query は追加しない（design 推奨案）。

- [x] 1. application 層の型定義と Raw JSON ↔ PolicyInput 変換層を追加
- [x] 1.1 application 層 DTO / sentinel error を `service_types.go` に定義
  - `PolicyRow`（DB 行）/ `PolicyView` / `PolicySummary`（JSON tag 付き）/ `PolicyRequest{name, body map[string]any}` / `AssignInput{device_id}` を定義
  - `Operation`（create/update/delete/assign）/ `Result`（success/failure）enum と sentinel error（`ErrPolicyNotFound`=404 汎用 message / `ErrDeleteConflict`=409）を定義（Req 4.5）
  - 既存 `types.go`（Validator 占有）を変更しないこと
  - 型定義の zero-value / JSON tag を検証する単体テストを `service_types.go` 近傍に追加
  - _Requirements: 4.5_
  - _Boundary: policy.Service, policy.Repository, policy.Handler_
- [x] 1.2 Raw JSON ↔ PolicyInput 変換を `mapper.go` + `mapper_test.go` に実装
  - `RawToPolicyInput(raw map[string]any) (policy.PolicyInput, []ValidationError)` を実装。5 領域（applications 件数 / passwordMinimumLength / encryptionPolicy・passwordQuality / systemUpdate / kiosk）を `PolicyInput` の各 struct に写像
  - 型不整合・必須キー欠落で変換不能なケースを invalid field 相当の `ValidationError` に写像（Req 2.3 / design 確認事項 4 推奨案）
  - 検証通過後に `amapi.PolicyBody{Name, Raw}` を pass-through で組み立てる helper を実装（`amapi/policies.go` の ForceSendFields 機構に依拠 / NFR 1.1）
  - 正常変換 / 型不整合→invalid field / 空 body の単体テストを `mapper_test.go` に追加（同 task 内テスト必須: failure path / Req 2.3）
  - _Requirements: 2.3, 2.1, NFR 1.1_
  - _Boundary: policy mapper_

- [x] 2. policies CRUD + 端末割当の Repository を実装
- [x] 2.1 `repository.go` + `repository_test.go` を実装
  - `db.BeginTxFunc(ctx, pool, ...)` + raw pgx で `Insert` / `Update` / `Get` / `List` / `Delete` / `AssignPolicyToDevice` を実装（tenant-scoped context のまま RLS に分離を委ねる / SuperAdmin 昇格しない）
  - `Get` / 割当 lookup の 0 行は `CodeNotFound`（汎用 message / Req 4.4 / 4.5）。RLS により他テナント行は SELECT 0 行（Req 4.2 / 4.4）
  - `AssignPolicyToDevice` は `WHERE id=$deviceID AND tenant_id=$tenantID` で `applied_policy_id` を UPDATE。複合 FK 違反（他テナント policy）/ affected=0（他テナント device）を Service が NotFound 写像できる戻り値（affected / error）にする（Req 4.3 / 3.3）
  - `Delete` は割当済み端末ありの FK 違反（pgerrcode 23503 等）を `CodeConflict`（409）に写像（design 確認事項 3 推奨案 / Req 5.3）
  - 列ごと型付き scan（NFR 2.1）。`body jsonb` は `map[string]any` へ scan
  - fake pool（`tenant/repository_test.go` 方式）で Insert/Update affected / NotFound 写像 / FK 違反→Conflict 写像の単体テストを追加（同 task 内テスト必須: stale data safety / failure path / Req 4.2 / 4.3 / 5.3）
  - _Requirements: 1.3, 1.5, 3.1, 3.2, 3.3, 4.2, 4.3, 4.4, 5.3, NFR 2.1_
  - _Boundary: policy.Repository_
  - _Depends: 1.1_

- [ ] 3. ポリシー upsert ユースケース（検証→AMAPI→snapshot→監査）を Service に実装
- [x] 3.1 `service.go` の `Create` / `Update` を実装 + `service_test.go`
  - 順序: mapper で Raw→PolicyInput → `policy.Validate` 検証ゲート → 検証失敗なら AMAPI/Repository を呼ばず早期 return（Req 2.5）→ `amapi.UpsertPolicy`（#34）→ 成功後に Repository へ snapshot 永続化（Req 1.3 / 1.5）→ `audit.Service.Record`（成否とも / Req 5.1 / 5.2）
  - AMAPI 再試行不可エラーは snapshot を確定保存せずそのまま伝達（Req 1.4）。検証エラーは `ValidationError.Kind.Code()` で 400/422 に写像し全件提示（Req 2.2 / 2.3 / 2.4）
  - enterprise_name は `tenant.Service.EnterpriseNameForTenant`（#38）で解決（tenant-scoped context）
  - 監査 Detail / 構造化ログに raw body 機密値を載せない（Req 5.4 / NFR 3.2）。拒否経路は `logDeny` で原因属性を構造化ログ（NFR 3.1）
  - fake repo / fake amapi / fake audit で「検証失敗時に AMAPI/Repo を呼ばない」「AMAPI 失敗時に snapshot 永続化しない」「成功時に snapshot+audit」「3000 件超→422 全件」の単体テストを追加（同 task 内テスト必須: failure path / safety-side fallback / Req 1.4 / 2.5）
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 2.1, 2.2, 2.3, 2.4, 2.5, 5.1, 5.2, 5.4, NFR 1.1, 2.2, 3.1, 3.2_
  - _Boundary: policy.Service_
  - _Depends: 1.2, 2.1_

- [ ] 4. ポリシー参照・削除・端末割当ユースケースを Service に実装
- [ ] 4.1 `service.go` の `Get` / `List` / `Delete` / `Assign` を実装 + `service_test.go` 追記
  - `Get` / `List` は tenant-scoped Repository へ委譲し自テナント行のみ返す。不在は NotFound（Req 4.4 / 4.5）
  - `Delete` は Repository.Delete 委譲。割当済み端末ありの Conflict（409）を伝達し、削除イベントを監査（Req 5.3）。確認事項 3 の推奨案（409 拒否）に従う
  - `Assign` は Repository.AssignPolicyToDevice 委譲。自テナント不在 policy（複合 FK 違反）/ device（affected=0）を NotFound に写像（Req 3.1 / 3.2 / 3.3 / 4.2 / 4.3）。割当は DB 更新までで AMAPI device patch は行わない（design 確認事項 1 推奨案）
  - 拒否経路は構造化ログ（NFR 3.1）。監査 Detail に機密値を載せない（Req 5.4）
  - fake repo / fake audit で「自テナント不在 policy/device→NotFound」「削除 Conflict→409+監査」「割当成功→applied_policy_id 確定+監査」の単体テストを追加（同 task 内テスト必須: failure path / Req 3.2 / 3.3 / 5.3）
  - _Requirements: 3.1, 3.2, 3.3, 3.4, 4.2, 4.3, 4.4, 4.5, 5.3, 5.4, NFR 3.1_
  - _Boundary: policy.Service_
  - _Depends: 2.1, 3.1_

- [ ] 5. `/api/policies` Handler（RBAC + HTTP I/O）を実装
- [ ] 5.1 `handler.go` + `handler_test.go` を実装
  - `Mount(r chi.Router)` で `GET/POST /policies`・`GET/PUT/DELETE /policies/{id}`・`PUT /policies/{id}/assign` を sub-route 登録（`audit/handler.go` が手本）
  - 各 endpoint で `authz.Authorizer.AuthorizeAndLog`（`ResourcePolicy` × `ActionCreate/Update/Delete/Read`）を呼び deny は 403/401 + 構造化 WARN（Req 4.1 / NFR 3.1）
  - JSON decode / path param parse は `tenant/handler.go` の `decodeJSON` / `parseID` 同方式。`*errors.Error` は `errors.WriteHTTP` で写像し存在差を露出しない（Req 4.5）。検証エラー 400（invalid field）/ 422（business rule）を分けて返す
  - actor / tenantID は `httpserver.AuthClaimsFromContext` で取得（`httpserver` import は Handler のみ / Service へは引数で渡す）
  - Viewer の POST→403 / TenantAdmin の POST→200 / 不在 GET→404 の HTTP 単体テストを追加（同 task 内テスト必須: authz failure path / Req 4.1）
  - _Requirements: 4.1, 4.5, 2.2, 2.3, NFR 3.1_
  - _Boundary: policy.Handler_
  - _Depends: 3.1, 4.1_

- [ ] 6. DI 配線（main.go）と doc.go 追記
- [ ] 6.1 `cmd/api/main.go` に Policy domain を配線 + `doc.go` 追記
  - `policy.NewRepository(pool)` / `policy.NewService(repo, amapiClient, auditSvc, authorizer, tenantSvc, log)` / `policy.NewHandler(...)` を構築し `routers.API.Mount("/policies", policyHandler)`（既存 amapiClient / auditSvc / authorizer / tenantSvc を再利用 / 新規構築しない）
  - `doc.go` に application 層（Service/Repository/Handler/mapper）の構成と依存方向（amapi/audit/tenant/authz import 可、cmd 不可）を追記。Validator 純粋性契約節は変更しない
  - `main_test.go` に Policy 配線が interim へ退行していないことの型レベル回帰テストを追加（`buildTenantRecorder` testability 方針に倣う）
  - _Requirements: NFR 2.2, 5.1_
  - _Boundary: cmd/api main wiring, policy.doc_
  - _Depends: 5.1_

- [ ]* 7. 結合・E2E テストの補完
  - tenant-scoped context で List/Get が自テナント行のみ返す結合テスト（RLS / Req 4.4）
  - 他テナント policy/device の割当指定→NotFound 結合テスト（Req 4.2 / 4.3）
  - POST→GET→PUT→DELETE のゴールデンパス E2E、連続更新で snapshot が最新反映済みと一致（Req 1.5）
  - 先行 task で同 task 内単体テスト済みの AC を重複させず、E2E / 統合のスコープに限定する
  - _Requirements: 1.5, 4.2, 4.3, 4.4_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを宣言する。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go test ./internal/policy/... ./cmd/api/...
```
