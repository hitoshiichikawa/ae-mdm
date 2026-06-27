# Implementation Plan

> 本 Issue（#5 / A5）は ae-mdm の **監査ログ Service** を提供する。新規コードは
> `backend/internal/audit/` に集約し、`GET /api/audit-logs`（tenant-console / own-tenant）と
> `GET /api/admin/audit-logs`（admin-console / cross-tenant）の 2 エンドポイントを `cmd/api` 起動時に
> `Routers.API` / `Routers.Admin` へ Mount する。
>
> **外部依存はすべて解決済み**: `audit_logs` テーブル + RLS + INSERT-only ロール + 保持期間 config は
> A2（#2、完了）、auth middleware / `AuthClaims` 注入 / `RequireAdminConsoleAndSuperAdmin` 固定ガードは
> #33（完了）、`authz.Authorizer`（`audit_log read` 許可マトリクス内包）は #37（完了）で実装済み。
> 本 Issue はこれらを **利用・連携**するのみで、migration / RLS / ロール / config の新規作成は **行わない**。
>
> 既存パターンの手本: `backend/internal/auth/repository.go`（BeginTxFunc 経由 raw pgx）、
> `backend/internal/platform/httpserver/admin_middleware.go`（Authorizer 連携 / 構造化拒否ログ）、
> `backend/test/integration/auth_repository_test.go`（実 DB integration test）。
>
> 並列実行可能なタスクには `(P)` を付け、`_Boundary:_` で担当 Components を明示する。

- [x] 1. Audit ドメイン型 + Service（記録 / 閲覧 + 保持期間下限）+ 単体テスト
- [x] 1.1 Audit types + Clock + failure_kinds (P)
  - `backend/internal/audit/types.go` を新規追加。`EventType string` / `ResultType string`
    （`ResultSuccess` / `ResultFailure` の 2 値）/ `Event`(ID / TenantID / ActorID / EventType /
    ResourceID / Detail `map[string]any` / Result / OccurredAt) / `Filter`(TenantID `*uuid.UUID` /
    EventType / ActorID string / ResourceID / From `*time.Time` / To `*time.Time`) を定義。
    `Event.TenantID == uuid.Nil` を NULL テナント（Req 1.2）として扱う旨を godoc に明記
  - `backend/internal/audit/clock.go` を新規追加。`Clock interface { Now() time.Time }` と
    `SystemClock` 実装（手本: `auth.SystemClock`）。Service の保持期間下限算出の DI 境界
  - `backend/internal/audit/failure_kinds.go` を新規追加。構造化ログ用 `failure_kind` 定数
    （`authz_denied` / `parse_invalid` / `persist_error` / `query_error`）。NFR 3.2 の失敗種別識別用
  - `backend/internal/audit/doc.go` を新規追加。`internal/audit` の依存方向（platform/db /
    platform/authz / platform/httpserver / config / logger / errors のみ import 可）を godoc に記載
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 2.2, 2.3, 2.4, 2.5, 3.2_
  - _Boundary: AuditTypes_
