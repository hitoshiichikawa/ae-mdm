# Implementation Plan

> 本 Issue（#2）は ae-mdm の共通プラットフォーム基盤（config / logger / errors / DB pool +
> Tenant Context + RLS / HTTP サブルータ / マイグレーション + audit_logs append-only）を提供する。
> 各ドメインの handler / service / repository 実装は後続 Issue（A3 以降）で行う。
> 並列実行可能なタスクには `(P)` を付け `_Boundary:_` で担当 Components を明示する。

- [x] 1. 設定・ロガー・エラー型の共通基盤
- [x] 1.1 Config Loader（env → Config struct + fail-fast）(P)
  - `backend/internal/config/config.go` に `Config` struct（DatabaseURL / OIDC × 2 / Pub/Sub /
    AMAPI / SessionSecret / AuditLogRetentionDays / DeviceSyncDelayThresholdHours /
    LogLevel / LogFormat / LogOutput / HTTPListenAddr 等）を定義
  - `backend/internal/config/env.go` に required / optional / int / duration の各パーサ
    ヘルパを実装。required 欠落・int 解析失敗・`SESSION_SECRET` 32 文字未満で
    `*errors.Error{Code: "config_invalid"}` を返す（fail-fast）
  - `backend/internal/config/config_test.go` に正常系・required 欠落・int 解析失敗・
    default 適用（`AUDIT_LOG_RETENTION_DAYS` 未指定で 180 / `LogLevel` 未指定で `info`）の
    ユニットテストを `t.Setenv` ベースで配置
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5_
  - _Boundary: Config_
- [x] 1.2 Structured Logger（zap ラッパ + redaction）(P)
  - `backend/internal/logger/logger.go` に `Logger` interface（Debug/Info/Warn/Error/With/Sync）
    と `NewLogger(cfg)` ファクトリを実装。zap の core を `cfg.LogLevel` / `cfg.LogFormat`
    （json|console）/ `cfg.LogOutput`（stderr|stdout|path）で構築
  - field ヘルパ `TenantID(uuid)` / `RequestID(string)` / `MessageID(string)` / `ActorID(uuid)` /
    `Err(error)` を提供（`Err` は `*errors.Error` の Code/Message/Cause を構造化）
  - `backend/internal/logger/redact.go` で機密キー allowlist（`session_secret` / `id_token` /
    `access_token` / `refresh_token` / `cookie` / `google_application_credentials` / `sa_json`
    / `private_key` / `password`）に該当する field 値を `***` に置換
  - `backend/internal/logger/logger_test.go` に field 付与・redaction・level 切替（DEBUG/INFO
    の閾値）のユニットテストを配置
  - _Requirements: 2.1, 2.2, 2.3, 2.4, 2.5_
  - _Boundary: Logger_
- [x] 1.3 Domain Error 型 + HTTP マッピング (P)
  - `backend/internal/errors/codes.go` に `Code` 型と定数（`invalid_request` / `unauthenticated`
    / `forbidden` / `not_found` / `conflict` / `business_rule_violation` / `internal_error` /
    `amapi_upstream_error` / `service_unavailable` / `config_invalid` /
    `tenant_context_missing`）を定義
  - `backend/internal/errors/errors.go` に `Error` struct（Code/Message/HTTPStatus/IsTransient/
    Cause）と `New`/`Wrap`/`Error()`/`Unwrap()` を実装（`errors.Is/As` 互換）。HTTPStatus=0 の
    場合 Code から自動決定するヘルパ `defaultHTTPStatus(code)` を内部に持つ
  - `backend/internal/errors/http_mapping.go` に `WriteHTTP(w, r, err, log)` を実装。独自 Error
    型でない error は `Code="internal_error"` / HTTPStatus=500 として wrap し ERROR ログ。
    同じ writer に 2 回書かない契約を守るため内部で sentinel を立てる
  - `backend/internal/errors/worker_mapping.go` に `ShouldAck(err error, log ErrLogger) (ack bool)`
    を実装（Req 3.4）。判定規則: `err == nil` → ack（log 呼ばない）/ `*Error` で
    `IsTransient=true` → nack（WARN ログ）/ `*Error` で `IsTransient=false` → ack（ERROR ログ）/
    独自 Error 型でない error → `CodeInternal/IsTransient=true` として wrap した上で nack
    （WARN ログ）として扱う（HTTP マッピングの default 500 / IsTransient=true 経路と整合）
  - `backend/internal/errors/errors_test.go` に `errors.Is/As` 互換・各 Code → HTTP status
    マッピング・default 500 / ERROR ログ呼び出し（fake logger で検証）・**`ShouldAck` の 4 ケース
    判定マトリクス**（nil / Transient=true / Transient=false / 独自型外 error）と log 副作用
    （nil 時に呼ばない / それ以外で 1 回呼ぶ）のユニットテストを配置
  - _Requirements: 3.1, 3.2, 3.3, 3.4, 3.5_
  - _Boundary: Errors_
  - _Depends: 1.2_

