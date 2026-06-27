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
