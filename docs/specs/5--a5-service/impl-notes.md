# 実装ノート（A5 監査ログ Service / Issue #5）

## Implementation Notes

### Task 1

- **採用方針**: `internal/audit` パッケージを auth ドメインと同パターンで新設し、純粋な
  ドメインロジック（記録時の ID/OccurredAt 補完・閲覧時の保持期間下限算出）を Service に集約。
  外部副作用（DB / 現在時刻）は Repository / Clock の DI 境界に逃がし、fake で表駆動検証した。
- **重要な判断**:
  - `Service.List` の保持期間下限を `effectiveFrom = max(retentionFloor, from)` として算出。
    `from.After(retentionFloor)` のときのみ from を採用し、nil / 起点以前は retentionFloor に
    丸める（Req 5.3）。冗長なテナント条件・実 SQL は Repository（task 2）に委ね、Service は
    ctx を素通しして RLS に分離を委ねる設計（design.md L309-310）に従った。
  - `Repository` interface を **service.go 内**（consumer-defines-interface イディオム）で宣言
    した。理由は下記「確認事項」を参照。
  - 機密 sanitize は呼び出し側責務（Req 1.7 / NFR 3.1）であることを `Event.Detail` / `doc.go` /
    `Service.Record` の godoc に明記。Service は Detail を素通しする。
- **残存課題（次 task への影響）**: task 2.1（repository.go）の実装者は `Repository` interface を
  **再宣言せず**、本 service.go の interface を満たす concrete struct + `NewRepository(pool) Repository`
  のみを実装すること（下記「確認事項」に詳述）。それ以外の残存課題はなし。

### Task 2

- **採用方針**: `repository.go` に concrete `repository` struct と `NewRepository(pool) Repository`
  を実装。`Repository` interface は service.go 宣言済みのため再宣言せず（task 1 申し送り遵守）、
  WHERE 句組み立てを純粋関数 `buildSelectQuery(f, effectiveFrom) (sql, args)` に切り出して
  実 DB 非依存で単体テスト可能にした。
- **重要な判断**:
  - Detail nil は空 jsonb `{}`（`marshalDetail`）として bind。既存スキーマ
    `detail jsonb NOT NULL DEFAULT '{}'`（`db/migrations/0009_create_audit_logs.up.sql:18`）と
    整合させ NULL を渡さない（Req 1.1）。`TenantID == uuid.Nil` は `nullableTenantID` で
    `*uuid.UUID` nil（NULL bind）に写像（手本: `auth/repository.go` の `*uuid.UUID` scan/bind / Req 1.2）。
  - `buildSelectQuery` は `occurred_at >= $1`（effectiveFrom）を常に先頭付与し、To/EventType/
    ActorID/ResourceID/TenantID 句を指定時のみ `$n` で追記。自テナント条件は書かず RLS に委ねる
    （Req 2.6/2.9/3.5/NFR 2.1）。INSERT/SELECT/scan 失敗は `CodeUnavailable` で wrap し
    機密値・query 生値を message に補間しない（Req 1.6/NFR 3.1/3.2）。
- **残存課題（次 task への影響）**: 実 DB を要する INSERT・NULL bind・cross-tenant 可視
  （Req 1.1/1.2/3.5）は `_Requirements_partial:_` 明示済みで task 6（integration）へ deferred。
  task 3/4（handler）は本 Repository ではなく Service interface に依存するため本 task の影響なし。

### Task 3

- **採用方針**: `handler.go` に tenant-console `Handler`（chi.Router 内包で http.Handler /
  chi.Router 双方を満たす）と `NewHandler(svc, authorizer, log)` を実装。query parse を純粋関数
  `parseFilter` / `parseRFC3339Query` に切り出し、両 handler 共通の `AuditLogDTO` /
  `toAuditLogDTO` / `writeAuditLogs` を handler.go に置いた（task 4 admin handler が再利用）。