- [x] 1.2 Audit Service 実装 + 単体テスト
  - `backend/internal/audit/service.go` を新規追加。`Service interface { Record(ctx, Event) error;
    List(ctx, Filter) ([]Event, error) }`（update/delete IF は **公開しない** / Req 1.5）と
    `NewService(cfg config.Config, repo Repository, clock Clock) Service` を実装
  - `Record`: `Event.ID` 未設定（uuid.Nil）なら `uuid.New()` を採番、`OccurredAt` 未設定（zero）なら
    `clock.Now()` を補完し、`repo.Insert(ctx, ev)` を呼ぶ。INSERT 失敗は error をそのまま伝播して
    成功扱いしない（Req 1.1 / 1.2 / 1.3 / 1.4 / 1.6）。`Detail` は素通しし、機密 sanitize は呼び出し側
    責務である旨を godoc に明記（Req 1.7 / NFR 3.1）
  - `List`: `retentionFloor := clock.Now().AddDate(0, 0, -cfg.AuditLogRetentionDays)` を算出し、
    `effectiveFrom := retentionFloor`。`f.From != nil && f.From.After(retentionFloor)` のとき
    `effectiveFrom = *f.From`（= `max(retentionFloor, from)` / Req 5.1 / 5.2 / 5.3 / 5.4 / NFR 1.1）。
    `repo.Select(ctx, f, effectiveFrom)` を呼んで結果を返す（0 件は空 slice + nil / Req 2.8 / 3.4）
  - `backend/internal/audit/service_test.go` を新規追加（fake Repository + fake Clock / 表駆動）:
    - (a) `Record` が ID/OccurredAt 未設定時に補完し、`ResultSuccess`/`ResultFailure` を Repository へ
      正しく渡す（Req 1.1〜1.4）
    - (b) `Record` で `Event.TenantID == uuid.Nil` がそのまま Repository へ渡る（Req 1.2 / NULL bind は
      Repository 責務）
    - (c) `Record` で fake Repository が error を返したら Service が成功扱いせず error 伝播（Req 1.6）
    - (d) `List` で `from` が retention 起点より前 → `effectiveFrom` が起点に丸められる（Req 5.3）
    - (e) `List` で `from` が retention 起点より後 → `effectiveFrom == from`（Req 5.3 境界）
    - (f) retention 既定 180 と変更値 365 で下限が切り替わる（fake Clock 固定時刻 / Req 5.1 / 5.4 / NFR 1.1）
    - (g) fake Repository が 0 行を返したら空 slice + nil（Req 2.8 / 3.4）
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7, 2.8, 3.4, 5.1, 5.2, 5.3, 5.4, NFR 1.1, NFR 3.1_
  - _Boundary: AuditService_
  - _Depends: 1.1_

- [x] 2. Audit Repository（append-only INSERT + 保持下限付き SELECT / raw pgx）+ 単体テスト
- [x] 2.1 Repository 実装 + 動的 SQL 組み立ての単体テスト
  - `backend/internal/audit/repository.go` を新規追加。`Repository interface { Insert(ctx, Event) error;
    Select(ctx, Filter, effectiveFrom time.Time) ([]Event, error) }`（UPDATE/DELETE メソッドは **持たない**
    / Req 1.5 / 6.1）と `NewRepository(pool *pgxpool.Pool) Repository` を実装（手本: `auth.repository`）
  - `Insert`: `db.BeginTxFunc(ctx, pool, fn)` 経由で `INSERT INTO audit_logs (id, tenant_id, actor_id,
    event_type, resource_id, detail, result, occurred_at) VALUES ($1..$8)` を実行。`Detail` は
    `json.Marshal` して jsonb bind、`TenantID == uuid.Nil` は NULL bind（`*uuid.UUID` の nil 渡し等 /
    Req 1.1 / 1.2）。INSERT 失敗は `*errors.Error{Code: CodeUnavailable}` で wrap
    （Cause に DB err / message 本文に detail 生値を補間しない / Req 1.6 / NFR 3.1）
  - `Select`: 動的 WHERE 句を `$n` プレースホルダで組み立てる（design.md「Audit Repository」Invariants
    と整合）。`occurred_at >= effectiveFrom` を **常に** 付与（Req 5.2 / 5.3）、`To != nil` で
    `occurred_at <= $n`（Req 2.7）、`EventType != ""` で `event_type = $n`（Req 2.2 / 3.3）、
    `ActorID != ""` で `actor_id = $n`（Req 2.3 / 3.3）、`ResourceID != ""` で `resource_id = $n`
    （Req 2.4 / 3.3）、`TenantID != nil` で `tenant_id = $n`（admin 横断のみ / Req 3.2）。
    **自テナント条件は書かず RLS に委ねる**（Req 2.6 / 2.9 / 3.5 / NFR 2.1）。`ORDER BY occurred_at DESC`
    （Req 2.1 / 3.1）。SELECT/scan 失敗は `*errors.Error{Code: CodeUnavailable}` で wrap（NFR 3.2）
  - `backend/internal/audit/repository_test.go` を新規追加。実 DB に依存しない範囲で **WHERE 句組み立て
    ロジックを純粋関数として切り出して単体テスト**する（例: `buildSelectQuery(f, effectiveFrom)
    (sql string, args []any)` を package-private で実装し、(a) Filter 全空で `occurred_at >= $1` +
    `ORDER BY occurred_at DESC` のみ、(b) 各 Filter 句が指定時のみ追加され args 順序が一致、(c)
    `TenantID != nil` で `tenant_id` 句が入り `TenantID == nil` で入らない、を検証）。実 DB を要する
    INSERT/SELECT/RLS の挙動（NULL bind / 自テナント分離 / cross-tenant 可視）は task 6 の integration
    test で検証する（本 task の `Insert` 実 SQL と RLS 連携は task 6 の実 DB 実行で回帰確認される）
  - 注: `_Requirements:_` のうち 1.1 / 1.2（DB への実 INSERT・NULL bind）と 3.5（RLS による cross-tenant
    可視）は **実 DB でしか検証できない**ため、対応テストは task 6 へ deferred し下記 partial で明示する
    （`_Requirements_partial:_` は `_Requirements:_` の subset）
  - _Requirements: 1.1, 1.2, 1.6, 2.1, 2.2, 2.3, 2.4, 2.5, 2.6, 2.7, 3.1, 3.2, 3.3, 3.5, 5.2, 5.3, NFR 3.1_
  - _Requirements_partial: 1.1, 1.2, 3.5_
  - _Boundary: AuditRepository_
  - _Depends: 1.1_

