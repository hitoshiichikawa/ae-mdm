# Implementation Notes

## Implementation Notes

### Task 1

- **採用方針**: errors → logger → config の一方向依存で 3 package を新規実装し、
  cmd/api / cmd/worker / db / httpserver から共通 import 可能な基盤を確立する。
- **重要な判断**
  - `internal/errors` は他 internal package を import しない（`ErrLogger` interface を
    パッケージ内で定義し、`logger.Logger` の Warn / Error が structural typing で暗黙的に
    満たす設計）。Go の import cycle を物理的に回避し、design.md「Components and Interfaces」
    の依存方向と完全一致させた。
  - `logger.Err(err error)` は `*errors.Error` のときに `zap.Inline` 内で
    `error_code` / `error_message` / `error_cause` を構造化し、独自型外 error は標準の
    `zap.Error` にフォールバックする。logger 側のテスト
    （`TestLogger_SatisfiesErrLoggerInterface`）で compile-time に interface 整合を固定。
  - redaction は field 名のサブストリング一致（lowercase）で `redactFields` を kv ペアに
    適用する方式を採用。`set-cookie` のような派生キーも `cookie` が部分一致するため
    自動的に対象になり、design.md Security Considerations の「session_secret / id_token /
    refresh_token / cookie / google_application_credentials / sa_json / private_key /
    password」を網羅する。なお zap.Field 直渡しの値（型変換済み）は redaction されないため、
    機密値はかならず kv ペア（"id_token", token）で渡す運用とする。
  - `config.Load()` は値型 Config を返し、`loadFrom(envGetter)` を内部に切り出すことで
    `t.Setenv` を使う公開 API smoke と、決定論的な map ベース env テストの両立を実現。
    Req 1.4（immutable）は「pointer を返さない」「公開セッターを置かない」で担保。
  - `WriteHTTP` の called-once 契約は (a) request context 経由 sentinel と
    (b) writer ポインタを key にした `sync.Map` の二重で実装。HTTP ハンドラ defer 終了時
    に `ClearWriter` を呼ぶことで sentinel エントリのリークを防ぐ前提（後続 task 4.1 の
    recover middleware で利用する）。
- **残存課題（次 task への申し送り）**
  - `internal/platform/db` 配下（task 2.x）で `BeginTxFunc` から `errors.New(CodeTenantCtxMissing, ...)`
    を panic する際、recover 後の `WriteHTTP` 経路で `ClearWriter` を defer 呼び出しする
    こと。Issue は無いが期待動作として明記。
  - logger の `fileOpener` は init 時 1 回だけ呼ぶ前提で、ファイルローテーション
    （lumberjack 等）は本 task では未対応。NFR 4.2 を満たすコンテナ運用では通常 stderr /
    stdout で十分のため見送り。
  - `logger.SetDefault` / `Default` は process global の便宜上の API。本 Issue 後続 task
    （cmd/api / cmd/worker）で main から SetDefault を 1 回だけ呼ぶ運用前提。

### Task 2

- **採用方針**: `internal/platform/db` に pgxpool 構築（`NewPool`）と Tenant Context 強制
  （`TenantContext` / `WithTenantContext` / `FromContext`）/ RLS GUC 発行
  （`SetLocalTenant`）/ tx 境界統一（`BeginTxFunc`）の 3 系統を追加し、後続 task で
  `internal/platform/db` を import するだけで「tx 開始時に必ず `app.tenant_id` が発行され、
  TenantContext 不在の経路では panic で物理的に止まる」状態を提供する。実 PostgreSQL に
  依存しない unit test のみで panic ガード・rollback・re-panic・commit の全分岐を網羅し、
  実 DB を要する RLS verify は task 6.1 の integration test に委譲する。