- [x] 2. DB 接続プール + Tenant Context + RLS Helper
- [x] 2.1 pgxpool 構築と起動時 Ping
  - `backend/internal/platform/db/pool.go` に `NewPool(ctx, cfg) (*pgxpool.Pool, error)` を
    実装。`cfg.DatabaseURL` を `pgxpool.ParseConfig` → `pgxpool.NewWithConfig` で構築し、
    `Pool.Ping(ctx)` で疎通確認。失敗時は `*errors.Error{Code: "service_unavailable"}` を返す
  - `pool_test.go` には正常な接続文字列で pool 構築可能であること（ping は実 DB が無いと
    回らないため Ping 部分は integration テストに委譲）と、不正な接続文字列で error が返る
    ことのユニットテストを配置
  - _Requirements: 4.1, NFR 3.1, NFR 3.2_
  - _Boundary: DBPool_
  - _Depends: 1.1, 1.3_
- [x] 2.2 TenantContext 型 + TxManager + RLS Helper + panic ガード
  - `backend/internal/platform/db/context.go` に `TenantContext` struct（TenantID /
    AdminUserID / Roles / IsSuperAdmin）と `WithTenantContext(ctx, tc)` /
    `FromContext(ctx) (TenantContext, error)` を実装。未設定時は
    `*errors.Error{Code: "tenant_context_missing"}` を返す
  - `backend/internal/platform/db/rls.go` に `SetLocalTenant(ctx, tx, tc) error` を実装。
    `SELECT set_config('app.tenant_id', $1, true)` をパラメータバインドで発行し（PostgreSQL の
    `SET LOCAL ... = $1` はバインドパラメータ不可のため、tx-local GUC は `set_config(key, value, is_local=true)`
    で設定する）、SuperAdmin 時は `SELECT set_config('app.is_superadmin', 'true', true)` を追加発行（TenantID=uuid.Nil の SuperAdmin は
    `app.tenant_id` をセットしない / `current_setting` が NULL を返す → default deny に倒れる）
  - `backend/internal/platform/db/txmanager.go` に `BeginTxFunc(ctx, pool, fn) error` を実装。
    ctx に TenantContext が無い場合 **panic**（`*errors.Error{Code: "tenant_context_missing"}`
    を `panic()` で起こす）。tx 開始 → `SetLocalTenant` → fn 実行 → fn が error 戻りなら
    rollback、panic なら rollback + re-panic、正常終了で commit
  - `rls_test.go` / `txmanager_test.go` に panic ガード（TenantContext 不在で panic）・fn が
    error を返した時の rollback・fn の panic 時の rollback + re-panic のユニットテスト
    （pgx.Tx は mock interface でカバー）
  - _Requirements: 4.2, 4.3, 4.4, 4.5, NFR 1.1_
  - _Boundary: TxManager, RLSHelper, TenantContext_
  - _Depends: 2.1_