- [ ] 3. tenant-console Handler（own-tenant 閲覧 + Authorizer 連携）+ 単体テスト
- [ ] 3.1 Handler 実装 + httptest 単体テスト
  - `backend/internal/audit/handler.go` を新規追加。`Handler`（chi.Router 互換 / `ServeHTTP`）と
    `NewHandler(svc Service, authorizer *authz.Authorizer, log logger.Logger) *Handler` を実装。
    `cmd/api` が `routers.API.Mount("/audit-logs", handler)` で配線する（実 path `/api/audit-logs`）
  - 処理順（design.md「Audit Handler」と整合）:
    1. `httpserver.AuthClaimsFromContext(ctx)` で claims 取得（不在は TenantContextMiddleware が先行
       401 のため通常到達しないが、防御的に 401 / Req 4.2）
    2. `authorizer.AuthorizeAndLog(ctx, log, authz.LogContext{RequestID, ActorID,
       SessionHashPrefix}, authz.Request{Roles: claims.Roles, SessionTenantID: claims.TenantID,
       Audience: authz.AudienceTenantConsole, Action: authz.ActionRead, Resource:
       authz.ResourceAuditLog, TargetTenantID: claims.TenantID.String()})` を呼び、deny なら 403
       （Operator/Viewer/権限不足 / Req 4.1 / 4.5 / 4.6）
    3. query parse（`event_type` / `resource_id` はそのまま / `actor_id` は uuid parse / `from` / `to` は
       RFC3339 parse。不正は `*errors.Error{Code: CodeInvalidRequest}` で 400 + `failure_kind=parse_invalid`
       / Req 2.8 と区別 = 不正入力は 400・0 件は 200）
    4. `Filter` を構築（**`TenantID` は設定しない** = own-tenant 固定 / RLS が分離）し
       `svc.List(ctx, filter)` を呼ぶ（ctx は TenantContextMiddleware 確立の tenant 文脈のまま伝播 /
       Req 2.1〜2.9）
    5. `[]AuditLogDTO` に写像して JSON encode（空は `[]` で 200 / Req 2.8）。DTO は id / tenant_id
       (nullable) / actor_id / event_type / resource_id / detail / result / occurred_at(RFC3339)。
       追加の機密値を載せない（NFR 3.1）
  - 失敗パスで `failure_kind` を含む構造化 WARN を出す（NFR 3.2 / 機密値・query 生値は補間しない / NFR 3.1）
  - `backend/internal/audit/handler_test.go` を新規追加（httptest + fake Service + 実 `authz.New()` /
    `httpserver.WithAuthClaims` で claims 注入 / 手本: admin_middleware_test 系）:
    - (a) TenantAdmin claims（Roles=["TenantAdmin"]）で `svc.List` が own-tenant Filter（TenantID 未設定）
      で呼ばれ 200 + JSON 配列（Req 2.1 / 4.6）
    - (b) Operator claims で 403（Req 4.1）
    - (c) Viewer claims で 403（Req 4.5）
    - (d) claims 不在で 401（Req 4.2）
    - (e) `event_type` / `actor_id` / `resource_id` / `from` / `to` が `Filter` に正しく写像される
      （fake Service の受信 Filter を assert / Req 2.2〜2.5）
    - (f) `from` のみ / `to` のみ指定が Filter に反映される（Req 2.6 / 2.7）
    - (g) `from`/`to` 非 RFC3339・`actor_id` 非 uuid で 400（`failure_kind=parse_invalid` / Req 2.8 と区別）
    - (h) fake Service が空 slice を返したら 200 + `[]`（Req 2.8）
  - _Requirements: 2.1, 2.2, 2.3, 2.4, 2.5, 2.6, 2.7, 2.8, 4.1, 4.2, 4.5, 4.6, NFR 3.1, NFR 3.2_
  - _Boundary: AuditHandler_
  - _Depends: 1.2_

