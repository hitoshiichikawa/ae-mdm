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
--   * 接続中 DB への GRANT ... ON DATABASE は EXECUTE format(...) で動的構築する
--     （PostgreSQL の GRANT ON DATABASE は識別子に関数呼び出しを許可しないため、
--      `current_database()` を直接埋めず DO ブロック内で format() 経由で名前展開する）

-- ====================================================================
-- migration_user（DDL 用）
-- ====================================================================
DO $$ BEGIN
    CREATE ROLE migration_user LOGIN PASSWORD '<REPLACE_ME_MIGRATION_PASSWORD>';
EXCEPTION
    WHEN duplicate_object THEN
        RAISE NOTICE 'role migration_user already exists; skipping CREATE';
END $$;

DO $$
BEGIN
    EXECUTE format('GRANT ALL PRIVILEGES ON DATABASE %I TO migration_user', current_database());
END $$;

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

DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO app_user', current_database());
END $$;

GRANT USAGE ON SCHEMA public TO app_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_user;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_user;

-- 本ファイルを実行したロール（通常 postgres superuser）が今後 public schema に作成する
-- テーブルに対する app_user への自動 GRANT。
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_user;

-- migration_user が今後 public schema に作成するテーブル・シーケンスに対する
-- app_user への自動 GRANT。本ファイルを実行したロールではなく migration_user が
-- 作成するオブジェクトを対象とするため、`FOR ROLE migration_user` を明示する必要がある
-- （これが無いと `make migrate-up` 適用後の新規テーブルへ app_user の DML 権限が
--  自動付与されず、アプリ接続が migrated table を読み書きできない）。
ALTER DEFAULT PRIVILEGES FOR ROLE migration_user IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_user;
ALTER DEFAULT PRIVILEGES FOR ROLE migration_user IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO app_user;

-- 注意:
--   * audit_logs の UPDATE/DELETE は 0012_audit_log_immutability.up.sql でも REVOKE されるが、
--     `db-init-roles` が `migrate-up` の **後** に実行された場合、上の `GRANT ... ON ALL TABLES`
--     と `ALTER DEFAULT PRIVILEGES` が audit_logs の UPDATE/DELETE を再付与してしまう。
--     append-only の最終 DB 状態を保証するため、本ファイル末尾でも明示的に REVOKE する
--     （0012 と二重防御 / PR #31 round-3 review 由来）。
--   * audit_logs テーブル自体が未作成（migrate-up 前）の場合、REVOKE は undefined_table で
--     失敗するため、DO ブロックで catch して NOTICE に降格する（冪等性確保）。

DO $$ BEGIN
    EXECUTE 'REVOKE UPDATE, DELETE ON audit_logs FROM app_user';
EXCEPTION
    WHEN undefined_table THEN
        RAISE NOTICE 'audit_logs table does not exist; skipping REVOKE (will be enforced by migration 0012 when applied)';
END $$;