- [x] 3. マイグレーション（DDL + RLS + audit_logs append-only）+ sqlc 配置確保
- [x] 3.1 全 12 テーブルの up/down マイグレーション + sqlc query 配置先確保
  - `backend/db/migrations/0001_create_tenants.{up,down}.sql` 〜
    `0010_create_notification_dedupe_and_unassigned.{up,down}.sql` までの 10 ペア（20 ファイル）を
    umbrella design.md の Logical Data Model 通りに作成
  - 各 up は `CREATE TABLE IF NOT EXISTS`（PK / UNIQUE / FK / NOT NULL を含む）、各 down は
    対応する `DROP TABLE IF EXISTS` のみ（cascade を最小化）
  - `notification_dedupe`（`message_id text PK`, `notification_type`, `processed_at`）と
    `unassigned_notifications`（`id uuid PK`, `message_id`, `notification_type`,
    `enterprise_name`, `payload jsonb`, `received_at`）は tenant_id を持たない infra 用
  - **sqlc 配置先の確保**: 既存 `backend/sqlc.yaml`（#1 で配置済）が指す `db/queries/` ディレクトリ
    と `internal/platform/db/sqlcgen/` ディレクトリの `.gitkeep` を本 Issue のスコープで維持。
    sqlc.yaml 自体は本 Issue では編集しない（Req 4.6 の「型安全 SQL 生成可能にする設定ファイル
    を提供する」は #1 で既に物理化済みのため、本 Issue では「壊さない」「sqlc が参照するスキーマ
    が本マイグレーションで揃う」ことを保証）
  - _Requirements: 4.6, 6.1, 6.2, NFR 2.1, NFR 2.2_
  - _Depends: 2.2_
- [x] 3.2 RLS 有効化マイグレーション
  - `backend/db/migrations/0011_enable_rls.up.sql` に、tenant_id カラムを持つ table
    （`admin_users` / `admin_role_assignments` / `enrollment_tokens` / `policies` /
    `devices` / `device_commands` / `tenant_apps`）に対して `ENABLE ROW LEVEL SECURITY` +
    `tenant_isolation_<table>` ポリシー（USING / WITH CHECK 双方: `tenant_id =
    current_setting('app.tenant_id', true)::uuid OR current_setting('app.is_superadmin', true)::boolean`）を定義。
    `tenants` 自体は SuperAdmin のみ全行可視に倒すポリシー
    （USING で `current_setting('app.is_superadmin', true)::boolean` を true 時のみ通す）
  - **`audit_logs` は本マイグレーションの対象外**（append-only 要件のため 0012 で SELECT/INSERT のみ個別定義する。タスク 3.3 参照）
  - **`sessions` も本マイグレーションで RLS を有効化する**（NFR 1.1 / 1.2 の `tenants` を除く全テーブル分離が二重防御対象のため）。`sessions` は `tenant_id` カラムを持たないが、`admin_user_id` を `admin_users.id` 経由で参照することで `admin_users.tenant_id` ベースの分離が可能。`tenant_isolation_sessions` ポリシー（USING / WITH CHECK 双方: `EXISTS (SELECT 1 FROM admin_users WHERE admin_users.id = sessions.admin_user_id AND admin_users.tenant_id = current_setting('app.tenant_id', true)::uuid) OR current_setting('app.is_superadmin', true)::boolean`）を定義する。**認証 lookup（token_hash でセッションを引く経路、TenantContext 確立前）は本ポリシー下で 0 行に倒れるため、後続 Issue の Session Manager で SuperAdmin / system context（`app.is_superadmin=true`）経由で lookup する前提**（design.md「sessions の認証 lookup 経路」散文と整合。本 Issue では lookup ヘルパは未実装）
  - **`notification_dedupe` / `unassigned_notifications`（tenant_id 無しの infra テーブル）** には SuperAdmin のみ可視の RLS ポリシー（USING で `current_setting('app.is_superadmin', true)::boolean`）を定義する（design.md「物理制約」節と整合）
  - `0011_enable_rls.down.sql` に対応する `DROP POLICY`（`tenant_isolation_sessions` 含む）+ `ALTER TABLE ... DISABLE ROW LEVEL
    SECURITY` を順番に発行
  - 既存 policy がある場合に冪等で適用できるよう、up は `DROP POLICY IF EXISTS ... ; CREATE
    POLICY ...` のイディオムを使う
  - _Requirements: 6.3, 6.4, NFR 1.1, NFR 1.2_
  - _Depends: 3.1_
