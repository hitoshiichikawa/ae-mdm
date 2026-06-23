-- 0002_create_admin_users_and_roles.up.sql
-- Req 6.1, 6.2
-- admin_users（oidc_subject ↔ 内部 admin_user_id のマッピング）と
-- admin_role_assignments を作成する。
-- SuperAdmin の場合 tenant_id は NULL（cross-tenant 操作のため）。
--
-- 注意（PR #31 round-1 review 由来）:
--   admin_role_assignments の重複防止は PRIMARY KEY (admin_user_id, role, tenant_id) に
--   含めず、UNIQUE NULLS NOT DISTINCT 制約で表現する。PostgreSQL では PK 列は暗黙に
--   NOT NULL になるため、tenant_id を PK に含めると SuperAdmin role assignment を
--   tenant_id=NULL で保存できなくなる。PG 15+ で導入された `NULLS NOT DISTINCT` 句で
--   NULL も等値扱いとし、同一 (admin_user_id, role, NULL) の重複も阻止する。
--   compose の postgres は 16-alpine のため対応。

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
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_user_id uuid NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    role          admin_role NOT NULL,
    tenant_id     uuid REFERENCES tenants(id),
    UNIQUE NULLS NOT DISTINCT (admin_user_id, role, tenant_id)
);

CREATE INDEX IF NOT EXISTS idx_admin_role_assignments_admin_user_id
    ON admin_role_assignments(admin_user_id);
CREATE INDEX IF NOT EXISTS idx_admin_role_assignments_tenant_id
    ON admin_role_assignments(tenant_id);