- **重要な判断**
  - **fake interface の導入根拠**: `BeginTxFunc` の公開 signature は design.md（Components:
    TxManager + RLS Helper 節）通り `*pgxpool.Pool` を受けるが、内部実装は最小 interface
    `txBeginner { BeginTx(ctx, pgx.TxOptions) (pgx.Tx, error) }` を経由する 2 段構成にした。
    `*pgxpool.Pool` が同 interface を自然に満たすため公開 API の見た目は design 通りに保ち、
    かつ unit test では fake pool / fake tx を注入できる。`pgx.Tx` は pgx 本体が既に interface
    として公開しているため、`fakeTx` は pgconn.NewCommandTag を返す最小実装で済んだ。
    compile-time check `var _ txBeginner = (*pgxpool.Pool)(nil)` と
    `var _ pgx.Tx = (*fakeTx)(nil)` を rls_test.go に置き、interface drift を物理的に検出する。
  - **panic payload の型を `*errors.Error` に固定**: `BeginTxFunc` の TenantContext 不在時
    panic は `panic(errors.New(CodeTenantCtxMissing, ...))` で *errors.Error を payload に
    する。後段 recover middleware（task 4.1）が `recover()` の値を `*errors.Error` に
    type-assert して `WriteHTTP` 経由で 500 + 構造化 ERROR ログに写像する経路を成立させる
    ため。task 1 の learning にも明記された期待動作と整合。
  - **SuperAdmin 分岐の根拠（`current_setting` NULL = default deny）**: SuperAdmin かつ
    `TenantID == uuid.Nil` の場合は `app.tenant_id` を **set しない**。RLS ポリシーが
    `current_setting('app.tenant_id', true)::uuid OR current_setting('app.is_superadmin', true)::boolean`
    で書かれている前提で、cross-tenant SuperAdmin 操作中に `app.tenant_id` が偶発的に
    残ると tenant_id 比較経路が誤マッチする可能性がある。`current_setting` が NULL を返す
    ようにすれば、`tenant_id = NULL`（不定）と SuperAdmin OR 句のみが評価され、SuperAdmin
    が false の場合は default deny に倒れる。`TenantID==uuid.Nil` で SuperAdmin=false の
    場合は `app.tenant_id` に空 UUID 文字列が set されるが、これは現状 SuperAdmin 系経路
    でしか到達しない invariant のため、現時点では追加の防御コードは入れない（必要なら
    後続 task 4.1 の middleware で「IsSuperAdmin=false かつ TenantID==uuid.Nil を 401」と
    弾く）。
  - **`set_config` vs `SET LOCAL` の使い分け**: PostgreSQL の `SET LOCAL ... = $1` は
    バインドパラメータ不可（リテラル展開のみ）のため、tenant_id を bind するには
    `SELECT set_config('app.tenant_id', $1, true)` を使う。design.md にも同注記あり。
    `app.is_superadmin` は値が固定文字列 `'true'` なので、対称性のために `set_config(...)`
    で揃えた（bind は不要）。SQL インジェクション耐性も向上する。
  - **`pgx.Tx` interface を直接ストレートに受ける**: `SetLocalTenant` は `pgx.Tx` を受ける
    形にした。pgx が interface として公開しているため、テストでは fake 実装を渡せる。
    `nil tx` の防御的経路は CodeInternal で返し、`SetLocalTenant` の単独呼び出し（通常は
    BeginTxFunc 経由）でも fail-fast を確保。
  - **commit エラーの取扱い**: `commit` 失敗は domain error（fn の返した error）と区別して
    `*errors.Error{Code: CodeInternal}` で wrap する。fn が rollback を経由せず success
    したのに DB 側で commit が失敗するのは infra 問題であり、後段の recover / WriteHTTP で
    500 として扱うのが妥当（HTTP の業務応答に Bleed させない）。