- [x] 3.3 audit_logs append-only マイグレーション + ロール定義 SQL
  - `backend/db/migrations/0012_audit_log_immutability.up.sql` に `ALTER TABLE audit_logs ENABLE
    ROW LEVEL SECURITY` + `FORCE ROW LEVEL SECURITY` + SELECT 用ポリシー（tenant_isolation +
    SuperAdmin 横断）+ INSERT 用ポリシー（**`WITH CHECK (tenant_id = current_setting('app.tenant_id',
    true)::uuid OR current_setting('app.is_superadmin', true)::boolean)`**）を定義。INSERT ポリシーの
    意図は (a) **通常テナント文脈**（IsSuperAdmin=false / app.tenant_id=<uuid>）で `tenant_id` mismatch
    または NULL の挿入を WITH CHECK で物理的に拒否し、他テナント宛 / 無テナント監査ログの混入を
    防ぐ。(b) **SuperAdmin 文脈**（IsSuperAdmin=true）では cross-tenant 操作監査やシステム監査ログ
    （tenant_id NULL 含む）を挿入できるよう OR 句で素通りさせる（Req 7.3 と整合）。UPDATE / DELETE
    用ポリシーは **意図的に未定義**（0011 の汎用ポリシーも audit_logs には作らないため許可されない）
    とし、追加で `REVOKE UPDATE, DELETE ON audit_logs FROM app_user` を発行
  - `0012_*.down.sql` に対応する `GRANT` + `DROP POLICY` を発行
  - `backend/db/roles/0001_create_app_and_migration_roles.sql` に `migration_user`（DDL 用）と
    `app_user`（DML 用、`audit_logs` の UPDATE/DELETE は本マイグレーションで REVOKE 済）の
    ロール定義 SQL を追加。本ファイルは `golang-migrate` の管轄外（初期セットアップ手順として
    別系統で適用）であることを冒頭コメントに明記
  - _Requirements: 6.5, 7.1, 7.2, 7.3, 7.4_
  - _Depends: 3.2_
- [x] 3.4 Makefile target / runbook / `.env.example` の整備
  - リポジトリルートの `Makefile` に `migrate-up` / `migrate-down` / `db-init-roles` の 3 target
    を追加（`golang-migrate` CLI を `go run -modfile=...` または別途インストール手順を採る
    かは Developer に委ねるが、`MIGRATE_DATABASE_URL` → `DATABASE_URL` の fallback を実装する）
  - `.env.example` の `DATABASE_URL` を `app_user`、`MIGRATE_DATABASE_URL` を `migration_user` に
    変更（design.md 「Modified Files」節と整合）。冒頭コメントに「2 ロールは
    `backend/db/roles/0001_create_app_and_migration_roles.sql` で定義し、`make db-init-roles` で
    セットアップする」旨を追記
  - `docs/runbook/local-dev.md`（新規）に「(1) `cp .env.example .env` → (2) `docker compose up
    -d postgres` → (3) `make db-init-roles` → (4) `make migrate-up`」の順を明記
  - _Requirements: 6.5, NFR 2.1, NFR 2.2, NFR 3.1_
  - _Depends: 3.3_