- [ ] 4. admin-console Handler（cross-tenant 閲覧）+ 単体テスト
- [ ] 4.1 AdminHandler 実装 + httptest 単体テスト
  - `backend/internal/audit/admin_handler.go` を新規追加。`AdminHandler`（chi.Router 互換）と
    `NewAdminHandler(svc Service, authorizer *authz.Authorizer, log logger.Logger) *AdminHandler` を実装。
    `cmd/api` が `routers.Admin.Mount("/audit-logs", adminHandler)` で配線する（実 path
    `/api/admin/audit-logs` / 固定ガード `RequireAdminConsoleAndSuperAdmin` 配下）
  - 処理順（design.md「Audit Admin Handler」と整合）:
    1. `AuthClaimsFromContext` で claims 取得（固定ガードが admin aud + SuperAdmin を既に強制済み /
       Req 4.4。防御的に claims 不在は 401）
    2. query parse（`tenant_id` は **任意** の uuid parse / 共通絞り込み `event_type` / `actor_id` (uuid) /
       `resource_id` / `from` / `to` (RFC3339) / 不正は 400 + `failure_kind=parse_invalid`）
    3. `authorizer.AuthorizeAndLog(... Audience: authz.AudienceAdminConsole, Action: ActionRead,
       Resource: ResourceAuditLog, SessionTenantID: claims.TenantID, TargetTenantID: <tenant_id query が
       あればその値 / 無ければ claims.TenantID.String()>)` で cross-tenant read を判定（二重防御 /
       Req 4.4 / 4.6。SuperAdmin 以外はそもそも固定ガードで 403）
    4. **SuperAdmin TenantContext を確立**: `ctx = db.WithTenantContext(r.Context(),
       db.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true})`（RLS の is_superadmin 句で全テナント +
       NULL 可視 / Req 3.1 / 3.5）
    5. `Filter` 構築（`tenant_id` query 指定時のみ `Filter.TenantID` を設定 / Req 3.2 / 3.5）→
       `svc.List(ctx, filter)`（Req 3.1 / 3.3）
    6. `[]AuditLogDTO` JSON encode（空は `[]` で 200 / Req 3.4）。NFR 3.1 を守る
  - 失敗パスで `failure_kind` 構造化 WARN（NFR 3.2 / 機密値非補間 / NFR 3.1）
  - `backend/internal/audit/admin_handler_test.go` を新規追加（httptest + fake Service + 実 `authz.New()` /
    SuperAdmin claims 注入）:
    - (a) SuperAdmin claims（Console=admin-console / IsSuperAdmin=true）で `svc.List` が呼ばれ 200 +
      JSON 配列。fake Service が受け取る ctx が SuperAdmin TenantContext（IsSuperAdmin=true）である
      ことを assert（Req 3.1 / 3.5）
    - (b) `tenant_id` query 指定で `Filter.TenantID` が当該 uuid、無指定で nil（Req 3.2 / 3.5）
    - (c) `event_type` / `actor_id` / `resource_id` / `from` / `to` が Filter に写像（Req 3.3）
    - (d) `tenant_id`・`actor_id` 非 uuid / `from`・`to` 非 RFC3339 で 400（`failure_kind=parse_invalid`）
    - (e) fake Service が空 slice を返したら 200 + `[]`（Req 3.4）
  - _Requirements: 3.1, 3.2, 3.3, 3.4, 3.5, 4.4, 4.6, NFR 3.1, NFR 3.2_
  - _Boundary: AuditAdminHandler_
  - _Depends: 1.2_

