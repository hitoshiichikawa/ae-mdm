-- 0012_audit_log_immutability.up.sql
-- Req 7.1, 7.2, 7.3, 7.4
-- audit_logs を DB レベルで append-only 化する。
--   * ENABLE + FORCE ROW LEVEL SECURITY: テーブル所有者であっても RLS を回避できない構成
--   * SELECT ポリシー: tenant_isolation + SuperAdmin 横断（Req 7.3）
--   * INSERT ポリシー WITH CHECK 二重防御:
--     - 通常テナント文脈（IsSuperAdmin=false, app.tenant_id=<uuid>）: tenant_id mismatch /
--       NULL の挿入を WITH CHECK で物理的に拒否し、他テナント宛 / 無テナント監査ログの
--       混入を防ぐ
--     - SuperAdmin 文脈（IsSuperAdmin=true）: cross-tenant 操作監査やシステム監査ログ
--       （tenant_id NULL 含む）を挿入できるよう OR 句で素通りさせる（Req 7.3 と整合）
--   * UPDATE / DELETE ポリシー: 意図的に未定義（0011 の汎用 FOR ALL ポリシーも audit_logs
--     には作っていないため、policy 未定義 = 許可されない）
--   * 追加で REVOKE UPDATE, DELETE ON audit_logs FROM app_user の二重防御を発行
--     （app_user ロールは backend/db/roles/0001_create_app_and_migration_roles.sql で定義）

ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;

-- SELECT: 自テナント分または SuperAdmin
DROP POLICY IF EXISTS audit_logs_select ON audit_logs;
CREATE POLICY audit_logs_select ON audit_logs FOR SELECT
    USING (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

-- INSERT: WITH CHECK で tenant_id 整合性 / SuperAdmin OR 句を二重防御
DROP POLICY IF EXISTS audit_logs_insert ON audit_logs;
CREATE POLICY audit_logs_insert ON audit_logs FOR INSERT WITH CHECK (
    tenant_id = current_setting('app.tenant_id', true)::uuid
    OR current_setting('app.is_superadmin', true)::boolean
);

-- UPDATE / DELETE は policy を作らない（policy 未定義 = 許可されない）。
-- さらに REVOKE で app_user からも物理的に剥奪する。
-- app_user ロールが未作成の環境では REVOKE はエラーを返すため、IF EXISTS 相当のガードを置く。
DO $$ BEGIN
    EXECUTE 'REVOKE UPDATE, DELETE ON audit_logs FROM app_user';
EXCEPTION
    WHEN undefined_object THEN
        RAISE NOTICE 'app_user role does not exist; skipping REVOKE (run db-init-roles first)';
END $$;