- **重要な判断**:
  - `Handler` は chi.Router を内包し root 相対 `GET /` を登録。本番は
    `routers.API.Mount("/audit-logs", h)` で chi が prefix を strip するため `/api/audit-logs`
    で成立する。handler_test も本番同様に親 chi router へ Mount してから request する
    （直接 ServeHTTP に `/api/audit-logs` を渡すと route 不一致で 404 になるため。test 側の
    弱体化ではなく production 経路の忠実再現）。
  - own-tenant 固定のため `Filter.TenantID = nil` を明示し RLS に分離を委ねる（Req 2.9 /
    4.3 / NFR 2.1）。`AuthorizeAndLog` を `Audience=AudienceTenantConsole` /
    `TargetTenantID=claims.TenantID.String()`（same-tenant）で呼ぶことで Operator/Viewer は
    matrix で deny → 403、TenantAdmin/SuperAdmin は allow（実 `authz.New()` で検証）。
  - DTO の `tenant_id` は NULL テナント（uuid.Nil）を JSON `null` にするため `*string`。0 件は
    `make([]AuditLogDTO, 0)` を encode して `[]`（`null` ではなく空配列）で 200（Req 2.8）。
    parse 失敗は 400 + `failure_kind=parse_invalid`、DB 失敗は `svc.List` の `CodeUnavailable`
    →503 で `failure_kind=query_error` を WARN（query 生値は補間しない / NFR 3.1 / 3.2）。
- **残存課題（次 task=4/5 への影響）**: `AuditLogDTO` / `toAuditLogDTO` / `writeAuditLogs` /
  `parseFilter` / `parseRFC3339Query` は handler.go 済みで admin handler（task 4）から再利用
  できる（再宣言しないこと）。admin handler は `tenant_id` query の uuid parse を追加し
  `Filter.TenantID` を設定、SuperAdmin TenantContext を `db.WithTenantContext` で確立する点が
  本 handler と異なる。それ以外の残存課題はなし。

### Task 4

- **採用方針**: `admin_handler.go` に admin-console `AdminHandler`（chi.Router 内包で http.Handler /
  chi.Router 双方を満たす）と `NewAdminHandler(svc, authorizer, log)` を実装。handler.go の
  共通資産（`AuditLogDTO` / `toAuditLogDTO` / `writeAuditLogs` / `parseFilter` / `parseRFC3339Query`）を
  再宣言せず流用し、admin 固有の `tenant_id` 任意 parse（`parseTenantIDQuery`）と SuperAdmin
  TenantContext 確立のみを追加した。
- **重要な判断**:
  - **authz↔design 不整合の解決**: design.md は authz を「常に claims.TenantID fallback で
    TargetTenantID 指定」と書くが、authz.Authorize は TargetTenantID が uuid.Nil（SuperAdmin の
    claims.TenantID）に正規化されると role/aud に関わらず無条件 deny する（authz.go /
    Req 4.4 fail-closed）。design 字義どおり claims.TenantID（= uuid.Nil）を渡すと全テナント横断ビュー
    （tenant_id query 無し）が 403 に化け AC Req 3.1 と矛盾する。これを解消するため cross-tenant
    authz matrix 判定は **常に**（tenant_id 指定の有無に関わらず）実施し、tenant_id 無しの全件
    ビューでは代表 probe テナント（`crossTenantAuthzProbeTenantID` / 非 nil sentinel）を
    TargetTenantID に渡す。SuperAdmin session（SessionTenantID=uuid.Nil）に対し任意の非 nil target は
    authz の cross-tenant 分岐へ落ちるため、許可マトリクスの `audit_log read`（cross-tenant =
    SuperAdmin のみ）が main path でも実際に gate する（Req 4.6 / fcf3694 で修正。詳細は下記「確認事項」）。
  - SuperAdmin TenantContext（`TenantID=uuid.Nil, IsSuperAdmin=true`）を `db.WithTenantContext` で
    handler 境界で確立してから `svc.List` を呼ぶ（RLS の is_superadmin 句で全テナント + NULL 可視 /
    Req 3.1 / 3.5）。`Filter.TenantID` は tenant_id query 指定時のみ `*uuid.UUID` で設定（Req 3.2）。
    probe テナントは authz 判定専用であり `Filter.TenantID` には設定しない（Filter は nil のまま）。
  - `warnFailure` は `*Handler` のメソッドで admin から呼べないため、`*AdminHandler` 専用に同等の
    小メソッドを持たせ handler.go を触らずに boundary（AuditAdminHandler）を保った（軽微な重複を許容）。
- **残存課題（次 task への影響）**: admin_handler は task 5 で `routers.Admin.Mount("/audit-logs",
  adminHandler)` 配線（cmd/api / 実 path `/api/admin/audit-logs`）、実 DB + RLS の cross-tenant 可視
  （全テナント + NULL 行の物理可視 / Req 3.1 / 3.5）は task 6（integration）で実 DB 検証する。それ以外なし。