- **残存課題（次 task への申し送り）**
  - **task 3.x（マイグレーション）への申し送り**: `tenant_isolation_*` ポリシーの USING / WITH CHECK は
    本 task の `SetLocalTenant` が発行する GUC `app.tenant_id` / `app.is_superadmin` 名と
    完全一致させること（typo は cross-tenant leak の直接原因になる）。設計書通り
    `current_setting('app.tenant_id', true)::uuid OR current_setting('app.is_superadmin', true)::boolean`
    で記述すること。
  - **task 4.1（recover middleware）への申し送り**: `BeginTxFunc` の panic 経路は
    payload に `*errors.Error{Code: CodeTenantCtxMissing}` を載せる契約。recover middleware は
    `recover()` の値が `*errors.Error` のとき `WriteHTTP(w, r, e, log)` に渡し、それ以外の
    panic は CodeInternal で wrap して 500 + ERROR ログ。`ClearWriter`（task 1 で実装済）
    の defer 呼び出しも忘れずに（task 1 の learning と整合）。
  - **nested tx は未対応**: 後続ドメインで savepoint 相当が必要になった場合、本 task の
    txmanager.go の signature を破壊せず別 helper を追加する方針が望ましい。
  - **`/api/admin` 配下の cross-tenant 操作**: SuperAdmin で `TenantID==uuid.Nil` を埋める
    middleware（task 4.1 の `RequireSuperAdmin` 経路）が、本 task の SetLocalTenant の
    cross-tenant 分岐に正しく合流するように、TenantContext を組み立てる箇所で
    `TenantID=uuid.Nil, IsSuperAdmin=true` を明示的に設定すること。

### Task 3

- **採用方針**: SQL マイグレーション 12 ペア（0001-0010 = CREATE TABLE + enum、
  0011 = RLS 有効化 + 汎用 / subselect / SuperAdmin 専用ポリシー、0012 = audit_logs
  append-only 強制）と DB ロール定義 SQL（`backend/db/roles/0001_*.sql`）を追加し、
  task 2 で実装した `SetLocalTenant` が発行する GUC（`app.tenant_id` / `app.is_superadmin`）
  と完全一致する RLS ポリシーで物理分離を確立する。本 task は SQL / Makefile / runbook /
  `.env.example` の追加 / 変更のみで Go コードは触らない（unit test は task 6.1 の
  integration test に委譲）。
