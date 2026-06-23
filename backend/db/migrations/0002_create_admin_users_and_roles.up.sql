-- 0002_create_admin_users_and_roles.up.sql
-- Req 6.1, 6.2
-- admin_users（oidc_subject ↔ 内部 admin_user_id のマッピング）と
-- admin_role_assignments（複合 PK、1 管理者複数ロール可）を作成する。
-- SuperAdmin の場合 tenant_id は NULL（cross-tenant 操作のため）。

DO $$ BEGIN
    CREATE TYPE admin_role AS ENUM ('SuperAdmin', 'TenantAdmin', 'Operator', 'Viewer');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS admin_users (
    id            uuid PRIMARY KEY,
    oidc_subject  text NOT NULL UNIQUE,
    email         text NOT NULL,
    tenant_id     uuid REFERENCES tenants(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_admin_users_tenant_id ON admin_users(tenant_id);

CREATE TABLE IF NOT EXISTS admin_role_assignments (
    admin_user_id uuid NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    role          admin_role NOT NULL,
    tenant_id     uuid REFERENCES tenants(id),
    PRIMARY KEY (admin_user_id, role, tenant_id)
);

CREATE INDEX IF NOT EXISTS idx_admin_role_assignments_tenant_id ON admin_role_assignments(tenant_id);
