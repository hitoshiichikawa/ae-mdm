-- 0001_create_app_and_migration_roles.sql
-- Req 6.5（DDL 用ロールと app 用ロールの分離）
--
-- 注意: 本ファイルは `golang-migrate` の管轄外（schema_migrations テーブルに記録されない）。
-- ホストから `make db-init-roles` で別系統に適用する初期セットアップ SQL です。
-- 適用順序: (1) `docker compose up -d postgres` → (2) `make db-init-roles` →
--           (3) `make migrate-up`。詳細は docs/runbook/local-dev.md を参照。
--
-- 定義する 2 ロール:
--   * migration_user: DDL 専用ロール。golang-migrate が `MIGRATE_DATABASE_URL` で接続する。
--     CREATE/ALTER/DROP TABLE 等の DDL 権限を持つ。
--   * app_user:       DML 専用ロール。api / worker が `DATABASE_URL` で接続する。
--     全テーブルへの SELECT/INSERT/UPDATE/DELETE 権限を持つが、audit_logs に対しては
--     0012_audit_log_immutability.up.sql で UPDATE/DELETE が REVOKE される（二重防御）。
--     DDL 権限は持たないため accidental schema drift を防ぐ。
--
-- パスワード:
--   `<REPLACE_ME_MIGRATION_PASSWORD>` / `<REPLACE_ME_APP_PASSWORD>` を環境ごとに置換する。
--   実運用では env 経由で psql -v マクロ展開や envsubst を用いて適用することを推奨。
--
-- 冪等性:
--   * CREATE ROLE は DO $$ ... duplicate_object EXCEPTION ... END $$ で冪等化
--   * GRANT ... ON ALL TABLES IN SCHEMA public ... は適用時点で存在する全テーブルに付与し、
--     ALTER DEFAULT PRIVILEGES で将来作成されるテーブルにも自動付与する

-- ====================================================================
-- migration_user（DDL 用）
-- ====================================================================
DO $$ BEGIN
    CREATE ROLE migration_user LOGIN PASSWORD '<REPLACE_ME_MIGRATION_PASSWORD>';
EXCEPTION
    WHEN duplicate_object THEN
        RAISE NOTICE 'role migration_user already exists; skipping CREATE';
END $$;

GRANT ALL PRIVILEGES ON DATABASE current_database() TO migration_user;
GRANT ALL ON SCHEMA public TO migration_user;
GRANT ALL ON ALL TABLES IN SCHEMA public TO migration_user;
GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO migration_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO migration_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO migration_user;

-- ====================================================================
-- app_user（DML 用、RLS バインド対象）
-- ====================================================================
DO $$ BEGIN
    CREATE ROLE app_user LOGIN PASSWORD '<REPLACE_ME_APP_PASSWORD>';
EXCEPTION
    WHEN duplicate_object THEN
        RAISE NOTICE 'role app_user already exists; skipping CREATE';
END $$;

GRANT CONNECT ON DATABASE current_database() TO app_user;
GRANT USAGE ON SCHEMA public TO app_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_user;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_user;

-- 注意:
--   * audit_logs の UPDATE/DELETE は 0012_audit_log_immutability.up.sql で REVOKE される
--     （本ファイルでは GRANT し、0012 で REVOKE することで二重防御を成立させる）
--   * ALTER DEFAULT PRIVILEGES は本 SQL を実行したロール（通常 postgres superuser）が
--     以後 public schema に作成するテーブルにのみ適用される。migration_user が CREATE TABLE
--     する場合に app_user に自動 GRANT させたい場合は、migration_user としても ALTER DEFAULT
--     PRIVILEGES を実行する必要があるが、本 MVP では `make db-init-roles` 後に
--     `make migrate-up`（migration_user 接続）→ 後続 0011/0012 が REVOKE / RLS で物理制御
--     する設計のため、追加付与は不要