- **重要な判断**
  - **GUC 名の完全一致**: 0011 / 0012 の RLS ポリシーが参照する `app.tenant_id` /
    `app.is_superadmin` は、`backend/internal/platform/db/rls.go` の `SetLocalTenant`
    が `set_config(key, value, true)` で発行する文字列と byte レベルで一致させた。
    task 2 の learning でも明示された通り、typo は cross-tenant leak の直接原因に
    なるためコード生成 / 切り出しはせず、SQL コメント側に「rls.go の SetLocalTenant
    が発行する名称と完全一致させること」を必須注釈として残した。
  - **audit_logs INSERT WITH CHECK の二重防御**: 0012 の `audit_logs_insert` ポリシーは
    `tenant_id = current_setting('app.tenant_id', true)::uuid OR current_setting('app.is_superadmin', true)::boolean`
    とし、(a) 通常テナント文脈で他テナント宛 / NULL の挿入を物理拒否、(b) SuperAdmin
    文脈で cross-tenant 監査 / tenant_id=NULL システム監査の挿入を許容、の 2 経路を
    OR で素通りさせる設計に固定。UPDATE / DELETE はポリシー未定義（0011 の汎用 FOR ALL
    ポリシーも作っていない）+ `REVOKE UPDATE, DELETE ON audit_logs FROM app_user` の
    二重防御で改竄不可を担保。`REVOKE` は `app_user` 未作成環境向けに
    `DO $$ ... undefined_object EXCEPTION ... END $$` で NOTICE skip 経路を入れた。
  - **sessions の subselect ポリシー**: `sessions.tenant_id` カラムが存在しないため、
    `admin_users.tenant_id` を `EXISTS (SELECT 1 FROM admin_users WHERE ...)` で参照する
    `tenant_isolation_sessions` を採用。post-tenant 経路（TenantContext 確立後）では
    通常通り分離が成立する一方、認証 lookup（token_hash でセッションを引く pre-tenant
    経路）は本ポリシー下で 0 行に倒れるため、後続 Issue の Session Manager は
    `app.is_superadmin=true` で SuperAdmin 文脈として lookup する前提（design.md
    「sessions の認証 lookup 経路」散文と整合）。本 task では lookup ヘルパは未実装。
  - **migration_user / app_user の権限粒度**: `migration_user` = DDL 全権限（`ALL
    PRIVILEGES ON DATABASE` + `ALL ON ALL TABLES IN SCHEMA public` + `ALTER DEFAULT
    PRIVILEGES`）、`app_user` = DML のみ（`SELECT, INSERT, UPDATE, DELETE ON ALL TABLES`
    + `USAGE, SELECT ON ALL SEQUENCES` + `ALTER DEFAULT PRIVILEGES`）の方針を採用。
    `audit_logs` の UPDATE/DELETE は本ファイルでは GRANT し、0012 で `REVOKE` する
    ことで二重防御を成立させる（GRANT → REVOKE の順序逆転に注意）。`migration_user`
    自身は `audit_logs` への UPDATE/DELETE も可能だが、`FORCE ROW LEVEL SECURITY` で
    テーブル所有者からも RLS が回避不可になるため、INSERT ポリシーで tenant_id の
    整合性が要求される。
  - **冪等性イディオムの選定**: テーブル / インデックスは `IF NOT EXISTS`、enum / role
    は `DO $$ ... duplicate_object EXCEPTION ... END $$`、policy は `DROP POLICY IF
    EXISTS ... ; CREATE POLICY ...` の 3 種で書き分け。PostgreSQL 16 では
    `CREATE POLICY IF NOT EXISTS` が未対応のため、policy だけは drop-recreate パターンで
    冪等化する選択を取った（design.md Migration Strategy 節と整合）。
  - **`make db-init-roles` の psql 依存**: roles の適用は `golang-migrate` の管轄外
    （schema_migrations に記録しない初期セットアップ）であり、`psql` CLI を `go run`
    経由で起動するのは現実的に困難なため、host 上の `psql` を必須とするシンプルな
    target を採用。psql 未インストール環境向けに `docker compose cp + docker compose
    exec psql` の代替手順を runbook に併記した。
- **残存課題（次 task への申し送り）**
  - **task 4.1（recover / TenantContextMiddleware）への申し送り**: 本 task の RLS
    ポリシーは「`current_setting('app.tenant_id', true)` が NULL ならポリシー false に
    倒れる」設計のため、TenantContextMiddleware が **claims を ctx に注入しない default
    deny 状態**でも `BeginTxFunc` 経由の DB アクセスは panic ガード（task 2）+ RLS の
    二重で物理的に止まる。auth スタブの default deny 挙動は本 task の RLS と完全に
    整合している。
  - **task 5.1 / 5.2（cmd/api / cmd/worker bootstrap）への申し送り**: `DATABASE_URL`
    は `app_user` 接続前提に変更済み。bootstrap で `db.NewPool(ctx, cfg)` が ping で
    疎通確認するときに、`app_user` ロールが未作成の環境では接続失敗するため、
    `NFR 3.1 / 3.2` の fail-fast 経路に乗る（CodeUnavailable で exit 1）。
    `make db-init-roles` 未実行のオペレータミスをログで明示する責務は cmd/api 側の
    ERROR ログに任せる（接続文字列の user 名を redaction 通過後に出力する想定）。
  - **task 6.1 / 6.2（integration test）への申し送り**: `docker compose up -d postgres`
    + `make db-init-roles` + `make migrate-up` の 3 ステップで実 RLS verify を回せる。
    integration test 側で migrate-up を Go コードから実行する場合、`migration_user` 接続を
    使うこと（`app_user` では DDL 失敗）。`audit_logs` の append-only verify は `app_user`
    接続で行い、`migration_user` 接続でやると REVOKE が効かないため誤判定する点に注意。
    task 6.1 の (g) WITH CHECK 二重防御テストは、本 task の 0012 INSERT ポリシーが
    意図通りに動作することを検証する経路で、本 task の judgment（design.md L796-804）と
    1:1 対応する。
  - **`audit_logs` UPDATE/DELETE REVOKE の発火タイミング**: 0012 を `app_user` 未作成で
    適用すると NOTICE skip になり REVOKE が効かないため、後から `make db-init-roles` を
    実行した場合は手動で `REVOKE UPDATE, DELETE ON audit_logs FROM app_user` を再発行
    するか、`make migrate-down → migrate-up` で 0012 を再適用する。本注意点は runbook に
    明記済み。