### Task 5

- **採用方針**: 新規ファイルを作らず `cmd/api/main.go` の既存 bootstrap（runBootstrap）に
  audit domain の DI 配線のみを追加。`httpserver.NewServer` の戻り値 routers（従来 `_` で破棄）を
  受け取り、(6) http server 構築直後・(8) runHTTPServer 呼び出し前に Repository / Service /
  Handler / AdminHandler を構築して 2 サブルータへ Mount した（Req 4.2 / 4.4）。
- **重要な判断**:
  - `srv, _, err :=` を `srv, routers, err :=` に変更し routers を捕捉。Mount は
    `routers.API.Mount("/audit-logs", auditHandler)`（実 path `/api/audit-logs`）と
    `routers.Admin.Mount("/audit-logs", auditAdminHandler)`（実 path `/api/admin/audit-logs` /
    固定ガード RequireAdminConsoleAndSuperAdmin 配下）の 2 経路。
  - `cfg` / `pool` / `log`（`logger.Logger`）は既存 bootstrap 構築済みのものを再利用し新規構築しない。
    Clock は `audit.SystemClock{}`、authorizer は `authz.New()`（`audit_log read` 許可マトリクス内包）を
    handler/admin_handler 共通で 1 インスタンス渡す。import に `internal/audit` と
    `internal/platform/authz` を追加。
  - bootstrap step 番号付きコメント慣習に合わせ、新規 audit 配線を (7) として godoc に追記
    （既存 (7) ListenAndServe を (8) に繰り下げ）。本 task は wiring のみで挙動分岐は追加しない。
- **残存課題（次 task=6 への影響）**: 本 task は wiring であり、wiring 起因の 401/403 ガード回帰
  （Req 4.2 / 4.4 = `_Requirements_partial:_` 明示済み）と routing スモークは test server を要するため
  task 6（integration / `audit_test.go` の routing スモーク (i)）へ deferred。本 task 単体の検証は
  `go build ./... && go vet ./... && go test ./...`（コンパイル整合 + 既存テスト非破壊）で完結する。
  実 DB + RLS / append-only / 保持下限の DB-backed verify も task 6 の責務。それ以外の残存課題はなし。

### Task 6

- **採用方針**: `backend/test/integration/audit_test.go` を新設し、実 `audit.NewRepository(pool)` /
  `audit.NewService(cfg, repo, fixedClock)` + `db.WithTenantContext` で task 2/5 の
  `_Requirements_partial:_`（実 DB INSERT・NULL bind・cross-tenant 可視 / wiring 起因 401）を
  実 Postgres で解消。DATABASE_URL 未設定は `requireDBURLs` で `t.Skip`（既存作法踏襲）。
- **重要な判断**:
  - 厳密件数を検証するテスト（a/b/c/f/g）は本テスト固有の `EventType` で `Filter` 絞り込みし、
    `seedDummyData` が事前投入する `seed.event.A/B` 行に影響されないようにした（test-side の
    入力絞り込みであり assertion 緩和ではない）。
  - **本番バグを DB-backed verify で検出・修正**: `internal/audit/repository.go` の `scanEvent` が
    nullable な `resource_id`（0009 で NOT NULL 制約なし）の NULL を `var resourceID string` で
    受けられず `cannot scan NULL into *string` で落ちていた。列省略 INSERT 行（`seedDummyData`
    等）が NULL を持つため SuperAdmin List 等が失敗。`*string` 受け + NULL→空文字写像に修正
    （tenant_id NULL→uuid.Nil と同イディオム / types.go の "対象なし=空文字" 規約と整合）。
    `t.Skip` 経路では露見しない appendix 層のバグで、DB-backed verify 必須工程の有効性を実証。
  - routing スモーク (i) は `httpserver.NewServer` + task5 同配線（audit handler を 2 サブルータへ
    Mount）した test server で `/api/audit-logs` / `/api/admin/audit-logs` が認証なし 401 を確認。
    svc=nil でも先行ガードで handler 本体に到達しないため DB 不要（DB 不在でも PASS）。
- **残存課題（次への影響）**: なし（全タスク完了）。Reviewer は本ファイル「DB-backed verify
  実行結果」節で DB-backed 検証の成否を確認できる。

