-- 0011_enable_rls.down.sql
-- 0011_enable_rls.up.sql で定義した全ポリシーを drop し、各テーブルの RLS を無効化する。
-- audit_logs は本マイグレーションの対象外（0012 で個別管理）。

-- notification 系 infra テーブル
DROP POLICY IF EXISTS superadmin_only_unassigned_notifications ON unassigned_notifications;
ALTER TABLE unassigned_notifications DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS superadmin_only_notification_dedupe ON notification_dedupe;
ALTER TABLE notification_dedupe DISABLE ROW LEVEL SECURITY;

-- sessions
DROP POLICY IF EXISTS tenant_isolation_sessions ON sessions;
ALTER TABLE sessions DISABLE ROW LEVEL SECURITY;

-- tenant_id 列を持つテーブル群
DROP POLICY IF EXISTS tenant_isolation_tenant_apps ON tenant_apps;
ALTER TABLE tenant_apps DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_device_commands ON device_commands;
ALTER TABLE device_commands DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_devices ON devices;
ALTER TABLE devices DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policies ON policies;
ALTER TABLE policies DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_enrollment_tokens ON enrollment_tokens;
ALTER TABLE enrollment_tokens DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_admin_role_assignments ON admin_role_assignments;
ALTER TABLE admin_role_assignments DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_admin_users ON admin_users;
ALTER TABLE admin_users DISABLE ROW LEVEL SECURITY;

-- tenants
DROP POLICY IF EXISTS tenant_isolation_tenants ON tenants;
ALTER TABLE tenants DISABLE ROW LEVEL SECURITY;