## AC Traceability（Req 1 / 2 / 3 / 4 / NFR 1.1 部分）

| Requirement | テスト |
|---|---|
| 1.1 構造体としての env 読み込み | `config.TestLoad_AllRequiredPresent_ReturnsConfig` / `TestLoad_FromOSEnv_Smoke` |
| 1.2 必要 env を網羅 | `config.TestLoad_AllRequiredPresent_ReturnsConfig`（13 件の required を carry / `validEnv` fixture） |
| 1.3 必須欠落・不正フォーマットで fail-fast | `config.TestLoad_MissingRequired_ReturnsConfigInvalid` / `TestLoad_InvalidIntFormat_ReturnsConfigInvalid` / `TestLoad_SessionSecretTooShort_ReturnsConfigInvalid` |
| 1.4 immutable | 値型返却（pointer 共有しない設計）+ `TestLoad_AllRequiredPresent_ReturnsConfig` の Config 値型確認 |
| 1.5 api / worker / CLI 共通 import 可 | `internal/config` 配置（cmd/api / cmd/worker の bootstrap は task 5.x で配線） |
| 2.1 構造化ログファクトリ | `logger.TestNewLogger_AppliesConfigDefaults` |
| 2.2 tenant_id / request_id / message_id field helper | `logger.TestLogger_AddsFieldsHelpers` |
| 2.3 エラー観測時 WARN / ERROR 記録 | `errors.TestWriteHTTP_DomainError` / `errors.TestShouldAck_TransientDomainError_NacksWithWarn` / `errors.TestShouldAck_PermanentDomainError_AcksWithError` |
| 2.4 レベル / フォーマット / 出力先を env から | `logger.TestLogger_LevelThreshold_FiltersBelow` / `logger.TestNewLogger_RejectsInvalidLevel` / `TestNewLogger_RejectsInvalidFormat` / `TestNewLogger_FileOutput` |
| 2.5 機密の平文出力禁止 | `logger.TestRedactFields_RedactsSecretsBySubstring`（11 ケース）/ `TestLogger_RedactsSecretFieldValues` |
| 3.1 Code / Message / Cause を持つ Error 型 | `errors.TestError_ErrorMessage` |
| 3.2 errors.Is / As 互換 | `errors.TestError_IsAsCompatibility` |
| 3.3 HTTP ステータスマッピング | `errors.TestDefaultHTTPStatus_Mapping`（12 ケース）/ `TestWriteHTTP_DomainError` |
| 3.4 worker 最外層 ack / nack 判定 | `errors.TestShouldAck_NilError_AcksAndNoLog` / `TestShouldAck_TransientDomainError_NacksWithWarn` / `TestShouldAck_PermanentDomainError_AcksWithError` / `TestShouldAck_UnknownErrorType_NacksWithWarn`（4 ケースマトリクス） |
| 3.5 予期しないエラーは 5xx + 構造化ログ | `errors.TestWriteHTTP_NonDomainError_DefaultsTo500` |
| 4.1 pgx 接続プール構築 | `db.TestNewPool_InvalidConnString_ReturnsServiceUnavailable` / `TestNewPool_UnreachableHost_ReturnsServiceUnavailable` |
| 4.2 tx 境界を単一ヘルパで管理 | `db.TestBeginTxFunc_NormalReturn_Commits` / `TestBeginTxFunc_FnReturnsError_RollsBackAndReturnsError` / `TestBeginTxFunc_CommitFails_WrapsAsInternal` / `TestBeginTxFunc_BeginTxFails_WrapsAsInternal` |
| 4.3 tx 内で `SET LOCAL app.tenant_id` 発行 | `db.TestSetLocalTenant_NormalTenant_EmitsSetConfigForTenantID`（SQL = `SELECT set_config('app.tenant_id', $1, true)` をバインド値検証） |
| 4.4 SuperAdmin で `app.is_superadmin=true` 併発行 | `db.TestSetLocalTenant_SuperAdminWithTenantID_EmitsBothSetConfigs` / `TestSetLocalTenant_SuperAdminCrossTenant_OnlyIsSuperAdminEmitted`（cross-tenant 経路の `app.tenant_id` 非発行を確認） |
| 4.5 tenant context 不在で panic ガード | `db.TestBeginTxFunc_TenantContextMissing_Panics`（panic payload が `*errors.Error{Code: CodeTenantCtxMissing}` であることと BeginTx 未呼び出しを確認）/ `TestFromContext_Missing_ReturnsTenantCtxMissing` / `TestFromContext_NilCtx_ReturnsTenantCtxMissing` |
| NFR 1.1（部分） アプリ層 + RLS の両方有効化 | `db.TestSetLocalTenant_*`（GUC 発行）+ `TestBeginTxFunc_TenantContextMissing_Panics`（アプリ層強制）。実 RLS ポリシーとの結合 verify は task 6.1 の integration test に委譲 |
| NFR 3.1 起動時 fail-fast | `db.TestNewPool_*`（CodeUnavailable で fail-fast）+ task 1 の config 側 |
| NFR 3.2 リクエスト処理前に外部依存初期化完了 | `db.NewPool` が Ping で疎通検証してから返す経路（cmd/api 配線は task 5.1） |