- [ ] 5. cmd/api への DI 配線 + Mount
- [ ] 5.1 audit Service/Handler/AdminHandler の構築と Routers への Mount
  - `backend/cmd/api/main.go` を修正。`httpserver.NewServer(...)` の戻り値を `srv, routers, err`
    （現状 `srv, _, err` で破棄）に変更し、`routers` を受け取る
  - bootstrap の (6) http server 構築後に audit ドメインを配線:
    - `auditRepo := audit.NewRepository(pool)`
    - `auditSvc := audit.NewService(cfg, auditRepo, audit.SystemClock{})`
    - `authorizer := authz.New()`
    - `auditHandler := audit.NewHandler(auditSvc, authorizer, log)`
    - `auditAdminHandler := audit.NewAdminHandler(auditSvc, authorizer, log)`
    - `routers.API.Mount("/audit-logs", auditHandler)`
    - `routers.Admin.Mount("/audit-logs", auditAdminHandler)`
  - `cfg` / `pool` / `log` は既存 bootstrap で構築済みのものを再利用する（新規構築しない）
  - **behavior-changing regression を同 task で担保**: Mount により `/api/audit-logs` が認証なしで
    401（TenantContextMiddleware default deny）、`/api/admin/audit-logs` が認証なしで 401 を返すこと、
    handler 未 mount 時の 404 catch-all とは別 route が成立することの最小回帰は task 6 の
    `backend/test/integration/audit_test.go` の routing スモークケース（後述 (i)）で観測する設計とする
    （本 task は wiring であり、wiring 起因の 401/403 ガード回帰は test server を要するため task 6 に
    集約 / task 6 が本 task の `_Requirements_partial:_` を解消する）。本 task 単体では `go build ./...` /
    `go vet ./...` が通ること（コンパイル整合）を最低限の検証とする
  - _Requirements: 4.2, 4.4_
  - _Requirements_partial: 4.2, 4.4_
  - _Boundary: APIWiring_
  - _Depends: 3.1, 4.1_