- [x] 4. HTTP サブルータ + middleware chain
- [x] 4.1 chi router + 2 サブルータ mount + middleware chain
  - `backend/internal/platform/httpserver/middleware.go` に recover / request_id（uuid v4）/
    structured access log（method / path / status / duration / request_id / tenant_id を field
    化）を実装。panic を recover した場合 `errors.WriteHTTP` 経由で 500 を返し ERROR ログを
    出す
  - 同ファイルに `TenantContextMiddleware(log) func(http.Handler) http.Handler` を実装。本 Issue
    では auth middleware の実装本体が未定のため、**入力契約として `ctx` の特定 key（例
    `internal/auth.authClaimsCtxKey`）から `*authClaims` を読み**、TenantContext に変換する
    アダプタとして実装する。auth スタブは本 Issue では「authClaims を ctx に注入しない」default
    deny 状態とし、結果として TenantContext は確立されず後段の DB アクセスは panic ガード対象に
    なる。**`/api/...` 配下のハンドラ実装が空のため、本 Issue の動作は 401 / 403 / 404 のみで
    閉じる**
  - `backend/internal/platform/httpserver/admin_middleware.go` に `RequireSuperAdmin(log)
    func(http.Handler) http.Handler` を実装。TenantContext 未確立 → 401、IsSuperAdmin=false →
    403、IsSuperAdmin=true → next.ServeHTTP。本 Issue では default deny（実 RBAC は後続 Issue で
    置換）
  - `backend/internal/platform/httpserver/server.go` に `NewServer(cfg, log, pool) (*http.Server,
    Routers, error)` を実装。chi.NewRouter で root を構築 → recover / request_id / access log を
    Use → `/healthz` `/readyz` を chain の外側（router root 直下）に登録 → `r.Route("/api",
    func(r chi.Router) { r.Use(TenantContextMiddleware); /* apiRouter */ })` と
    `r.Route("/api/admin", func(r chi.Router) { r.Use(TenantContextMiddleware,
    RequireSuperAdmin); /* adminRouter */ })` の 2 サブルータを mount。`Routers{API, Admin}` を
    後続 Issue 用に返す
  - `middleware_test.go` / `admin_middleware_test.go` / `server_test.go` に以下のユニットテスト:
    (a) recover middleware が panic を 500 化、(b) request_id が応答ヘッダに乗る、
    (c) `/healthz` `/readyz` が認証なしで 200 を返す、(d) `/api/admin/*` に **TenantContext 未確立**
    （auth スタブが claims を ctx に注入しない default 状態）で到達すると `RequireSuperAdmin` が
    **401** を返す（同 middleware の TenantContext 不在 = 401 の契約 / 本タスク詳細項目の
    `admin_middleware.go` 仕様と整合）、(e) `/api/admin/*` で TenantContext を put したテスト用
    router で IsSuperAdmin=false なら **403** を返す（SuperAdmin ガード本来の 403 経路）、
    (f) `/api/admin/*` で TenantContext を put して IsSuperAdmin=true なら next handler 到達、
    (g) `/api/*` 配下で TenantContext 未確立時の 401 化
  - _Requirements: 5.1, 5.2, 5.3, 5.4, 5.5, 5.6_
  - _Boundary: HTTPServer, TenantContextMiddleware, AdminRouteGuard, MiddlewareChain_
  - _Depends: 1.2, 1.3, 2.2_

- [x] 5. cmd/api / cmd/worker / depspin のリプレース
- [x] 5.1 cmd/api を bootstrap に置換
  - `backend/cmd/api/main.go` の `net/http.ServeMux` 実装を撤去し、`config.Load()` →
    `logger.NewLogger(cfg)` → `db.NewPool(ctx, cfg)` → `httpserver.NewServer(cfg, log, pool)` →
    `srv.ListenAndServe()` → SIGTERM で graceful shutdown（5s）の bootstrap に置換。`/healthz`
    `/readyz` は `httpserver` 側で提供される
  - `-healthcheck` サブコマンド（distroless 対応 / #1 由来）は **維持**し、内部 HTTP GET 先を
    `http://127.0.0.1:8080/healthz` のままにする
  - 初期化失敗時は exit code 1 + 構造化 ERROR ログ（`config_invalid` / `service_unavailable`）を
    出すこと
  - _Requirements: NFR 3.1, NFR 3.2, NFR 4.1_
  - _Boundary: cmd-api_
  - _Depends: 4.1_
- [x] 5.2 cmd/worker を bootstrap に置換 + depspin 整理
  - `backend/cmd/worker/main.go` に `config.Load()` + `logger.NewLogger(cfg)` を導入。Pub/Sub
    subscriber 実装は後続 Issue（umbrella tasks 6.x）に委ねるが、本 Issue では config と logger
    だけは正しく初期化された状態にして、後続 Issue が `internal/platform/pubsub` を `import`
    するだけで完結できるようにする。`/healthz`（:8090）と `-healthcheck` サブコマンドは維持
  - `backend/internal/depspin/depspin.go` から本 Issue で実利用される blank import を削除:
    `chi` / `pgx` / `pgxpool` / `golang-migrate` / `zap` / `coreos-go-oidc`。残置するのは
    `cloud.google.com/go/pubsub` と `google.golang.org/api/androidmanagement/v1` の 2 件のみ。
    冒頭 godoc を「後続 Issue で全件削除して自然消滅させる」前提に更新
  - `go build ./...` と `go vet ./...` がエラーなく通ること
  - _Requirements: NFR 4.1, NFR 4.2_
  - _Boundary: cmd-worker, depspin_
  - _Depends: 5.1_