## AC Traceability（task 6 で担保した範囲 / `backend/test/integration/audit_test.go`）

| AC | 担保テスト（シナリオ） |
|---|---|
| 1.1 | `..._SuperAdminSeesAllDesc` 他（実 Service.Record→Insert で全フィールド追記が取得側で確認） |
| 1.2 | `..._AppendOnly_NullTenantRowImmutable`（NULL bind 後の List で TenantID=uuid.Nil を確認） |
| 2.8 | `..._EmptyResult_ReturnsEmptySliceNilError`（0 件は空 slice + nil） |
| 2.9 | `..._TenantContextIsolation_OtherTenantAndNullInvisible`（tenant A で B/NULL 不可視） |
| 3.1 | `..._SuperAdminSeesAllDesc`（A+B+NULL を occurred_at 降順で全件） |
| 3.2 | `..._SuperAdminFilterByTenant`（Filter.TenantID=A で A 行のみ） |
| 3.4 | `..._EmptyResult_ReturnsEmptySliceNilError`（横断経路も Service 層は同一） |
| 3.5 | `..._SuperAdminSeesAllDesc`（SuperAdmin 文脈で NULL 行含む全テナント可視 + 降順） |
| 4.2 | `..._RoutingSmoke_UnauthenticatedReturns401`（/api/audit-logs 認証なし 401） |
| 4.3 | `..._TenantContextIsolation_OtherTenantAndNullInvisible`（他テナント行の存在を露出しない） |
| 4.4 | `..._RoutingSmoke_UnauthenticatedReturns401`（/api/admin/audit-logs 認証なし 401） |
| 5.2 | `..._RetentionFloor_ExcludesOlderRows`（retentionFloor=-180 日で -200 日行を除外） |
| 5.3 | `..._RetentionFloor_ExcludesOlderRows`（from 未指定 → 起点に丸め保持期間外を除外） |
| 6.1/6.2/6.3 | `..._AppendOnly_UpdateDeleteRejected`（tenant/SuperAdmin 両文脈で UPDATE/DELETE が 42501） |
| 6.4 | `..._AppendOnly_NullTenantRowImmutable`（NULL テナント行への UPDATE/DELETE も 42501） |
| NFR 1.2 | `..._NormalTenant_CrossTenantInsertRejectedAndInTenantRetained`（保持期間内行を欠損なく取得） |
| NFR 2.1 | `..._TenantContextIsolation_OtherTenantAndNullInvisible`（A の自テナント分離を恒常担保） |
| NFR 2.2 | `..._NormalTenant_CrossTenantInsertRejectedAndInTenantRetained`（tenant A で tenant_id=B Insert が WITH CHECK 拒否） |

## DB-backed verify 実行結果

- **DB 起動**: 環境に `.env` / `psql` CLI が無いため、ephemeral Postgres 16 コンテナを起動
  （`docker run -d --name ae-mdm-audit-itest -e POSTGRES_USER=ae_mdm -e POSTGRES_PASSWORD=testpass
  -e POSTGRES_DB=ae_mdm -p 55439:5432 postgres:16-alpine`）。
- **roles**: `db/roles/0001_create_app_and_migration_roles.sql` のパスワード placeholder を実値に
  置換し `docker exec ... psql` で適用（app_user / migration_user 作成）。
- **migrate**: `go run -tags postgres .../migrate -path db/migrations -database <migration_user DSN> up`
  で 15 migration 全適用。
- **実行コマンド**: `DATABASE_URL=<app_user DSN> MIGRATE_DATABASE_URL=<migration_user DSN>
  GOTOOLCHAIN=local go test ./... -count=1`
- **結果（DB-backed）**: 全パッケージ `ok`。`test/integration` の audit 9 テスト（サブ含む）
  すべて PASS。`internal/audit` 単体テストも PASS（scanEvent 修正の非破壊を確認）。
- **`t.Skip` 個数（DB 不在時）**: `audit_test.go` の DB-backed テスト 8 件が `t.Skip`、
  routing スモーク (i) 1 件は DB 不要のため DB 不在でも PASS（計 9 関数）。
  stage-a-verify コマンド `cd backend && go build ./... && go vet ./... && go test ./...` を
  DATABASE_URL 未設定で実行し全 `ok`（false-fail なし）を確認済み。
