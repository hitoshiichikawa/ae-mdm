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
