-- 0011_enable_rls.up.sql
-- Req 6.3, 6.4 / NFR 1.1, NFR 1.2
-- tenant_id を持つ全テーブル + sessions (subselect) + tenants (SuperAdmin only) +
-- notification 系 infra テーブル (SuperAdmin only) に RLS を有効化する。
--
-- audit_logs は本マイグレーションの対象外（append-only 要件のため 0012 で SELECT/INSERT のみ
-- 個別定義する）。
--
-- 冪等性のため、各ポリシーは DROP POLICY IF EXISTS ... ; CREATE POLICY ... のイディオムで適用。
-- USING / WITH CHECK の両方を定義することで、SELECT / UPDATE / DELETE / INSERT すべてに
-- テナント分離が適用される（FOR ALL）。
--
-- 注意: tenant_isolation_* ポリシーが参照する GUC 名（app.tenant_id / app.is_superadmin）は、
-- backend/internal/platform/db/rls.go の SetLocalTenant が set_config で発行する名称と
-- 完全一致させること（typo は cross-tenant leak の直接原因になる）。

-- ====================================================================
-- tenants (SuperAdmin のみ全行可視)
-- ====================================================================
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_tenants ON tenants;
CREATE POLICY tenant_isolation_tenants ON tenants
    USING (
        id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

-- ====================================================================
-- tenant_id 列を持つテーブル群（汎用 tenant_isolation_<table> ポリシー）
-- ====================================================================
ALTER TABLE admin_users ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_admin_users ON admin_users;
CREATE POLICY tenant_isolation_admin_users ON admin_users
    USING (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

ALTER TABLE admin_role_assignments ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_admin_role_assignments ON admin_role_assignments;
CREATE POLICY tenant_isolation_admin_role_assignments ON admin_role_assignments
    USING (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

ALTER TABLE enrollment_tokens ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_enrollment_tokens ON enrollment_tokens;
CREATE POLICY tenant_isolation_enrollment_tokens ON enrollment_tokens
    USING (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

ALTER TABLE policies ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policies ON policies;
CREATE POLICY tenant_isolation_policies ON policies
    USING (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

ALTER TABLE devices ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_devices ON devices;
CREATE POLICY tenant_isolation_devices ON devices
    USING (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

ALTER TABLE device_commands ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_device_commands ON device_commands;
CREATE POLICY tenant_isolation_device_commands ON device_commands
    USING (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

ALTER TABLE tenant_apps ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_tenant_apps ON tenant_apps;
CREATE POLICY tenant_isolation_tenant_apps ON tenant_apps
    USING (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        tenant_id = current_setting('app.tenant_id', true)::uuid
        OR current_setting('app.is_superadmin', true)::boolean
    );

-- ====================================================================
-- sessions（tenant_id 列を持たないため admin_users.tenant_id 経由で subselect 分離）
-- 注意: 認証 lookup（token_hash でセッションを引く経路、TenantContext 確立前）は本ポリシー
-- 下で 0 行に倒れるため、後続 Issue の Session Manager で SuperAdmin / system context
-- （app.is_superadmin=true）経由で lookup する前提（design.md「sessions の認証 lookup 経路」
-- 散文と整合）。
-- ====================================================================
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_sessions ON sessions;
CREATE POLICY tenant_isolation_sessions ON sessions
    USING (
        EXISTS (
            SELECT 1 FROM admin_users
            WHERE admin_users.id = sessions.admin_user_id
              AND admin_users.tenant_id = current_setting('app.tenant_id', true)::uuid
        )
        OR current_setting('app.is_superadmin', true)::boolean
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM admin_users
            WHERE admin_users.id = sessions.admin_user_id
              AND admin_users.tenant_id = current_setting('app.tenant_id', true)::uuid
        )
        OR current_setting('app.is_superadmin', true)::boolean
    );

-- ====================================================================
-- notification_dedupe / unassigned_notifications（tenant_id 無しの infra テーブル）
-- SuperAdmin のみ可視。通常テナント文脈では空集合になる（default deny）。
-- ====================================================================
ALTER TABLE notification_dedupe ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS superadmin_only_notification_dedupe ON notification_dedupe;
CREATE POLICY superadmin_only_notification_dedupe ON notification_dedupe
    USING (current_setting('app.is_superadmin', true)::boolean)
    WITH CHECK (current_setting('app.is_superadmin', true)::boolean);

ALTER TABLE unassigned_notifications ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS superadmin_only_unassigned_notifications ON unassigned_notifications;
CREATE POLICY superadmin_only_unassigned_notifications ON unassigned_notifications
    USING (current_setting('app.is_superadmin', true)::boolean)
    WITH CHECK (current_setting('app.is_superadmin', true)::boolean);