- **検出した不具合**: DB-backed verify により nullable `resource_id` の scan バグを検出し本番
  コード（`scanEvent`）を修正（上記 Task 6 learnings 参照）。本 commit に同梱。

## AC Traceability（task 1 で担保した範囲）

| AC | 担保テスト（`internal/audit/service_test.go`） |
|---|---|
| 1.1 | `TestService_Record`（ID/OccurredAt 補完・採番）/「ID/OccurredAt が設定済みのとき上書きせず保持する」 |
| 1.2 | `TestService_Record`「TenantID == uuid.Nil がそのまま Repository へ渡る」（NULL bind は task 2 / 6） |
| 1.3 | `TestService_Record`（ResultSuccess を Repository へ渡す） |
| 1.4 | `TestService_Record`「結果失敗のとき ResultFailure をそのまま渡す」 |
| 1.5 | `Service` interface に Record/List のみ（update/delete 非公開 / コンパイル時に担保） |
| 1.6 | `TestService_Record`「Repository が永続化エラーを返したとき成功扱いせずエラー伝播」 |
| 1.7 | `types.go` / `doc.go` / `service.go` の godoc で「機密 sanitize は呼び出し側責務」を明記（Service は Detail 素通し） |
| 2.8 | `TestService_List_EmptyResult`（0 件は空 slice + nil） |
| 3.4 | `TestService_List_EmptyResult`（同上 / 横断経路も Service 層は同一実装） |
| 5.1 | `TestService_List_RetentionFloor`「from 未指定のとき effectiveFrom は保持起点」 |
| 5.2 | `TestService_List_RetentionFloor`（effectiveFrom = retentionFloor を必ず付与） |
| 5.3 | `TestService_List_RetentionFloor`「from が保持起点より前/後」両ケース（丸め / 境界） |
| 5.4 | `TestService_List_DefaultRetentionDiffersFrom365`（retention 180/365 で下限切替） |
| NFR 1.1 | 同上（`cfg.AuditLogRetentionDays` を List 毎に参照し下限を決定） |
| NFR 3.1 | `doc.go`「機密値の非格納契約」/ `Event.Detail` godoc（Service は機密値を補間しない） |

> 注: 1.2（DB への実 NULL bind）の永続層検証は task 2.1 / task 6（integration）の責務。
> task 1 では「Service が TenantID を改変せず素通しする」ことまでを担保した。

## AC Traceability（task 3 で担保した範囲 / `internal/audit/handler_test.go`）

| AC | 担保テスト |
|---|---|
| 2.1 | `TestHandler_List_TenantAdmin_OwnTenant_Returns200`（own-tenant Filter で 200 + JSON 配列。occurred_at desc は Service/Repository 責務） |
| 2.2 | `TestHandler_List_MapsAllFilterFields`（event_type → Filter.EventType 写像） |
| 2.3 | `TestHandler_List_MapsAllFilterFields`（actor_id uuid → Filter.ActorID 写像） |
| 2.4 | `TestHandler_List_MapsAllFilterFields`（resource_id → Filter.ResourceID 写像） |
| 2.5 | `TestHandler_List_MapsAllFilterFields`（from/to RFC3339 → Filter.From/To 写像） |
| 2.6 | `TestHandler_List_FromOnly_ReflectedInFilter`（from のみ指定 / to は nil） |
| 2.7 | `TestHandler_List_ToOnly_ReflectedInFilter`（to のみ指定 / from は nil） |
| 2.8 | `TestHandler_List_EmptyResult_Returns200EmptyArray`（空は `[]` で 200）/ `TestHandler_List_InvalidQuery_Returns400`（不正入力は 400 で区別） |
| 4.1 | `TestHandler_List_Operator_Returns403`（Operator は 403 / svc.List 未呼出） |
| 4.2 | `TestHandler_List_NoClaims_Returns401`（claims 不在は 401 / svc.List 未呼出） |
| 4.5 | `TestHandler_List_Viewer_Returns403`（Viewer は 403 / svc.List 未呼出） |
| 4.6 | `TestHandler_List_TenantAdmin_OwnTenant_Returns200` + `..._Operator/Viewer_Returns403`（実 `authz.New()` の `audit_log read` matrix で TenantAdmin allow / Operator・Viewer deny を担保） |
| NFR 3.1 | `TestHandler_List_EmptyResult_Returns200EmptyArray`（DTO は `[]AuditLogDTO` のみ・追加機密値なし）/ parse 失敗 WARN に query 生値非補間（実装で `warnFailure` が固定 field のみ） |
| NFR 3.2 | `TestHandler_List_InvalidQuery_Returns400`（`failure_kind=parse_invalid` を WARN に出力） |