Req 5 / 6 / 7 / NFR 1.2 / NFR 2.x / NFR 4.x は後続 task（3.x / 4.x / 5.x / 6.x）の責務であり、本 task では未対応。

## Verify

```sh
cd backend && go build ./... && go vet ./... && go test ./...
```

- task 1 完了時点（commit `7aebc12`）:
  - `go build ./...`: PASS
  - `go vet ./...`: PASS
  - `go test ./...`: `internal/config` / `internal/errors` / `internal/logger`: PASS、
    `cmd/api` / `cmd/worker` / `internal/depspin`: no test files
- task 2 完了時点（本 task; commit `4e6d509` 以前で実行）:
  - `go build ./...`: PASS
  - `go vet ./...`: PASS
  - `go test ./...`: `internal/config` / `internal/errors` / `internal/logger` /
    **`internal/platform/db`**: PASS（`internal/platform/db` で 18 件の test が
    pass / `TestNewPool_*` 2 件・`TestSetLocalTenant_*` 5 件・`TestBeginTxFunc_*` 8 件・
    `TestFromContext_*` 2 件・`TestWithTenantContext_RoundTrip` 1 件）。
    `cmd/api` / `cmd/worker` / `internal/depspin`: no test files
- task 3 完了時点（本 task; SQL / Makefile / runbook / .env.example の追加のみで Go コード非変更）:
  - `go build ./...`: PASS
  - `go vet ./...`: PASS
  - `go test ./...`: task 2 と同一結果（`internal/config` / `internal/errors` /
    `internal/logger` / `internal/platform/db`: PASS、`cmd/api` / `cmd/worker` /
    `internal/depspin`: no test files）。SQL 自体の verify は task 6.1 / 6.2 の
    integration test に委譲する（実 PostgreSQL が必要なため本 task では unit test なし）。

## 確認事項

なし（本 task 範囲では design.md / requirements.md / tasks.md と矛盾無し）。

STATUS: complete