- [ ] 6. 結合テスト（実 DB + RLS / append-only / 保持下限 / 空結果）
- [ ] 6.1 audit integration test
  - `backend/test/integration/audit_test.go` を新規追加（手本: `auth_repository_test.go` / `helpers_test.go`
    の `requireDBURLs` / `applyMigrationsUp` / `truncateAll` / `newAppPool` / `seedTenant` /
    `seedAdminUser`。DATABASE_URL 未設定で `t.Skip`）。実 `audit.NewRepository(pool)` /
    `audit.NewService(cfg, repo, fixedClock)` を用い、TenantContext を `db.WithTenantContext` で確立して
    検証する。シナリオ:
    - (a) tenant 文脈（`TenantID=<A>, IsSuperAdmin=false`）で `Insert` → `Select`：A 行のみ返り、別テナント
      B 行・NULL テナント行が **不可視**（RLS `audit_logs_select`。TenantAdmin が他テナント監査ログを
      閲覧できず存在自体も露出しない = データ層での Req 4.3 担保 / Req 2.9 / 4.3 / NFR 2.1）
    - (b) SuperAdmin 文脈（`TenantID=uuid.Nil, IsSuperAdmin=true`）で `Select`：A/B + NULL 行が
      occurred_at 降順で全件返る（Req 3.1 / 3.5）
    - (c) SuperAdmin 文脈で `Filter.TenantID=<A>` 指定 → A 行のみ（Req 3.2）
    - (d) append-only 検証：tenant / SuperAdmin いずれの文脈でも直接 `UPDATE audit_logs ...` /
      `DELETE FROM audit_logs ...` が拒否される（RLS policy 不在 + app_user REVOKE / Req 6.1 / 6.2 / 6.3）
    - (e) SuperAdmin 文脈で NULL テナント行を `Insert` 後、当該行への UPDATE/DELETE が拒否（Req 6.4）
    - (f) 保持期間下限：retention 起点より前の `occurred_at` を持つ行を seed し、`Service.List` が当該行を
      除外する（fixedClock で `effectiveFrom` を決定的に / Req 5.2 / 5.3）
    - (g) 通常テナント文脈（`TenantID=<A>, IsSuperAdmin=false`）で `tenant_id=<B>` の行を `Insert` しようと
      すると `audit_logs_insert` WITH CHECK で拒否される（NFR 2.2 / 機密混入拒否）。同シナリオで保持期間内
      レコードが欠損なく取得できることも併せて確認（NFR 1.2）
    - (h) 絞り込み一致 0 件で `Service.List` が空 slice + nil（Req 2.8 / 3.4）
    - (i) **routing スモーク（task 5 wiring の回帰）**: `httpserver.NewServer` + audit handler の Mount を
      組んだ test server（または最小 router）で `/api/audit-logs` / `/api/admin/audit-logs` が認証なし
      アクセス時に 401 を返すことを確認（Req 4.2 / 4.4 の wiring 起因回帰 / task 5 の
      `_Requirements_partial:_` を解消）。固定ガード経路の SuperAdmin 強制は #37 既存 test がカバー
      するため、本ケースは「audit handler が Mount された後も 401/403 ガードが先行する」回帰に限定する
  - 本 integration test は dedicated integration/regression test task であり、task 2 の
    `_Requirements_partial:_`（1.1 / 1.2 / 3.5 = 実 DB INSERT・NULL bind・cross-tenant 可視）と task 5 の
    `_Requirements_partial:_`（4.2 / 4.4 = wiring 起因の 401/403 ガード回帰）を解消する（partial 解消関係）
  - _Requirements: 1.1, 1.2, 2.8, 2.9, 3.1, 3.2, 3.4, 3.5, 4.2, 4.3, 4.4, 5.2, 5.3, 6.1, 6.2, 6.3, 6.4, NFR 1.2, NFR 2.1, NFR 2.2_
  - _Boundary: AuditRepository, APIWiring_
  - _Depends: 2.1, 5.1_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを構造化ブロックで宣言する。
`backend/internal/audit/*` の単体テストおよびコンパイル整合は `go build` / `go vet` / `go test ./...` で
担保される。integration test（`backend/test/integration/audit_test.go`）は DATABASE_URL 未設定環境で
`t.Skip` する設計のため、DB 不在環境でも verify は false-fail しない（手本: #33 spec）。

**DB-backed verify は本 Issue の必須補完工程**: 本 Issue の主要リスク（RLS による tenant 分離 /
append-only の UPDATE/DELETE 拒否 / 保持期間下限 / `audit_logs_insert` WITH CHECK）は **DB 接続が
確立している環境でしか検証できず**、`t.Skip` 経路では検出できない。Developer は Stage A 完了前に
`docker compose up -d postgres && make db-init-roles && cd backend && make migrate-up &&
DATABASE_URL="<test_dsn>" go test ./test/integration/... -count=1` 等で DB-backed 検証を **必ず** 実施し、
結果（`t.Skip` 個数を含む）を `impl-notes.md` の「DB-backed verify 実行結果」節に記録する責務を負う。
Reviewer は同節の有無と内容で DB-backed verify 実施を確認する。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go vet ./... && go test ./...
```