> 注: 2.9（他テナント / NULL 行を含めない）は本 handler が `Filter.TenantID=nil` 固定で RLS に
> 委ねる設計であり、実 RLS 分離の検証は task 6（integration）の責務。本 task では
> `TestHandler_List_TenantAdmin_OwnTenant_Returns200` が「own-tenant 経路で Filter.TenantID を
> 設定しない（RLS に委ねる）」ことを assert することで構造的に担保した。

## AC Traceability（task 4 で担保した範囲 / `internal/audit/admin_handler_test.go`）

| AC | 担保テスト |
|---|---|
| 3.1 | `TestAdminHandler_List_SuperAdmin_NoTenantID_Returns200AndEstablishesSuperAdminContext`（tenant_id 無しで 200 + SuperAdmin TenantContext 確立を assert / 全テナント + NULL 行を含む） |
| 3.2 | `TestAdminHandler_List_TenantIDQuery_SetsFilterTenantID`（指定で Filter.TenantID）/ `..._NoTenantIDQuery_FilterTenantIDNil`（無指定で nil）/ `..._InvalidQuery_Returns400`（非 uuid は 400） |
| 3.3 | `TestAdminHandler_List_MapsAllFilterFields`（event_type / actor_id / resource_id / from / to を Filter へ写像） |
| 3.4 | `TestAdminHandler_List_EmptyResult_Returns200EmptyArray`（空は `[]` で 200） |
| 3.5 | `TestAdminHandler_List_SuperAdmin_NoTenantID_..._EstablishesSuperAdminContext`（TenantContext.IsSuperAdmin=true / TenantID=uuid.Nil を assert）/ `..._TenantIDQuery_SetsFilterTenantID`（tenant 絞り込み写像） |
| 4.4 | 固定ガード `RequireAdminConsoleAndSuperAdmin` 配下の前提（claims 不在は防御的に 401）+ tenant_id 無しビューは固定ガード依拠（下記「確認事項」/ 実ガード検証は httpserver パッケージ責務） |
| 4.6 | `TestAdminHandler_List_TenantIDQuery_SetsFilterTenantID`（実 `authz.New()` で SuperAdmin × admin-console × cross-tenant が allow → 200。tenant_id 指定時のみ matrix 判定） |
| NFR 3.1 | `TestAdminHandler_List_EmptyResult_Returns200EmptyArray`（DTO は `[]AuditLogDTO` のみ）/ parse 失敗 WARN に query 生値非補間（実装で `warnFailure` が固定 field のみ） |
| NFR 3.2 | `TestAdminHandler_List_InvalidQuery_Returns400`（`failure_kind=parse_invalid` を WARN に出力 / tenant_id・actor_id 非 uuid・from・to 非 RFC3339 の 4 ケース） |

> 注: 4.4（admin aud + SuperAdmin 強制）の実ガード判定は `RequireAdminConsoleAndSuperAdmin`
> （httpserver パッケージ・#37 で検証済み）の責務。admin handler は固定ガード配下前提で claims
> 不在を防御的に 401 にする補完のみ担う。実 RLS の cross-tenant 物理可視（3.1 / 3.5）は task 6
> （integration）が実 DB で検証する。

## 確認事項

### Repository interface の宣言場所（設計乖離 / task 2.1 実装者への申し送り）

- **乖離内容**: design.md L375-379 および tasks.md task 2.1 は `repository.go` が `Repository`
  interface を宣言する記述になっているが、本 task では `Repository` interface を **service.go 内**
  （consumer-defines-interface イディオム）で宣言した。
- **理由**: per-task ループでは task 1 完了時点で `internal/audit` パッケージが単独で
  `go build` / `go test` を通過する必要がある。service.go が参照する `Repository` 型が未定義だと
  パッケージがビルド不能になるため、利用側（service.go）に interface を置いた。シグネチャは
  design.md L375-379（`Insert(ctx, Event) error` / `Select(ctx, Filter, effectiveFrom time.Time)
  ([]Event, error)`）と厳密に一致させている。