- [ ] 6. 結合テスト（実 PostgreSQL）
- [ ] 6.1 RLS テナント分離 + panic ガード + audit_logs append-only の結合テスト
  - `backend/test/integration/db_tenant_isolation_test.go` を新規追加。`docker compose up -d
    postgres` 前提（CI / ローカルで `DATABASE_URL` から接続可能、無ければ test を skip）
  - テストシナリオ:
    (a) `make migrate-up` 相当を Go テスト側で適用、テナント A / B の 2 行を `tenants` に挿入、
    各テナント配下の `admin_users` / `sessions`（`admin_user_id` 経由）/ `devices` / `policies` /
    `audit_logs` にダミーレコードを挿入
    (b) TenantContext=A で `BeginTxFunc` を呼び `SELECT FROM devices` → A の行のみ、B の行は
    0 件であること（Req 6.4, NFR 1.2）
    (c) TenantContext=A で B の `devices` 行を `UPDATE` / `DELETE` → 0 rows affected
    (d) IsSuperAdmin=true で `SELECT FROM devices` → 全 tenant の行が返ること（Req 6.3）
    (e) TenantContext を put しない ctx で `BeginTxFunc` を呼ぶと panic（recover で
    `tenant_context_missing` を含む構造化エラーを確認 / Req 4.5）
    (f) `app_user` で `audit_logs` を UPDATE / DELETE → `permission denied` または `policy
    violation` のエラーが返ること（Req 7.1, 7.2, 7.4）
    (g) **`audit_logs` INSERT の WITH CHECK 二重防御**: (g-1) TenantContext=A で `tenant_id=B`
    の audit_logs を INSERT → policy violation（通常テナント文脈での cross-tenant 挿入拒否 /
    Req 7.4）、(g-2) TenantContext=A で `tenant_id=NULL` の audit_logs を INSERT → policy
    violation、(g-3) IsSuperAdmin=true で `tenant_id=NULL` および `tenant_id=B` の INSERT →
    成功（cross-tenant 監査 / システム監査ログ用途、Req 7.3 と整合）
    (h) **sessions のテナント分離**: TenantContext=A で B 配下の `admin_user_id` を持つ
    sessions 行に対する `SELECT` / `UPDATE` / `DELETE` → 0 rows（`tenant_isolation_sessions`
    ポリシーが subselect 経由で分離 / Req 6.4, NFR 1.2）、IsSuperAdmin=true で全 sessions が
    返ること
  - `backend/test/integration/http_subrouter_mount_test.go` を新規追加。
    `httpserver.NewServer` で構築した *http.Server を `httptest.NewServer` 相当で起動し、
    (g) `GET /healthz` が 200 を返す、(h) `GET /api/anything` が 401（auth スタブ default deny）、
    (i) `GET /api/admin/anything` が 401 または 403（SuperAdmin ガード default deny）、
    (j) test 用 router で TenantContext を put して `IsSuperAdmin=true` の場合に next handler
    まで到達することを確認
  - _Requirements: 4.5, 5.5, 6.3, 6.4, 7.1, 7.2, 7.3, 7.4, NFR 1.1, NFR 1.2_
  - _Boundary: TxManager, RLSHelper, HTTPServer, AdminRouteGuard, Migrations_
  - _Depends: 3.4, 4.1, 5.2_
- [ ] 6.2 マイグレーション可逆性テスト
  - `backend/test/integration/migrations_reversible_test.go` を新規追加。`make migrate-up` 相当 →
    `make migrate-down` 相当を Go テスト側で実行し、(a) down 後に主要テーブルが消失する、
    (b) 再度 up すると同じ最終状態に到達する（NFR 2.1）、(c) 2 回目の up が冪等に no-op で
    終わること（NFR 2.1）を確認
  - _Requirements: NFR 2.1, NFR 2.2_
  - _Boundary: Migrations_
  - _Depends: 6.1_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを構造化ブロックで宣言する。
integration test（`backend/test/integration/...`）は Req 6 / 7 / NFR 1 / NFR 2 の受入基準を担う
唯一の検証経路のため、**verify 対象に含める**。CI / ローカルで `DATABASE_URL` 接続が確立できない
場合は各 integration test が自身で `t.Skip` する設計（`tasks.md` 6.1 / 6.2 詳細参照）であり、
DB 不在の環境でも verify は false-fail しない。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go vet ./... && go test ./...
```