- **task 2.1 実装者への申し送り**: `repository.go` では `Repository` interface を **再宣言しない**
  こと（同一パッケージ内での二重宣言はコンパイルエラーになる）。task 2.1 は本 interface を満たす
  concrete struct（例: `type repository struct { pool *pgxpool.Pool }`）と
  `NewRepository(pool *pgxpool.Pool) Repository` のみを実装する。Insert の NULL bind / Select の
  動的 SQL 組み立ては design.md「Audit Repository」Invariants に従う。

### 本 task で着手していない範囲

- task 2 以降（repository.go の concrete 実装 / handler.go / admin_handler.go / cmd/api 配線 /
  integration test）には着手していない。本 task は types / clock / failure_kinds / doc / service と
  その単体テストのみ。

### authz↔design 不整合の解決（設計乖離 / task 4 / spec 本文は書き換えず）

- **乖離内容**: design.md「Audit Admin Handler」L435 および tasks.md task 4 step 3 は、authz を
  「`TargetTenantID = <tenant_id query があればその値 / 無ければ claims.TenantID.String()>`」で
  常に呼ぶ記述になっている。しかし `internal/platform/authz/authz.go:156-159`（`Authorize` /
  `AuthorizeAndLog`）は **TargetTenantID が空文字 / uuid.Nil に正規化されると role/aud に関わらず
  無条件 deny**（Req 4.4 fail-closed）する。SuperAdmin の admin-console claims は
  `claims.TenantID == uuid.Nil` のため、design 字義どおり「tenant_id 無し → claims.TenantID
  fallback」で authorize すると TargetTenantID=uuid.Nil → authz deny → 403 となり、**AC Req 3.1
  （tenant_id 無しの全テナント横断ビューは 200）と矛盾**する。
- **解決方針（実装済み / fcf3694 で更新）**: cross-tenant authz matrix 判定（Req 4.6）は
  **tenant_id 指定の有無に関わらず常に** 行う（deny なら 403 + `failure_kind=authz_denied`）。
  tenant_id query 指定時は TargetTenantID=その tenant_id を、無指定（全テナント横断ビュー）時は
  代表 probe テナント（`crossTenantAuthzProbeTenantID` / 非 nil sentinel）を TargetTenantID に渡す。
  SuperAdmin session（SessionTenantID=uuid.Nil）に対し任意の非 nil target は authz の cross-tenant
  分岐へ落ちるため、いずれの経路でも許可マトリクスが cross-tenant `audit_log read`（SuperAdmin の
  み）を main path で gate する（Req 4.6）。probe は authz 判定専用で Filter.TenantID には設定しない
  （Filter は nil のままで RLS の SuperAdmin 句が全テナント + NULL を可視にする / Req 3.1 / 3.5）。
  claims 不在は防御的に 401。
- **位置付け**: design.md の字義（常に claims.TenantID fallback で authorize）は SuperAdmin の
  claims.TenantID=uuid.Nil を fail-closed deny に化けさせ AC Req 3.1 と両立しないため、authz 判定専用の
  probe sentinel を介在させて全テナントビューでも matrix を発火させる **意図的な乖離**である。理由
  （authz Req 4.4 の uuid.Nil fail-closed と AC Req 3.1 / Req 4.6 の両立）を本項に明記し、spec 本文
  （design.md / tasks.md / requirements.md）は書き換えていない。PM / Architect の判断が必要なら本項を
  起点に差し戻し可能。当初は「tenant_id 指定時のみ matrix 判定 / 全件ビューは固定ガード依拠」で実装
  したが、Req 4.6（全テナントビューにも matrix 適用）を main path で満たすため fcf3694 で常時判定へ
  改めた。

## 検証結果（サマリ）

実行コマンド: `cd backend && go build ./... && go vet ./... && go test ./...`

- `go build ./...`: PASS（エラーなし）
- `go vet ./...`: PASS（警告なし）
- `go test ./...`: PASS（全パッケージ ok / 失敗なし）
  - `internal/audit` 単体テスト: `TestService_Record`（5 サブテスト）/ `TestService_List_RetentionFloor`
    （4 サブテスト）/ `TestService_List_DefaultRetentionDiffersFrom365` / `TestService_List_PassesFilterAndResult`
    / `TestService_List_EmptyResult` / `TestService_List_PropagatesSelectError` すべて PASS。
  - 既存テスト（auth / config / policy / platform/* / integration 等）に破壊なし。

STATUS: complete
